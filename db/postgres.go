// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

//go:embed schema_pg.sql
var postgresSchema string

// postgresMigrations are immutable, ordered upgrades applied after the
// original consolidated v0.4 schema. Keeping postgresSchema unchanged
// preserves its recorded checksum for existing deployments while allowing
// normal forward upgrades instead of requiring manual schema surgery.
var postgresMigrations = []migration{
	{
		Version: "postgres_0001_scope_concept_states_by_domain",
		Body: `ALTER TABLE concept_states
    ADD COLUMN IF NOT EXISTS domain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE concept_states
    DROP CONSTRAINT IF EXISTS concept_states_learner_id_concept_key;
INSERT INTO concept_states
    (learner_id, domain_id, concept, stability, difficulty, elapsed_days,
     scheduled_days, reps, lapses, card_state, last_review, next_review,
     p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
SELECT cs.learner_id, d.id, cs.concept, cs.stability, cs.difficulty,
       cs.elapsed_days, cs.scheduled_days, cs.reps, cs.lapses, cs.card_state,
       cs.last_review, cs.next_review, cs.p_mastery, cs.p_learn, cs.p_forget,
       cs.p_slip, cs.p_guess, cs.theta, cs.updated_at
FROM concept_states cs
JOIN domains d
  ON d.learner_id = cs.learner_id
 AND (d.graph_json::jsonb -> 'concepts') ? cs.concept
WHERE cs.domain_id = '';
ALTER TABLE concept_states
    ADD CONSTRAINT concept_states_learner_domain_concept_key
    UNIQUE (learner_id, domain_id, concept);
DROP INDEX IF EXISTS idx_concept_states_learner;
CREATE INDEX idx_concept_states_learner
    ON concept_states(learner_id, domain_id);
DROP INDEX IF EXISTS idx_concept_states_review;
CREATE INDEX idx_concept_states_review
    ON concept_states(learner_id, domain_id, next_review)`,
	},
	{
		Version: "postgres_0002_scope_metacognitive_records_by_domain",
		Body: `ALTER TABLE calibration_records
    ADD COLUMN IF NOT EXISTS domain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE transfer_records
    ADD COLUMN IF NOT EXISTS domain_id TEXT NOT NULL DEFAULT '';
DROP INDEX IF EXISTS idx_transfer_records_learner_concept;
CREATE INDEX idx_transfer_records_learner_concept
    ON transfer_records(learner_id, domain_id, concept_id, created_at);
DROP INDEX IF EXISTS idx_calibration_records_learner;
CREATE INDEX idx_calibration_records_learner
    ON calibration_records(learner_id, domain_id, created_at)`,
	},
	{
		Version: "postgres_0003_backfill_unique_domain_identity",
		Body: `WITH inferred AS (
    SELECT i.id, MIN(d.id) AS domain_id
    FROM interactions i
    JOIN domains d
      ON d.learner_id = i.learner_id
     AND (d.graph_json::jsonb -> 'concepts') ? i.concept
    WHERE COALESCE(i.domain_id, '') = ''
    GROUP BY i.id
    HAVING COUNT(DISTINCT d.id) = 1
)
UPDATE interactions i
SET domain_id = inferred.domain_id
FROM inferred
WHERE i.id = inferred.id;
WITH inferred AS (
    SELECT c.prediction_id, MIN(d.id) AS domain_id
    FROM calibration_records c
    JOIN domains d
      ON d.learner_id = c.learner_id
     AND (d.graph_json::jsonb -> 'concepts') ? c.concept_id
    WHERE c.domain_id = ''
    GROUP BY c.prediction_id
    HAVING COUNT(DISTINCT d.id) = 1
)
UPDATE calibration_records c
SET domain_id = inferred.domain_id
FROM inferred
WHERE c.prediction_id = inferred.prediction_id;
WITH inferred AS (
    SELECT t.id, MIN(d.id) AS domain_id
    FROM transfer_records t
    JOIN domains d
      ON d.learner_id = t.learner_id
     AND (d.graph_json::jsonb -> 'concepts') ? t.concept_id
    WHERE t.domain_id = ''
    GROUP BY t.id
    HAVING COUNT(DISTINCT d.id) = 1
)
UPDATE transfer_records t
SET domain_id = inferred.domain_id
FROM inferred
WHERE t.id = inferred.id`,
	},
	{
		Version: "postgres_0004_webhook_queue_processing_claim",
		Body: `ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ`,
	},
	{
		Version: "postgres_0005_refresh_token_families",
		Body: `ALTER TABLE refresh_tokens
    ADD COLUMN IF NOT EXISTS family_id TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens
    ADD COLUMN IF NOT EXISTS used_at TIMESTAMPTZ;
ALTER TABLE refresh_tokens
    ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
UPDATE refresh_tokens SET family_id = token WHERE family_id = '';
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family ON refresh_tokens(family_id)`,
	},
	{
		Version: "postgres_0006_bind_oauth_codes_redirect_pkce",
		Body: `ALTER TABLE oauth_codes
    ADD COLUMN IF NOT EXISTS code_challenge_method TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_codes
    ADD COLUMN IF NOT EXISTS redirect_uri TEXT NOT NULL DEFAULT ''`,
	},
	{
		Version: "postgres_0007_index_security_state_ttl",
		Body: `CREATE INDEX IF NOT EXISTS idx_rate_limit_buckets_updated_at
    ON rate_limit_buckets(updated_at);
CREATE INDEX IF NOT EXISTS idx_login_failures_attempted_at
    ON login_failures(attempted_at)`,
	},
	{
		Version: "postgres_0008_create_learning_sessions",
		Body: `CREATE TABLE IF NOT EXISTS learning_sessions (
    id             TEXT PRIMARY KEY,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    domain_id      TEXT REFERENCES domains(id) ON DELETE SET NULL,
    status         TEXT NOT NULL CHECK (status IN ('open','closed')) DEFAULT 'open',
    started_at     TIMESTAMPTZ NOT NULL,
    last_active_at TIMESTAMPTZ NOT NULL,
    closed_at      TIMESTAMPTZ,
    CHECK ((status = 'open' AND closed_at IS NULL) OR
           (status = 'closed' AND closed_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_learning_sessions_one_open
    ON learning_sessions(learner_id) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_learning_sessions_learner_started
    ON learning_sessions(learner_id, started_at DESC)`,
	},
	{
		Version: "postgres_0009_link_interactions_to_sessions",
		Body: `ALTER TABLE interactions
    ADD COLUMN IF NOT EXISTS session_id TEXT REFERENCES learning_sessions(id);
CREATE INDEX IF NOT EXISTS idx_interactions_learner_session
    ON interactions(learner_id, session_id, created_at)`,
	},
	{
		Version: "postgres_0010_intention_lifecycle_and_session",
		Body: `ALTER TABLE implementation_intentions
    ADD COLUMN IF NOT EXISTS session_id TEXT REFERENCES learning_sessions(id);
ALTER TABLE implementation_intentions
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','honored','missed','cancelled'));
ALTER TABLE implementation_intentions
    ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ;
ALTER TABLE implementation_intentions
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE implementation_intentions
SET status = CASE
        WHEN honored = 1 THEN 'honored'
        WHEN honored = 0 THEN 'missed'
        ELSE 'pending'
    END,
    resolved_at = CASE WHEN honored IS NOT NULL THEN created_at ELSE NULL END,
    updated_at = created_at
WHERE updated_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_impl_intent_learner_status
    ON implementation_intentions(learner_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_impl_intent_session
    ON implementation_intentions(session_id)`,
	},
	{
		Version: "postgres_0011_create_assessment_attempts",
		Body: `CREATE TABLE IF NOT EXISTS assessment_attempts (
    id                         TEXT PRIMARY KEY,
    learner_id                 TEXT NOT NULL REFERENCES learners(id),
    domain_id                  TEXT NOT NULL REFERENCES domains(id),
    concept_id                 TEXT NOT NULL,
    session_id                 TEXT REFERENCES learning_sessions(id),
    activity_id                TEXT NOT NULL,
    activity_version           INTEGER NOT NULL CHECK (activity_version >= 1),
    activity_type              TEXT NOT NULL,
    observable                 TEXT NOT NULL,
    task_text                  TEXT,
    task_content_hash          TEXT NOT NULL,
    response_text              TEXT,
    response_content_hash      TEXT NOT NULL DEFAULT '',
    rubric_json                TEXT NOT NULL,
    passing_score              DOUBLE PRECISION NOT NULL CHECK (passing_score > 0),
    status                     TEXT NOT NULL CHECK (status IN ('prepared','submitted','evaluated','cancelled')),
    rubric_score_json          TEXT,
    score                      DOUBLE PRECISION NOT NULL DEFAULT 0,
    passed                     SMALLINT NOT NULL DEFAULT 0 CHECK (passed IN (0,1)),
    evaluator_id               TEXT,
    evaluation_method          TEXT CHECK (evaluation_method IS NULL OR evaluation_method IN ('host_llm','external_service','human_review','deterministic')),
    evaluation_provenance_json TEXT,
    trusted_evaluation         SMALLINT NOT NULL DEFAULT 0 CHECK (trusted_evaluation IN (0,1)),
    created_at                 TIMESTAMPTZ NOT NULL,
    submitted_at               TIMESTAMPTZ,
    evaluated_at               TIMESTAMPTZ,
    cancelled_at               TIMESTAMPTZ,
    CHECK (task_text IS NOT NULL OR task_content_hash <> ''),
    CHECK (status <> 'submitted' OR submitted_at IS NOT NULL),
    CHECK (status <> 'evaluated' OR (submitted_at IS NOT NULL AND evaluated_at IS NOT NULL)),
    CHECK (status <> 'cancelled' OR cancelled_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_assessment_attempts_learning_evidence
    ON assessment_attempts(learner_id, domain_id, concept_id, trusted_evaluation, passed, evaluated_at DESC);
CREATE INDEX IF NOT EXISTS idx_assessment_attempts_session
    ON assessment_attempts(learner_id, session_id, created_at);
ALTER TABLE interactions
    ADD COLUMN IF NOT EXISTS assessment_attempt_id TEXT REFERENCES assessment_attempts(id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_interactions_assessment_attempt
    ON interactions(assessment_attempt_id) WHERE assessment_attempt_id IS NOT NULL;
ALTER TABLE transfer_records
    ADD COLUMN IF NOT EXISTS assessment_attempt_id TEXT REFERENCES assessment_attempts(id);
CREATE INDEX IF NOT EXISTS idx_transfer_records_assessment_attempt
    ON transfer_records(assessment_attempt_id)`,
	},
	{
		Version: "postgres_0012_one_intention_per_session",
		Body: `CREATE UNIQUE INDEX IF NOT EXISTS idx_impl_intent_one_per_session
    ON implementation_intentions(session_id)
    WHERE session_id IS NOT NULL`,
	},
	{
		Version: "postgres_0013_availability_accessibility_high_stakes",
		Body: `ALTER TABLE availability ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'UTC';
ALTER TABLE availability ADD COLUMN IF NOT EXISTS notification_consent SMALLINT NOT NULL DEFAULT 0 CHECK (notification_consent IN (0,1));
ALTER TABLE availability ADD COLUMN IF NOT EXISTS notification_frequency TEXT NOT NULL DEFAULT 'daily' CHECK (notification_frequency IN ('as_scheduled','daily','weekly'));
ALTER TABLE availability ADD COLUMN IF NOT EXISTS max_notifications_per_day INTEGER NOT NULL DEFAULT 1 CHECK (max_notifications_per_day BETWEEN 1 AND 10);
ALTER TABLE availability ADD COLUMN IF NOT EXISTS accessibility_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE availability ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1);
ALTER TABLE availability ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
ALTER TABLE domains ADD COLUMN IF NOT EXISTS high_stakes SMALLINT NOT NULL DEFAULT 0 CHECK (high_stakes IN (0,1));
CREATE INDEX IF NOT EXISTS idx_domains_learner_high_stakes ON domains(learner_id, high_stakes);
CREATE INDEX IF NOT EXISTS idx_assessment_attempts_human_review ON assessment_attempts(learner_id, domain_id, evaluation_method, trusted_evaluation, status)`,
	},
	{
		Version: "postgres_0014_tombstone_domains",
		Body: `ALTER TABLE domains ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_domains_learner_deleted ON domains(learner_id, deleted_at)`,
	},
	{
		Version: "postgres_0015_create_immutable_curriculum",
		Body: `CREATE TABLE curriculum_versions (
    domain_id       TEXT NOT NULL,
    learner_id      TEXT NOT NULL,
    version         INTEGER NOT NULL CHECK (version >= 1),
    parent_version  INTEGER,
    snapshot_json   JSONB NOT NULL,
    operation_type  TEXT NOT NULL CHECK (operation_type IN ('create','baseline_import','add','rename','update_metadata','split','merge','remove','legacy_graph_update')),
    operation_json  JSONB NOT NULL,
    provenance_json JSONB NOT NULL,
    review_json     JSONB NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (domain_id, version),
    FOREIGN KEY (domain_id, parent_version)
        REFERENCES curriculum_versions(domain_id, version)
);
CREATE TABLE curriculum_concepts (
    id         TEXT PRIMARY KEY,
    domain_id  TEXT NOT NULL,
    learner_id TEXT NOT NULL,
    stable_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (domain_id, stable_key)
);
CREATE TABLE curriculum_metadata_ids (
    id         TEXT PRIMARY KEY,
    concept_id TEXT NOT NULL REFERENCES curriculum_concepts(id),
    domain_id  TEXT NOT NULL,
    learner_id TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('outcome','criterion')),
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_curriculum_versions_learner_domain
    ON curriculum_versions(learner_id, domain_id, version DESC);
CREATE INDEX idx_curriculum_concepts_learner_domain
    ON curriculum_concepts(learner_id, domain_id);
CREATE INDEX idx_curriculum_metadata_learner_domain
    ON curriculum_metadata_ids(learner_id, domain_id);
CREATE FUNCTION reject_curriculum_identity_mutation() RETURNS trigger
LANGUAGE plpgsql AS $curriculum_immutable$
BEGIN
    RAISE EXCEPTION 'curriculum history and identities are immutable';
    RETURN NULL;
END;
$curriculum_immutable$;
CREATE TRIGGER curriculum_versions_no_mutation
    BEFORE UPDATE OR DELETE ON curriculum_versions
    FOR EACH STATEMENT EXECUTE FUNCTION reject_curriculum_identity_mutation();
CREATE TRIGGER curriculum_concepts_no_mutation
    BEFORE UPDATE OR DELETE ON curriculum_concepts
    FOR EACH STATEMENT EXECUTE FUNCTION reject_curriculum_identity_mutation();
CREATE TRIGGER curriculum_metadata_ids_no_mutation
    BEFORE UPDATE OR DELETE ON curriculum_metadata_ids
    FOR EACH STATEMENT EXECUTE FUNCTION reject_curriculum_identity_mutation()`,
	},
	{
		Version: "postgres_0016_tool_call_idempotency",
		Body: `CREATE TABLE tool_call_idempotency (
    learner_id      TEXT NOT NULL REFERENCES learners(id),
    tool_name       TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('processing','completed')),
    response_text   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    completed_at    TIMESTAMPTZ,
    PRIMARY KEY (learner_id, tool_name, idempotency_key),
    CHECK ((status = 'processing' AND completed_at IS NULL) OR
           (status = 'completed' AND completed_at IS NOT NULL))
);
CREATE INDEX idx_tool_call_idempotency_updated
    ON tool_call_idempotency(updated_at)`,
	},
	{
		Version: "postgres_0017_webhook_retry_state",
		Body: `ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0);
ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS max_attempts INTEGER NOT NULL DEFAULT 5 CHECK (max_attempts BETWEEN 1 AND 100);
ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;
ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS dead_lettered_at TIMESTAMPTZ;
UPDATE webhook_message_queue
SET next_attempt_at = scheduled_for
WHERE status = 'pending' AND next_attempt_at IS NULL;
UPDATE webhook_message_queue
SET attempt_count = 1
WHERE status = 'processing' AND attempt_count = 0;
CREATE INDEX IF NOT EXISTS idx_wmq_retry_dispatch
    ON webhook_message_queue(learner_id, kind, status, next_attempt_at, scheduled_for)`,
	},
	{
		Version: "postgres_0018_idempotency_response_expiry",
		Body: `ALTER TABLE tool_call_idempotency
    ADD COLUMN IF NOT EXISTS response_expired_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_tool_call_idempotency_completed
    ON tool_call_idempotency(status, completed_at)`,
	},
	{
		Version: "postgres_0019_revoke_unbound_refresh_tokens",
		Body: `WITH unsafe_families AS (
    SELECT DISTINCT family_id
    FROM refresh_tokens
    WHERE family_id <> ''
      AND (BTRIM(COALESCE(client_id, '')) = ''
        OR token NOT LIKE 'sha256:%')
)
UPDATE refresh_tokens
SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP)
WHERE family_id IN (SELECT family_id FROM unsafe_families)
   OR BTRIM(COALESCE(client_id, '')) = ''
   OR BTRIM(COALESCE(family_id, '')) = ''
   OR token NOT LIKE 'sha256:%'`,
	},
	{
		Version: "postgres_0020_webhook_domain_scope",
		Body: `ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS domain_id TEXT NOT NULL DEFAULT '';
UPDATE webhook_message_queue
SET domain_id = SUBSTRING(kind FROM 5)
WHERE domain_id = '' AND kind LIKE 'olm:%';
CREATE OR REPLACE FUNCTION migration_webhook_domain_id(raw_content TEXT)
RETURNS TEXT
LANGUAGE plpgsql
AS $migration$
BEGIN
    RETURN COALESCE(raw_content::jsonb ->> 'domain_id', '');
EXCEPTION WHEN OTHERS THEN
    RETURN '';
END;
$migration$;
UPDATE webhook_message_queue
SET domain_id = migration_webhook_domain_id(content)
WHERE domain_id = '';
DROP FUNCTION migration_webhook_domain_id(TEXT);
CREATE INDEX IF NOT EXISTS idx_wmq_domain_active
    ON webhook_message_queue(learner_id, domain_id, status)`,
	},
	{
		Version: "postgres_0021_purge_unbound_oauth_codes",
		Body: `DELETE FROM oauth_codes
WHERE BTRIM(COALESCE(client_id, '')) = ''
   OR BTRIM(COALESCE(redirect_uri, '')) = ''
   OR code_challenge_method <> 'S256'
	   OR BTRIM(COALESCE(code_challenge, '')) = ''`,
	},
	{
		Version: "postgres_0022_bind_oauth_credentials_to_resource",
		Body: `ALTER TABLE oauth_codes
    ADD COLUMN IF NOT EXISTS resource TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens
    ADD COLUMN IF NOT EXISTS resource TEXT NOT NULL DEFAULT '';
DELETE FROM oauth_codes WHERE BTRIM(resource) = '';
UPDATE refresh_tokens
SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP)
WHERE BTRIM(resource) = '';
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_client_resource
    ON refresh_tokens(client_id, resource)`,
	},
	{
		Version: "postgres_0023_expire_dynamic_oauth_clients",
		Body: `ALTER TABLE oauth_clients
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_oauth_clients_expires_at
    ON oauth_clients(expires_at)`,
	},
	{
		Version: "postgres_0024_verified_email_and_account_tokens",
		Body: `ALTER TABLE learners
    ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ;
UPDATE learners SET email_verified_at = COALESCE(created_at, CURRENT_TIMESTAMP)
WHERE email_verified_at IS NULL;
ALTER TABLE learners ALTER COLUMN email_verified_at DROP NOT NULL;
CREATE TABLE IF NOT EXISTS account_tokens (
    token_hash            TEXT PRIMARY KEY,
    learner_id            TEXT NOT NULL REFERENCES learners(id),
    purpose               TEXT NOT NULL CHECK (purpose IN ('email_verification','password_reset')),
    client_id             TEXT NOT NULL DEFAULT '',
    redirect_uri          TEXT NOT NULL DEFAULT '',
    resource              TEXT NOT NULL DEFAULT '',
    state                 TEXT NOT NULL DEFAULT '',
    scope                 TEXT NOT NULL DEFAULT '',
    code_challenge        TEXT NOT NULL DEFAULT '',
    code_challenge_method TEXT NOT NULL DEFAULT '',
    expires_at            TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL,
    consumed_at           TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_account_tokens_learner_purpose
    ON account_tokens(learner_id, purpose, expires_at)`,
	},
	{
		Version: "postgres_0025_learning_read_hot_paths",
		Body: `CREATE INDEX IF NOT EXISTS idx_assessment_attempts_evaluated_recent
    ON assessment_attempts(learner_id, domain_id, concept_id, evaluated_at DESC)
    WHERE status = 'evaluated'
      AND submitted_at IS NOT NULL
      AND evaluated_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_interactions_domain_recent
    ON interactions(learner_id, domain_id, created_at DESC)`,
	},
	{
		Version: "postgres_0026_oauth_tool_scopes",
		// DEFAULT learner is intentional expand/contract compatibility: an old
		// process omits the new columns and only knows the bounded legacy grant.
		Body: `ALTER TABLE oauth_codes ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'learner';
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'learner';
ALTER TABLE learner_approved_clients ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'learner';
UPDATE oauth_codes SET scope = 'learner' WHERE BTRIM(scope) = '';
UPDATE refresh_tokens SET scope = 'learner' WHERE BTRIM(scope) = '';
UPDATE learner_approved_clients SET scope = 'learner' WHERE BTRIM(scope) = '';
UPDATE account_tokens SET scope = 'learner'
WHERE purpose = 'email_verification' AND BTRIM(scope) = '';
DELETE FROM oauth_codes
WHERE scope NOT IN ('learner', 'learner:read', 'learner:write', 'learner:read learner:write');
UPDATE refresh_tokens
SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP)
WHERE scope NOT IN ('learner', 'learner:read', 'learner:write', 'learner:read learner:write');
DELETE FROM learner_approved_clients
WHERE scope NOT IN ('learner', 'learner:read', 'learner:write', 'learner:read learner:write');
UPDATE account_tokens
SET consumed_at = COALESCE(consumed_at, CURRENT_TIMESTAMP)
	WHERE purpose = 'email_verification'
	  AND scope NOT IN ('learner', 'learner:read', 'learner:write', 'learner:read learner:write')`,
	},
	{
		Version: "postgres_0027_recoverable_scheduled_job_leases",
		Body: `ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'succeeded';
ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS owner TEXT NOT NULL DEFAULT '';
ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS leased_until TIMESTAMPTZ;
ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS max_attempts INTEGER NOT NULL DEFAULT 3;
ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;
ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;
ALTER TABLE scheduled_job_runs
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE scheduled_job_runs
SET completed_at = COALESCE(completed_at, claimed_at),
    updated_at = COALESCE(updated_at, claimed_at)
WHERE status = 'succeeded';
ALTER TABLE scheduled_job_runs
    ALTER COLUMN updated_at SET DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE scheduled_job_runs
    ALTER COLUMN updated_at SET NOT NULL;
CREATE INDEX IF NOT EXISTS idx_scheduled_job_runs_runnable
    ON scheduled_job_runs(status, next_attempt_at, leased_until)`,
	},
	{
		Version: "postgres_0028_webhook_delivery_state_machine",
		Body: `ALTER TABLE scheduled_alerts
    ADD COLUMN IF NOT EXISTS delivery_state TEXT NOT NULL DEFAULT 'reserved';
UPDATE scheduled_alerts
SET delivery_state = CASE WHEN sent = 1 THEN 'delivered' ELSE 'reserved' END
WHERE delivery_state NOT IN ('reserved', 'delivered', 'delivery_unknown');
ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS event_id TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS reservation_id BIGINT REFERENCES scheduled_alerts(id);
ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS dispatch_started_at TIMESTAMPTZ;
UPDATE webhook_message_queue SET event_id = 'legacy-' || id::text WHERE event_id = '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_wmq_event_id
    ON webhook_message_queue(event_id) WHERE event_id <> '';
CREATE INDEX IF NOT EXISTS idx_wmq_delivery_reconcile
    ON webhook_message_queue(status, claimed_at, dispatch_started_at, id);
CREATE TABLE IF NOT EXISTS webhook_delivery_transitions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    queue_id      BIGINT NOT NULL REFERENCES webhook_message_queue(id) ON DELETE CASCADE,
    event_id      TEXT NOT NULL,
    learner_id    TEXT NOT NULL REFERENCES learners(id),
    attempt_count INTEGER NOT NULL,
    from_status   TEXT NOT NULL DEFAULT '',
    to_status     TEXT NOT NULL,
    reason        TEXT NOT NULL,
    occurred_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_transitions_event
    ON webhook_delivery_transitions(event_id, id DESC);
INSERT INTO webhook_delivery_transitions
    (queue_id, event_id, learner_id, attempt_count, from_status, to_status, reason, occurred_at)
SELECT id, event_id, learner_id, attempt_count, '', status, 'migration_snapshot',
       COALESCE(sent_at, claimed_at, created_at)
FROM webhook_message_queue q
WHERE NOT EXISTS (
    SELECT 1 FROM webhook_delivery_transitions t WHERE t.queue_id = q.id
)`,
	},
	{
		Version: "postgres_0029_webhook_content_format",
		Body: `ALTER TABLE webhook_message_queue
    ADD COLUMN IF NOT EXISTS content_format TEXT NOT NULL DEFAULT 'message'`,
	},
	{
		Version: "postgres_0030_bounded_login_failure_windows",
		Body: `CREATE TABLE IF NOT EXISTS login_failure_windows (
    account_key       TEXT PRIMARY KEY,
    window_started_at TIMESTAMPTZ NOT NULL,
    last_attempt_at   TIMESTAMPTZ NOT NULL,
    failure_count     INTEGER NOT NULL CHECK (failure_count BETWEEN 1 AND 100),
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_login_failure_windows_last_attempt
    ON login_failure_windows(last_attempt_at);
INSERT INTO login_failure_windows
    (account_key, window_started_at, last_attempt_at, failure_count, updated_at)
SELECT account_key, MIN(attempted_at), MAX(attempted_at),
       LEAST(COUNT(*), 100)::INTEGER, MAX(attempted_at)
FROM login_failures
WHERE attempted_at > CURRENT_TIMESTAMP - INTERVAL '10 minutes'
GROUP BY account_key
ON CONFLICT (account_key) DO NOTHING;
DELETE FROM login_failures`,
	},
	{
		Version: "postgres_0031_adaptive_login_challenges",
		Body: `CREATE TABLE IF NOT EXISTS login_challenges (
    token_hash            TEXT PRIMARY KEY,
    learner_id            TEXT NOT NULL REFERENCES learners(id),
    client_id             TEXT NOT NULL,
    redirect_uri          TEXT NOT NULL,
    resource              TEXT NOT NULL,
    state                 TEXT NOT NULL DEFAULT '',
    scope                 TEXT NOT NULL DEFAULT '',
    code_challenge        TEXT NOT NULL DEFAULT '',
    code_challenge_method TEXT NOT NULL DEFAULT '',
    expires_at            TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL,
    consumed_at           TIMESTAMPTZ,
    trusted_until         TIMESTAMPTZ,
    active                INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_login_challenges_one_active
    ON login_challenges(learner_id) WHERE active = 1;
CREATE INDEX IF NOT EXISTS idx_login_challenges_cleanup
    ON login_challenges(active, expires_at, trusted_until)`,
	},
	{
		Version: "postgres_0032_shared_narrative_objects",
		Body: `CREATE TABLE IF NOT EXISTS narrative_objects (
    learner_id  TEXT NOT NULL REFERENCES learners(id) ON DELETE CASCADE,
    scope       TEXT NOT NULL CHECK (scope IN ('memory','memory_pending','session','concept','archive')),
    domain_id   TEXT NOT NULL DEFAULT '',
    object_key  TEXT NOT NULL DEFAULT '',
    ciphertext  TEXT NOT NULL,
    key_id      TEXT NOT NULL,
    version     BIGINT NOT NULL CHECK (version > 0),
    checksum    TEXT NOT NULL CHECK (LENGTH(checksum) = 64),
    size_bytes  BIGINT NOT NULL CHECK (size_bytes >= 0),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (learner_id, scope, domain_id, object_key)
);
CREATE INDEX IF NOT EXISTS idx_narrative_objects_list
    ON narrative_objects(learner_id, scope, domain_id, object_key DESC);
CREATE INDEX IF NOT EXISTS idx_narrative_objects_retention
    ON narrative_objects(updated_at, learner_id);
CREATE TABLE IF NOT EXISTS narrative_mutations (
    learner_id       TEXT NOT NULL REFERENCES learners(id) ON DELETE CASCADE,
    mutation_id      TEXT NOT NULL,
    scope            TEXT NOT NULL,
    domain_id        TEXT NOT NULL DEFAULT '',
    object_key       TEXT NOT NULL DEFAULT '',
    mutation_checksum TEXT NOT NULL CHECK (LENGTH(mutation_checksum) = 64),
    result_version   BIGINT NOT NULL CHECK (result_version > 0),
    created_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (learner_id, mutation_id)
);
CREATE INDEX IF NOT EXISTS idx_narrative_mutations_created
    ON narrative_mutations(created_at)`,
	},
	{
		Version: "postgres_0033_dcr_token_lifecycle",
		Body: `CREATE TABLE IF NOT EXISTS oauth_dcr_initial_access_tokens (
    token_id           TEXT PRIMARY KEY,
    token_hash         TEXT NOT NULL UNIQUE CHECK (LENGTH(token_hash) = 64),
    label              TEXT NOT NULL,
    max_registrations  INTEGER NOT NULL CHECK (max_registrations BETWEEN 1 AND 100000),
    used_registrations INTEGER NOT NULL DEFAULT 0 CHECK (used_registrations >= 0 AND used_registrations <= max_registrations),
    created_at         TIMESTAMPTZ NOT NULL,
    expires_at         TIMESTAMPTZ,
    revoked_at         TIMESTAMPTZ,
    created_by         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_dcr_tokens_active
    ON oauth_dcr_initial_access_tokens(revoked_at, expires_at);
CREATE TABLE IF NOT EXISTS oauth_dcr_audit (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    action      TEXT NOT NULL,
    actor       TEXT NOT NULL,
    token_id    TEXT NOT NULL DEFAULT '',
    client_id   TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_dcr_audit_time
    ON oauth_dcr_audit(occurred_at DESC, id DESC);
ALTER TABLE oauth_clients ADD COLUMN IF NOT EXISTS registration_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_clients ADD COLUMN IF NOT EXISTS registration_token_id TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_clients ADD COLUMN IF NOT EXISTS registration_secret_ciphertext TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_clients_registration_fingerprint
    ON oauth_clients(registration_fingerprint)
    WHERE registration_fingerprint <> ''`,
	},
	{
		Version: "postgres_0034_retention_jobs_and_legal_holds",
		Body: `CREATE TABLE IF NOT EXISTS retention_legal_holds (
    hold_id        TEXT PRIMARY KEY,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    reason         TEXT NOT NULL,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    released_at    TIMESTAMPTZ,
    released_by    TEXT NOT NULL DEFAULT '',
    release_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_retention_legal_holds_active
    ON retention_legal_holds(learner_id, created_at)
    WHERE released_at IS NULL;
CREATE TABLE IF NOT EXISTS retention_jobs (
    job_id            TEXT PRIMARY KEY,
    policy_json       TEXT NOT NULL,
    policy_hash       TEXT NOT NULL CHECK (LENGTH(policy_hash) = 64),
    as_of             TIMESTAMPTZ NOT NULL,
    backup_reference  TEXT NOT NULL,
    backup_created_at TIMESTAMPTZ NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('pending','running','failed','completed')),
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    lease_owner       TEXT NOT NULL DEFAULT '',
    leased_until      TIMESTAMPTZ,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    started_at        TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ,
    last_error        TEXT NOT NULL DEFAULT '',
    report_json       TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_retention_jobs_status_lease
    ON retention_jobs(status, leased_until, created_at);
CREATE TABLE IF NOT EXISTS retention_job_phases (
    job_id        TEXT NOT NULL REFERENCES retention_jobs(job_id) ON DELETE CASCADE,
    phase         TEXT NOT NULL CHECK (phase IN ('database','narrative')),
    position      INTEGER NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('pending','running','failed','completed','skipped')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    eligible      BIGINT NOT NULL DEFAULT 0,
    applied       BIGINT NOT NULL DEFAULT 0,
    held          BIGINT NOT NULL DEFAULT 0,
    report_json   TEXT NOT NULL DEFAULT '{}',
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    last_error    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, phase)
);
CREATE INDEX IF NOT EXISTS idx_retention_job_phases_order
    ON retention_job_phases(job_id, position)`,
	},
	{
		Version: "postgres_0035_tenant_identity_foundation",
		Body: `CREATE TABLE IF NOT EXISTS tenants (
    id          TEXT PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('active','suspended','closed')),
    region      TEXT NOT NULL DEFAULT 'default',
    policy_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
INSERT INTO tenants (id, slug, name, status, region, policy_json, created_at, updated_at)
VALUES ('tenant_legacy', 'legacy', 'Legacy tenant', 'active', 'default', '{}'::jsonb, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT (id) DO NOTHING;
CREATE TABLE IF NOT EXISTS users (
    id                TEXT PRIMARY KEY,
    email             TEXT NOT NULL,
    normalized_email  TEXT NOT NULL,
    password_hash     TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('pending','active','suspended','revoked')),
    email_verified_at TIMESTAMPTZ,
    token_version     BIGINT NOT NULL DEFAULT 1 CHECK (token_version >= 1),
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_normalized_email ON users(normalized_email);
CREATE TABLE IF NOT EXISTS tenant_memberships (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    user_id      TEXT NOT NULL REFERENCES users(id),
    learner_id   TEXT REFERENCES learners(id),
    roles_json   JSONB NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('invited','active','suspended','revoked')),
    version      BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, user_id),
    UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tenant_memberships_tenant_learner
    ON tenant_memberships(tenant_id, learner_id) WHERE learner_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tenant_memberships_user_status
    ON tenant_memberships(user_id, status, tenant_id);
CREATE TABLE IF NOT EXISTS external_identities (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,
    issuer        TEXT NOT NULL,
    subject       TEXT NOT NULL,
    email_at_link TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (provider, issuer, subject)
);
ALTER TABLE learners ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE learners ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE learners ADD COLUMN IF NOT EXISTS membership_id TEXT NOT NULL DEFAULT '';
UPDATE learners
SET tenant_id = 'tenant_legacy', user_id = id, membership_id = 'membership_legacy_' || id
WHERE tenant_id = 'tenant_legacy' AND (user_id = '' OR membership_id = '');
INSERT INTO users
    (id, email, normalized_email, password_hash, status, email_verified_at, token_version, created_at, updated_at)
SELECT id, email, lower(email), password_hash,
       CASE WHEN email_verified_at IS NULL THEN 'pending' ELSE 'active' END,
       email_verified_at, 1, created_at, COALESCE(last_active, created_at)
FROM learners
ON CONFLICT (id) DO NOTHING;
INSERT INTO tenant_memberships
    (id, tenant_id, user_id, learner_id, roles_json, status, version, created_at, updated_at)
SELECT membership_id, tenant_id, user_id, id, '["learner"]'::jsonb,
       CASE WHEN email_verified_at IS NULL THEN 'invited' ELSE 'active' END,
       1, created_at, COALESCE(last_active, created_at)
FROM learners
ON CONFLICT (id) DO NOTHING;
CREATE UNIQUE INDEX IF NOT EXISTS idx_learners_tenant_id_id ON learners(tenant_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_learners_tenant_membership ON learners(tenant_id, membership_id);
CREATE OR REPLACE FUNCTION tutor_legacy_learner_identity_before_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tenant_id IS NULL OR NEW.tenant_id = '' THEN NEW.tenant_id := 'tenant_legacy'; END IF;
    IF NEW.user_id = '' THEN NEW.user_id := NEW.id; END IF;
    IF NEW.membership_id = '' THEN NEW.membership_id := 'membership_legacy_' || NEW.id; END IF;
    INSERT INTO users
        (id, email, normalized_email, password_hash, status, email_verified_at, token_version, created_at, updated_at)
    VALUES (NEW.user_id, NEW.email, lower(NEW.email), NEW.password_hash,
        CASE WHEN NEW.email_verified_at IS NULL THEN 'pending' ELSE 'active' END,
        NEW.email_verified_at, 1, NEW.created_at, COALESCE(NEW.last_active, NEW.created_at))
    ON CONFLICT (id) DO NOTHING;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS learners_identity_before_insert ON learners;
CREATE TRIGGER learners_identity_before_insert
BEFORE INSERT ON learners FOR EACH ROW EXECUTE FUNCTION tutor_legacy_learner_identity_before_insert();
CREATE OR REPLACE FUNCTION tutor_legacy_learner_membership_after_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO tenant_memberships
        (id, tenant_id, user_id, learner_id, roles_json, status, version, created_at, updated_at)
    VALUES (NEW.membership_id, NEW.tenant_id, NEW.user_id, NEW.id, '["learner"]'::jsonb,
        CASE WHEN NEW.email_verified_at IS NULL THEN 'invited' ELSE 'active' END,
        1, NEW.created_at, COALESCE(NEW.last_active, NEW.created_at))
    ON CONFLICT (id) DO NOTHING;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS learners_identity_after_insert ON learners;
CREATE TRIGGER learners_identity_after_insert
AFTER INSERT ON learners FOR EACH ROW EXECUTE FUNCTION tutor_legacy_learner_membership_after_insert();
CREATE OR REPLACE FUNCTION tutor_learner_identity_after_update()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.email_verified_at IS NULL AND NEW.email_verified_at IS NOT NULL THEN
        UPDATE users
        SET status = 'active', email_verified_at = NEW.email_verified_at, updated_at = CURRENT_TIMESTAMP
        WHERE id = NEW.user_id AND status = 'pending';
        UPDATE tenant_memberships
        SET status = 'active', version = version + 1, updated_at = CURRENT_TIMESTAMP
        WHERE tenant_id = NEW.tenant_id AND id = NEW.membership_id AND status = 'invited';
    END IF;
    IF OLD.password_hash IS DISTINCT FROM NEW.password_hash THEN
        UPDATE users
        SET password_hash = NEW.password_hash, token_version = token_version + 1, updated_at = CURRENT_TIMESTAMP
        WHERE id = NEW.user_id;
        UPDATE tenant_memberships
        SET version = version + 1, updated_at = CURRENT_TIMESTAMP
        WHERE user_id = NEW.user_id AND status IN ('invited','active');
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS learners_identity_after_update ON learners;
CREATE TRIGGER learners_identity_after_update
AFTER UPDATE OF email_verified_at, password_hash ON learners
FOR EACH ROW EXECUTE FUNCTION tutor_learner_identity_after_update();`,
	},
	{
		Version: "postgres_0036_tenant_columns_composite_fk_rls",
		Body: `CREATE TABLE IF NOT EXISTS tenant_migration_quarantine (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_table   TEXT NOT NULL,
    source_key     TEXT NOT NULL,
    reason         TEXT NOT NULL,
    content_hash   TEXT NOT NULL,
    detected_at    TIMESTAMPTZ NOT NULL,
    resolved_at    TIMESTAMPTZ,
    resolution     TEXT NOT NULL DEFAULT '',
    UNIQUE (source_table, source_key, reason)
);
DO $$
DECLARE
    table_name text;
    tenant_tables text[] := ARRAY[
        'refresh_tokens','domains','concept_states','interactions','availability',
        'scheduled_alerts','oauth_codes','learner_approved_clients','affect_states',
        'calibration_records','transfer_records','implementation_intentions',
        'webhook_message_queue','pedagogical_snapshots','pending_consolidations',
        'webhook_push_log','learning_sessions','assessment_attempts',
        'curriculum_versions','curriculum_concepts','curriculum_metadata_ids',
        'tool_call_idempotency','account_tokens','login_challenges',
        'narrative_objects','narrative_mutations','retention_legal_holds',
        'webhook_delivery_transitions'
    ];
BEGIN
    FOREACH table_name IN ARRAY tenant_tables LOOP
		EXECUTE format('ALTER TABLE %I ADD COLUMN IF NOT EXISTS tenant_id TEXT DEFAULT ''tenant_legacy''', table_name);
        IF EXISTS (SELECT 1 FROM pg_class WHERE relname = table_name) THEN
            EXECUTE format('ALTER TABLE %I ALTER COLUMN tenant_id SET NOT NULL', table_name);
        END IF;
		EXECUTE format('ALTER TABLE %I ALTER COLUMN tenant_id DROP DEFAULT', table_name);
		EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I (tenant_id, learner_id)',
            'idx_' || table_name || '_tenant_learner', table_name);
    END LOOP;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS idx_learners_tenant_id_id ON learners(tenant_id, id);
CREATE OR REPLACE FUNCTION tutor_enforce_tenant_from_learner()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE resolved_tenant text;
BEGIN
    SELECT tenant_id INTO resolved_tenant FROM learners WHERE id = NEW.learner_id;
    IF resolved_tenant IS NULL THEN
        RAISE EXCEPTION 'unknown learner for tenant-owned row' USING ERRCODE = '23503';
    END IF;
    IF NEW.tenant_id IS NULL THEN
        NEW.tenant_id := resolved_tenant;
    ELSIF NEW.tenant_id <> resolved_tenant THEN
        RAISE EXCEPTION 'cross-tenant learner relation' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
DO $$
DECLARE
    table_name text;
    constraint_name text;
    tenant_tables text[] := ARRAY[
        'refresh_tokens','domains','concept_states','interactions','availability',
        'scheduled_alerts','oauth_codes','learner_approved_clients','affect_states',
        'calibration_records','transfer_records','implementation_intentions',
        'webhook_message_queue','pedagogical_snapshots','pending_consolidations',
        'webhook_push_log','learning_sessions','assessment_attempts',
        'curriculum_versions','curriculum_concepts','curriculum_metadata_ids',
        'tool_call_idempotency','account_tokens','login_challenges',
        'narrative_objects','narrative_mutations','retention_legal_holds',
        'webhook_delivery_transitions'
    ];
BEGIN
    FOREACH table_name IN ARRAY tenant_tables LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS tenant_scope_guard ON %I', table_name);
        EXECUTE format('CREATE TRIGGER tenant_scope_guard BEFORE INSERT OR UPDATE OF tenant_id, learner_id ON %I FOR EACH ROW EXECUTE FUNCTION tutor_enforce_tenant_from_learner()', table_name);
        constraint_name := table_name || '_tenant_learner_fk';
        IF NOT EXISTS (
            SELECT 1
            FROM pg_constraint
            WHERE conrelid = to_regclass(table_name)
              AND conname = constraint_name
        ) THEN
            EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id) NOT VALID', table_name, constraint_name);
        END IF;
        EXECUTE format('ALTER TABLE %I VALIDATE CONSTRAINT %I', table_name, constraint_name);
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', table_name);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %1$I USING (tenant_id = current_setting(''app.current_tenant'', true)) WITH CHECK (tenant_id = current_setting(''app.current_tenant'', true))',
            table_name
        );
    END LOOP;
END $$;
ALTER TABLE learners ENABLE ROW LEVEL SECURITY;
ALTER TABLE learners FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON learners;
CREATE POLICY tenant_isolation ON learners
USING (tenant_id = current_setting('app.current_tenant', true))
WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
ALTER TABLE tenant_memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_memberships FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON tenant_memberships;
CREATE POLICY tenant_isolation ON tenant_memberships
USING (tenant_id = current_setting('app.current_tenant', true))
WITH CHECK (tenant_id = current_setting('app.current_tenant', true));`,
	},
	{
		Version: "postgres_0037_tenant_scope_oauth_credentials",
		Body: `CREATE TABLE IF NOT EXISTS credential_tenant_routes (
    kind           TEXT NOT NULL CHECK (kind IN ('authorization_code','refresh_token','email_verification','password_reset','login_challenge')),
    credential_key TEXT NOT NULL,
    tenant_id      TEXT NOT NULL REFERENCES tenants(id),
    user_id        TEXT NOT NULL REFERENCES users(id),
    membership_id  TEXT NOT NULL,
    learner_id     TEXT NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (kind, credential_key),
    FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id),
    FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_credential_tenant_routes_expiry
    ON credential_tenant_routes(expires_at, kind);
ALTER TABLE oauth_codes DISABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens DISABLE ROW LEVEL SECURITY;
ALTER TABLE learner_approved_clients DISABLE ROW LEVEL SECURITY;
ALTER TABLE account_tokens DISABLE ROW LEVEL SECURITY;
ALTER TABLE login_challenges DISABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_codes ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_codes ADD COLUMN IF NOT EXISTS membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_codes ADD COLUMN IF NOT EXISTS membership_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS membership_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE learner_approved_clients ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE learner_approved_clients ADD COLUMN IF NOT EXISTS membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE account_tokens ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE account_tokens ADD COLUMN IF NOT EXISTS membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE login_challenges ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE login_challenges ADD COLUMN IF NOT EXISTS membership_id TEXT NOT NULL DEFAULT '';
UPDATE oauth_codes c SET user_id = l.user_id, membership_id = l.membership_id
FROM learners l WHERE c.learner_id = l.id AND (c.user_id = '' OR c.membership_id = '');
UPDATE refresh_tokens r SET user_id = l.user_id, membership_id = l.membership_id
FROM learners l WHERE r.learner_id = l.id AND (r.user_id = '' OR r.membership_id = '');
UPDATE learner_approved_clients c SET user_id = l.user_id, membership_id = l.membership_id
FROM learners l WHERE c.learner_id = l.id AND (c.user_id = '' OR c.membership_id = '');
UPDATE account_tokens a SET user_id = l.user_id, membership_id = l.membership_id
FROM learners l WHERE a.learner_id = l.id AND (a.user_id = '' OR a.membership_id = '');
UPDATE login_challenges c SET user_id = l.user_id, membership_id = l.membership_id
FROM learners l WHERE c.learner_id = l.id AND (c.user_id = '' OR c.membership_id = '');
DELETE FROM oauth_codes;
UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP);
DELETE FROM account_tokens;
DELETE FROM login_challenges;
CREATE INDEX IF NOT EXISTS idx_oauth_codes_tenant_membership ON oauth_codes(tenant_id, membership_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_tenant_membership ON refresh_tokens(tenant_id, membership_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_approved_clients_tenant_membership ON learner_approved_clients(tenant_id, membership_id);
ALTER TABLE oauth_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_codes FORCE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens FORCE ROW LEVEL SECURITY;
ALTER TABLE learner_approved_clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE learner_approved_clients FORCE ROW LEVEL SECURITY;
ALTER TABLE account_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE account_tokens FORCE ROW LEVEL SECURITY;
ALTER TABLE login_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE login_challenges FORCE ROW LEVEL SECURITY;`,
	},
	{
		Version: "postgres_0038_membership_identity_selection_policy",
		Body: `DROP POLICY IF EXISTS tenant_isolation ON tenant_memberships;
CREATE POLICY tenant_isolation ON tenant_memberships
USING (
    tenant_id = current_setting('app.current_tenant', true)
    OR user_id = current_setting('app.identity_user', true)
)
WITH CHECK (tenant_id = current_setting('app.current_tenant', true));`,
	},
	{
		Version: "postgres_0039_invitations_mfa_services_audit",
		Body: `CREATE TABLE IF NOT EXISTS tenant_invitations (
    id             TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL UNIQUE,
    tenant_id      TEXT NOT NULL REFERENCES tenants(id),
    email          TEXT NOT NULL,
    normalized_email TEXT NOT NULL,
    roles_json     JSONB NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('pending','accepted','revoked','expired')),
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    accepted_at    TIMESTAMPTZ,
    accepted_user_id TEXT,
    accepted_membership_id TEXT
);
CREATE INDEX IF NOT EXISTS idx_tenant_invitations_tenant_status ON tenant_invitations(tenant_id, status, expires_at);
CREATE TABLE IF NOT EXISTS invitation_tenant_routes (
    token_hash TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_invitation_tenant_routes_expiry ON invitation_tenant_routes(expires_at);
CREATE TABLE IF NOT EXISTS mfa_credentials (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('totp','webauthn')),
    label          TEXT NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    key_id         TEXT NOT NULL,
    credential_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL,
    last_used_at   TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mfa_credentials_user_active ON mfa_credentials(user_id, revoked_at);
ALTER TABLE tenant_memberships ADD COLUMN IF NOT EXISTS mfa_required INTEGER NOT NULL DEFAULT 0 CHECK (mfa_required IN (0,1));
ALTER TABLE tenant_memberships ADD COLUMN IF NOT EXISTS mfa_verified_at TIMESTAMPTZ;
UPDATE tenant_memberships SET mfa_required = 1
WHERE roles_json ?| ARRAY['owner','admin'];
CREATE TABLE IF NOT EXISTS service_accounts (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL REFERENCES tenants(id),
    name          TEXT NOT NULL,
    client_id     TEXT NOT NULL REFERENCES oauth_clients(client_id),
    roles_json    JSONB NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('active','suspended','revoked')),
    version       BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, client_id)
);
CREATE TABLE IF NOT EXISTS audit_events (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id),
    actor_user_id   TEXT NOT NULL,
    membership_id   TEXT NOT NULL,
    action          TEXT NOT NULL,
    target_type     TEXT NOT NULL,
    target_id       TEXT NOT NULL,
    request_id      TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    details_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_time ON audit_events(tenant_id, occurred_at DESC, id DESC);
CREATE OR REPLACE FUNCTION tutor_audit_append_only()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'audit events are append-only'; END $$;
DROP TRIGGER IF EXISTS audit_events_no_mutation ON audit_events;
CREATE TRIGGER audit_events_no_mutation BEFORE UPDATE OR DELETE ON audit_events
FOR EACH ROW EXECUTE FUNCTION tutor_audit_append_only();
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['tenant_invitations','service_accounts','audit_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %1$I USING (tenant_id = current_setting(''app.current_tenant'', true)) WITH CHECK (tenant_id = current_setting(''app.current_tenant'', true))', table_name);
    END LOOP;
END $$;`,
	},
	{
		Version: "postgres_0040_formation_catalog_cohorts_enrollments",
		Body: `CREATE TABLE IF NOT EXISTS formations (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL CHECK (status IN ('draft','active','archived')),
    created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE TABLE IF NOT EXISTS formation_versions (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL, formation_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version >= 1), status TEXT NOT NULL CHECK (status IN ('draft','published','superseded')),
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL, published_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id), UNIQUE (tenant_id, formation_id, version),
    FOREIGN KEY (tenant_id, formation_id) REFERENCES formations(tenant_id, id)
);
CREATE TABLE IF NOT EXISTS formation_modules (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL, formation_version_id TEXT NOT NULL,
    stable_key TEXT NOT NULL, title TEXT NOT NULL, position INTEGER NOT NULL CHECK (position >= 0),
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (tenant_id, id), UNIQUE (tenant_id, formation_version_id, stable_key),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id)
);
CREATE TABLE IF NOT EXISTS formation_concepts (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL, formation_version_id TEXT NOT NULL,
    module_id TEXT NOT NULL, stable_key TEXT NOT NULL, label TEXT NOT NULL,
    position INTEGER NOT NULL CHECK (position >= 0), metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (tenant_id, id), UNIQUE (tenant_id, formation_version_id, stable_key),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
    FOREIGN KEY (tenant_id, module_id) REFERENCES formation_modules(tenant_id, id)
);
CREATE TABLE IF NOT EXISTS concept_prerequisites (
    tenant_id TEXT NOT NULL, formation_version_id TEXT NOT NULL,
    concept_id TEXT NOT NULL, prerequisite_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, formation_version_id, concept_id, prerequisite_id),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
    FOREIGN KEY (tenant_id, concept_id) REFERENCES formation_concepts(tenant_id, id),
    FOREIGN KEY (tenant_id, prerequisite_id) REFERENCES formation_concepts(tenant_id, id),
    CHECK (concept_id <> prerequisite_id)
);
CREATE TABLE IF NOT EXISTS cohorts (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL, formation_version_id TEXT NOT NULL,
    name TEXT NOT NULL, starts_at TIMESTAMPTZ, ends_at TIMESTAMPTZ,
    capacity INTEGER NOT NULL CHECK (capacity > 0),
    reserved_seats INTEGER NOT NULL DEFAULT 0 CHECK (reserved_seats >= 0 AND reserved_seats <= capacity),
    status TEXT NOT NULL CHECK (status IN ('planned','open','closed','archived')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1), created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
    CHECK (ends_at IS NULL OR starts_at IS NULL OR ends_at > starts_at)
);
CREATE TABLE IF NOT EXISTS cohort_trainers (
    tenant_id TEXT NOT NULL, cohort_id TEXT NOT NULL, membership_id TEXT NOT NULL,
    assigned_by TEXT NOT NULL, assigned_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, cohort_id, membership_id),
    FOREIGN KEY (tenant_id, cohort_id) REFERENCES cohorts(tenant_id, id),
    FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id)
);
CREATE TABLE IF NOT EXISTS enrollments (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL, cohort_id TEXT NOT NULL,
    formation_version_id TEXT NOT NULL, user_id TEXT NOT NULL, membership_id TEXT NOT NULL,
    learner_id TEXT, status TEXT NOT NULL CHECK (status IN ('invited','active','completed','suspended','cancelled')),
    objectives_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    seat_reserved INTEGER NOT NULL DEFAULT 1 CHECK (seat_reserved IN (0,1)),
    created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, completed_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id), UNIQUE (tenant_id, cohort_id, user_id),
    FOREIGN KEY (tenant_id, cohort_id) REFERENCES cohorts(tenant_id, id),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
    FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id),
    FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_formation_versions_tenant_formation ON formation_versions(tenant_id, formation_id, version DESC);
CREATE INDEX IF NOT EXISTS idx_cohorts_tenant_status ON cohorts(tenant_id, status, starts_at);
CREATE INDEX IF NOT EXISTS idx_enrollments_tenant_user ON enrollments(tenant_id, user_id, status);
CREATE INDEX IF NOT EXISTS idx_enrollments_tenant_cohort ON enrollments(tenant_id, cohort_id, status);
CREATE OR REPLACE FUNCTION tutor_published_formation_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE version_status text;
BEGIN
	IF TG_TABLE_NAME = 'formation_versions' THEN
		IF TG_OP = 'INSERT' THEN RETURN NEW; END IF;
		version_status := OLD.status;
	ELSIF TG_OP = 'DELETE' THEN
		SELECT status INTO version_status FROM formation_versions WHERE tenant_id = OLD.tenant_id AND id = OLD.formation_version_id;
	ELSE
		SELECT status INTO version_status FROM formation_versions WHERE tenant_id = NEW.tenant_id AND id = NEW.formation_version_id;
    END IF;
    IF version_status = 'published' THEN RAISE EXCEPTION 'published formation content is immutable'; END IF;
	IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
	RETURN NEW;
END $$;
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['formation_versions','formation_modules','formation_concepts','concept_prerequisites'] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS published_formation_immutable ON %I', table_name);
		IF table_name = 'formation_versions' THEN
			EXECUTE format('CREATE TRIGGER published_formation_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION tutor_published_formation_immutable()', table_name);
		ELSE
			EXECUTE format('CREATE TRIGGER published_formation_immutable BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION tutor_published_formation_immutable()', table_name);
		END IF;
    END LOOP;
    FOREACH table_name IN ARRAY ARRAY['formations','formation_versions','formation_modules','formation_concepts','concept_prerequisites','cohorts','cohort_trainers','enrollments'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %1$I USING (tenant_id = current_setting(''app.current_tenant'', true)) WITH CHECK (tenant_id = current_setting(''app.current_tenant'', true))', table_name);
    END LOOP;
END $$;`,
	},
	{
		Version: "postgres_0041_legacy_enrollment_backfill",
		Body: `CREATE TABLE legacy_domain_enrollments (
    tenant_id TEXT NOT NULL, learner_id TEXT NOT NULL, domain_id TEXT NOT NULL DEFAULT '',
    enrollment_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, learner_id, domain_id),
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id)
);
CREATE TABLE legacy_concept_sources (
    tenant_id TEXT NOT NULL, learner_id TEXT NOT NULL, enrollment_id TEXT NOT NULL,
    domain_id TEXT NOT NULL DEFAULT '', concept_label TEXT NOT NULL, concept_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, enrollment_id, domain_id, concept_label),
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id)
);
CREATE TABLE legacy_concept_mappings (
    tenant_id TEXT NOT NULL, enrollment_id TEXT NOT NULL, domain_id TEXT NOT NULL DEFAULT '',
    concept_label TEXT NOT NULL, concept_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, enrollment_id, domain_id, concept_label),
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id),
    FOREIGN KEY (tenant_id, concept_id) REFERENCES formation_concepts(tenant_id, id)
);
INSERT INTO formations (id, tenant_id, name, description, status, created_by, created_at, updated_at)
SELECT 'legacy_formation_' || d.id, d.tenant_id, d.name, d.personal_goal,
       'active', l.user_id, d.created_at, d.created_at
FROM domains d JOIN learners l ON l.tenant_id = d.tenant_id AND l.id = d.learner_id;
INSERT INTO formation_versions
    (id, tenant_id, formation_id, version, status, metadata_json, created_by, created_at)
SELECT 'legacy_version_' || d.id, d.tenant_id, 'legacy_formation_' || d.id,
       1, 'draft', '{}'::jsonb, l.user_id, d.created_at
FROM domains d JOIN learners l ON l.tenant_id = d.tenant_id AND l.id = d.learner_id;
INSERT INTO formation_modules
    (id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
SELECT 'legacy_module_' || d.id, d.tenant_id, 'legacy_version_' || d.id,
       'legacy', d.name, 0, '{}'::jsonb FROM domains d;
INSERT INTO cohorts
    (id, tenant_id, formation_version_id, name, capacity, reserved_seats, status,
     version, created_by, created_at, updated_at)
SELECT 'legacy_cohort_' || d.id, d.tenant_id, 'legacy_version_' || d.id,
       'Legacy individual enrollment', 1, 1, 'open', 1, l.user_id, d.created_at, d.created_at
FROM domains d JOIN learners l ON l.tenant_id = d.tenant_id AND l.id = d.learner_id;
INSERT INTO enrollments
    (id, tenant_id, cohort_id, formation_version_id, user_id, membership_id,
     learner_id, status, objectives_json, seat_reserved, created_at, updated_at)
SELECT 'legacy_enrollment_' || d.id, d.tenant_id, 'legacy_cohort_' || d.id,
       'legacy_version_' || d.id, l.user_id, l.membership_id, l.id, 'active',
       '{}'::jsonb, 1, d.created_at, d.created_at
FROM domains d JOIN learners l ON l.tenant_id = d.tenant_id AND l.id = d.learner_id;
INSERT INTO legacy_domain_enrollments (tenant_id, learner_id, domain_id, enrollment_id)
SELECT tenant_id, learner_id, id, 'legacy_enrollment_' || id FROM domains;
INSERT INTO formations (id, tenant_id, name, description, status, created_by, created_at, updated_at)
SELECT 'legacy_recovery_formation_' || id, tenant_id, 'Legacy recovery',
       'Quarantined evidence with no unambiguous historical domain', 'active',
       user_id, created_at, created_at FROM learners;
INSERT INTO formation_versions
    (id, tenant_id, formation_id, version, status, metadata_json, created_by, created_at)
SELECT 'legacy_recovery_version_' || id, tenant_id, 'legacy_recovery_formation_' || id,
       1, 'draft', '{"quarantine":true}'::jsonb, user_id, created_at FROM learners;
INSERT INTO formation_modules
    (id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
SELECT 'legacy_recovery_module_' || id, tenant_id, 'legacy_recovery_version_' || id,
       'recovery', 'Recovery', 0, '{"quarantine":true}'::jsonb FROM learners;
INSERT INTO cohorts
    (id, tenant_id, formation_version_id, name, capacity, reserved_seats, status,
     version, created_by, created_at, updated_at)
SELECT 'legacy_recovery_cohort_' || id, tenant_id, 'legacy_recovery_version_' || id,
       'Legacy recovery', 1, 1, 'archived', 1, user_id, created_at, created_at FROM learners;
INSERT INTO enrollments
    (id, tenant_id, cohort_id, formation_version_id, user_id, membership_id,
     learner_id, status, objectives_json, seat_reserved, created_at, updated_at)
SELECT 'legacy_recovery_enrollment_' || id, tenant_id, 'legacy_recovery_cohort_' || id,
       'legacy_recovery_version_' || id, user_id, membership_id, id, 'completed',
       '{"quarantine":true}'::jsonb, 1, created_at, created_at FROM learners;
INSERT INTO legacy_domain_enrollments (tenant_id, learner_id, domain_id, enrollment_id)
SELECT tenant_id, id, '', 'legacy_recovery_enrollment_' || id FROM learners;
INSERT INTO legacy_concept_sources
    (tenant_id, learner_id, enrollment_id, domain_id, concept_label, concept_id)
SELECT source.tenant_id, source.learner_id, mapping.enrollment_id, source.domain_id,
       source.concept_label,
       'legacy_concept_' || md5(mapping.enrollment_id || chr(31) || source.concept_label)
FROM (
    SELECT tenant_id, learner_id, domain_id, concept AS concept_label FROM concept_states
    UNION
    SELECT tenant_id, learner_id, COALESCE(domain_id, ''), concept FROM interactions
    UNION
    SELECT tenant_id, learner_id, domain_id, concept_id FROM assessment_attempts
) source
JOIN legacy_domain_enrollments mapping
  ON mapping.tenant_id = source.tenant_id
 AND mapping.learner_id = source.learner_id
 AND mapping.domain_id = source.domain_id
WHERE source.concept_label <> ''
ON CONFLICT DO NOTHING;
INSERT INTO legacy_concept_sources
    (tenant_id, learner_id, enrollment_id, domain_id, concept_label, concept_id)
SELECT mapping.tenant_id, mapping.learner_id, mapping.enrollment_id, mapping.domain_id, '',
       'legacy_unmapped_' || md5(mapping.enrollment_id)
FROM legacy_domain_enrollments mapping
ON CONFLICT DO NOTHING;
INSERT INTO formation_concepts
    (id, tenant_id, formation_version_id, module_id, stable_key, label, position, metadata_json)
SELECT source.concept_id, source.tenant_id, enrollment.formation_version_id,
       module.id,
       CASE WHEN source.concept_label = '' THEN 'legacy_unmapped'
            ELSE 'legacy_' || md5(source.concept_label) END,
       CASE WHEN source.concept_label = '' THEN 'Unmapped legacy evidence'
            ELSE source.concept_label END,
       ROW_NUMBER() OVER (
           PARTITION BY source.tenant_id, enrollment.formation_version_id
           ORDER BY source.concept_label
       ) - 1,
       CASE WHEN source.domain_id = '' OR source.concept_label = ''
            THEN '{"quarantine":true,"unmapped":true}'::jsonb ELSE '{}'::jsonb END
FROM legacy_concept_sources source
JOIN enrollments enrollment
  ON enrollment.tenant_id = source.tenant_id AND enrollment.id = source.enrollment_id
JOIN formation_modules module
  ON module.tenant_id = enrollment.tenant_id
 AND module.formation_version_id = enrollment.formation_version_id;
INSERT INTO legacy_concept_mappings
    (tenant_id, enrollment_id, domain_id, concept_label, concept_id)
SELECT tenant_id, enrollment_id, domain_id, concept_label, concept_id
FROM legacy_concept_sources;
UPDATE formation_versions
SET status = 'published', published_at = created_at
WHERE status = 'draft'
  AND (id IN (SELECT 'legacy_version_' || id FROM domains)
       OR id IN (SELECT 'legacy_recovery_version_' || id FROM learners));
INSERT INTO tenant_migration_quarantine
    (source_table, source_key, reason, content_hash, detected_at)
SELECT 'concept_states', id::text, 'missing_or_ambiguous_domain',
       md5('legacy-concept-state-' || id::text), CURRENT_TIMESTAMP
FROM concept_states WHERE domain_id = ''
ON CONFLICT (source_table, source_key, reason) DO NOTHING;
ALTER TABLE concept_states ADD COLUMN enrollment_id TEXT;
ALTER TABLE concept_states ADD COLUMN formation_concept_id TEXT;
ALTER TABLE learning_sessions ADD COLUMN enrollment_id TEXT;
ALTER TABLE interactions ADD COLUMN enrollment_id TEXT;
ALTER TABLE interactions ADD COLUMN formation_concept_id TEXT;
ALTER TABLE assessment_attempts ADD COLUMN enrollment_id TEXT;
ALTER TABLE assessment_attempts ADD COLUMN formation_concept_id TEXT;
ALTER TABLE affect_states ADD COLUMN enrollment_id TEXT;
ALTER TABLE calibration_records ADD COLUMN enrollment_id TEXT;
ALTER TABLE transfer_records ADD COLUMN enrollment_id TEXT;
ALTER TABLE implementation_intentions ADD COLUMN enrollment_id TEXT;
ALTER TABLE pedagogical_snapshots ADD COLUMN enrollment_id TEXT;
ALTER TABLE narrative_objects ADD COLUMN enrollment_id TEXT;
ALTER TABLE narrative_mutations ADD COLUMN enrollment_id TEXT;
UPDATE concept_states row SET enrollment_id = mapping.enrollment_id,
       formation_concept_id = source.concept_id
FROM legacy_domain_enrollments mapping, legacy_concept_sources source
WHERE mapping.tenant_id = row.tenant_id AND mapping.learner_id = row.learner_id
  AND mapping.domain_id = row.domain_id
  AND source.tenant_id = row.tenant_id AND source.learner_id = row.learner_id
  AND source.enrollment_id = mapping.enrollment_id AND source.domain_id = row.domain_id
  AND source.concept_label = row.concept;
UPDATE learning_sessions row SET enrollment_id = mapping.enrollment_id
FROM legacy_domain_enrollments mapping
WHERE mapping.tenant_id = row.tenant_id AND mapping.learner_id = row.learner_id
  AND mapping.domain_id = COALESCE(row.domain_id, '');
UPDATE interactions row SET enrollment_id = mapping.enrollment_id,
       formation_concept_id = source.concept_id
FROM legacy_domain_enrollments mapping, legacy_concept_sources source
WHERE mapping.tenant_id = row.tenant_id AND mapping.learner_id = row.learner_id
  AND mapping.domain_id = COALESCE(row.domain_id, '')
  AND source.tenant_id = row.tenant_id AND source.learner_id = row.learner_id
  AND source.enrollment_id = mapping.enrollment_id
  AND source.domain_id = COALESCE(row.domain_id, '') AND source.concept_label = row.concept;
UPDATE assessment_attempts row SET enrollment_id = mapping.enrollment_id,
       formation_concept_id = source.concept_id
FROM legacy_domain_enrollments mapping, legacy_concept_sources source
WHERE mapping.tenant_id = row.tenant_id AND mapping.learner_id = row.learner_id
  AND mapping.domain_id = row.domain_id
  AND source.tenant_id = row.tenant_id AND source.learner_id = row.learner_id
  AND source.enrollment_id = mapping.enrollment_id AND source.domain_id = row.domain_id
  AND source.concept_label = row.concept_id;
UPDATE affect_states row SET enrollment_id = COALESCE((SELECT session.enrollment_id
    FROM learning_sessions session WHERE session.tenant_id = row.tenant_id
      AND session.learner_id = row.learner_id AND session.id = row.session_id),
    'legacy_recovery_enrollment_' || row.learner_id);
UPDATE calibration_records row SET enrollment_id = COALESCE((SELECT enrollment_id
    FROM legacy_domain_enrollments mapping WHERE mapping.tenant_id = row.tenant_id
      AND mapping.learner_id = row.learner_id AND mapping.domain_id = row.domain_id),
    'legacy_recovery_enrollment_' || row.learner_id);
UPDATE transfer_records row SET enrollment_id = COALESCE((SELECT enrollment_id
    FROM legacy_domain_enrollments mapping WHERE mapping.tenant_id = row.tenant_id
      AND mapping.learner_id = row.learner_id AND mapping.domain_id = row.domain_id),
    'legacy_recovery_enrollment_' || row.learner_id);
UPDATE implementation_intentions row SET enrollment_id = COALESCE((SELECT enrollment_id
    FROM legacy_domain_enrollments mapping WHERE mapping.tenant_id = row.tenant_id
      AND mapping.learner_id = row.learner_id AND mapping.domain_id = row.domain_id),
    'legacy_recovery_enrollment_' || row.learner_id);
UPDATE pedagogical_snapshots row SET enrollment_id = COALESCE((SELECT enrollment_id
    FROM legacy_domain_enrollments mapping WHERE mapping.tenant_id = row.tenant_id
      AND mapping.learner_id = row.learner_id AND mapping.domain_id = row.domain_id),
    'legacy_recovery_enrollment_' || row.learner_id);
UPDATE narrative_objects row SET enrollment_id = COALESCE((SELECT enrollment_id
    FROM legacy_domain_enrollments mapping WHERE mapping.tenant_id = row.tenant_id
      AND mapping.learner_id = row.learner_id AND mapping.domain_id = row.domain_id),
    'legacy_recovery_enrollment_' || row.learner_id);
UPDATE narrative_mutations row SET enrollment_id = COALESCE((SELECT enrollment_id
    FROM legacy_domain_enrollments mapping WHERE mapping.tenant_id = row.tenant_id
      AND mapping.learner_id = row.learner_id AND mapping.domain_id = row.domain_id),
    'legacy_recovery_enrollment_' || row.learner_id);
ALTER TABLE concept_states ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE concept_states ALTER COLUMN formation_concept_id SET NOT NULL;
ALTER TABLE learning_sessions ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE interactions ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE interactions ALTER COLUMN formation_concept_id SET NOT NULL;
ALTER TABLE assessment_attempts ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE assessment_attempts ALTER COLUMN formation_concept_id SET NOT NULL;
ALTER TABLE affect_states ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE calibration_records ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE transfer_records ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE implementation_intentions ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE pedagogical_snapshots ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE narrative_objects ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE narrative_mutations ALTER COLUMN enrollment_id SET NOT NULL;
ALTER TABLE concept_states ADD CONSTRAINT concept_states_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE concept_states ADD CONSTRAINT concept_states_formation_concept_fk
    FOREIGN KEY (tenant_id, formation_concept_id) REFERENCES formation_concepts(tenant_id, id) NOT VALID;
ALTER TABLE learning_sessions ADD CONSTRAINT learning_sessions_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE interactions ADD CONSTRAINT interactions_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE interactions ADD CONSTRAINT interactions_formation_concept_fk
    FOREIGN KEY (tenant_id, formation_concept_id) REFERENCES formation_concepts(tenant_id, id) NOT VALID;
ALTER TABLE assessment_attempts ADD CONSTRAINT assessment_attempts_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE assessment_attempts ADD CONSTRAINT assessment_attempts_formation_concept_fk
    FOREIGN KEY (tenant_id, formation_concept_id) REFERENCES formation_concepts(tenant_id, id) NOT VALID;
ALTER TABLE affect_states ADD CONSTRAINT affect_states_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE calibration_records ADD CONSTRAINT calibration_records_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE transfer_records ADD CONSTRAINT transfer_records_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE implementation_intentions ADD CONSTRAINT implementation_intentions_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE pedagogical_snapshots ADD CONSTRAINT pedagogical_snapshots_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE narrative_objects ADD CONSTRAINT narrative_objects_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE narrative_mutations ADD CONSTRAINT narrative_mutations_enrollment_fk
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id) NOT VALID;
ALTER TABLE concept_states VALIDATE CONSTRAINT concept_states_enrollment_fk;
ALTER TABLE concept_states VALIDATE CONSTRAINT concept_states_formation_concept_fk;
ALTER TABLE learning_sessions VALIDATE CONSTRAINT learning_sessions_enrollment_fk;
ALTER TABLE interactions VALIDATE CONSTRAINT interactions_enrollment_fk;
ALTER TABLE interactions VALIDATE CONSTRAINT interactions_formation_concept_fk;
ALTER TABLE assessment_attempts VALIDATE CONSTRAINT assessment_attempts_enrollment_fk;
ALTER TABLE assessment_attempts VALIDATE CONSTRAINT assessment_attempts_formation_concept_fk;
ALTER TABLE affect_states VALIDATE CONSTRAINT affect_states_enrollment_fk;
ALTER TABLE calibration_records VALIDATE CONSTRAINT calibration_records_enrollment_fk;
ALTER TABLE transfer_records VALIDATE CONSTRAINT transfer_records_enrollment_fk;
ALTER TABLE implementation_intentions VALIDATE CONSTRAINT implementation_intentions_enrollment_fk;
ALTER TABLE pedagogical_snapshots VALIDATE CONSTRAINT pedagogical_snapshots_enrollment_fk;
ALTER TABLE narrative_objects VALIDATE CONSTRAINT narrative_objects_enrollment_fk;
ALTER TABLE narrative_mutations VALIDATE CONSTRAINT narrative_mutations_enrollment_fk;
CREATE INDEX idx_concept_states_enrollment_concept ON concept_states(tenant_id, enrollment_id, formation_concept_id);
CREATE INDEX idx_interactions_enrollment_created ON interactions(tenant_id, enrollment_id, created_at);
CREATE INDEX idx_sessions_enrollment_started ON learning_sessions(tenant_id, enrollment_id, started_at);
CREATE INDEX idx_assessment_enrollment_concept ON assessment_attempts(tenant_id, enrollment_id, formation_concept_id);
CREATE OR REPLACE FUNCTION tutor_fill_concept_learning_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE label text; requested_domain text;
BEGIN
    requested_domain := COALESCE(NEW.domain_id, '');
    IF TG_TABLE_NAME = 'assessment_attempts' THEN label := NEW.concept_id; ELSE label := NEW.concept; END IF;
    IF NEW.enrollment_id IS NULL OR NEW.formation_concept_id IS NULL THEN
        SELECT mapping.enrollment_id, concept.concept_id
          INTO NEW.enrollment_id, NEW.formation_concept_id
        FROM legacy_domain_enrollments mapping
        JOIN legacy_concept_mappings concept
          ON concept.tenant_id = mapping.tenant_id
         AND concept.enrollment_id = mapping.enrollment_id
         AND concept.domain_id = mapping.domain_id
        WHERE mapping.tenant_id = NEW.tenant_id AND mapping.learner_id = NEW.learner_id
          AND mapping.domain_id = requested_domain AND concept.concept_label IN (label, '')
        ORDER BY CASE WHEN concept.concept_label = label THEN 0 ELSE 1 END LIMIT 1;
    END IF;
    IF NEW.enrollment_id IS NULL OR NEW.formation_concept_id IS NULL THEN
        SELECT mapping.enrollment_id, concept.concept_id
          INTO NEW.enrollment_id, NEW.formation_concept_id
        FROM legacy_domain_enrollments mapping
        JOIN legacy_concept_mappings concept
          ON concept.tenant_id = mapping.tenant_id AND concept.enrollment_id = mapping.enrollment_id
         AND concept.domain_id = '' AND concept.concept_label = ''
        WHERE mapping.tenant_id = NEW.tenant_id AND mapping.learner_id = NEW.learner_id
          AND mapping.domain_id = '' LIMIT 1;
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION tutor_fill_enrollment_learning_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.enrollment_id IS NULL THEN
        SELECT enrollment_id INTO NEW.enrollment_id FROM legacy_domain_enrollments
        WHERE tenant_id = NEW.tenant_id AND learner_id = NEW.learner_id
          AND domain_id = COALESCE(NEW.domain_id, '') LIMIT 1;
    END IF;
    IF NEW.enrollment_id IS NULL THEN
        SELECT enrollment_id INTO NEW.enrollment_id FROM legacy_domain_enrollments
        WHERE tenant_id = NEW.tenant_id AND learner_id = NEW.learner_id AND domain_id = '' LIMIT 1;
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION tutor_fill_session_learning_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.enrollment_id IS NULL THEN
        SELECT enrollment_id INTO NEW.enrollment_id FROM legacy_domain_enrollments
        WHERE tenant_id = NEW.tenant_id AND learner_id = NEW.learner_id
          AND domain_id = COALESCE(NEW.domain_id, '') LIMIT 1;
    END IF;
    IF NEW.enrollment_id IS NULL THEN
        SELECT enrollment_id INTO NEW.enrollment_id FROM legacy_domain_enrollments
        WHERE tenant_id = NEW.tenant_id AND learner_id = NEW.learner_id AND domain_id = '' LIMIT 1;
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION tutor_fill_affect_learning_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.enrollment_id IS NULL THEN
        SELECT enrollment_id INTO NEW.enrollment_id FROM learning_sessions
        WHERE tenant_id = NEW.tenant_id AND learner_id = NEW.learner_id AND id = NEW.session_id LIMIT 1;
    END IF;
    IF NEW.enrollment_id IS NULL THEN
        SELECT enrollment_id INTO NEW.enrollment_id FROM legacy_domain_enrollments
        WHERE tenant_id = NEW.tenant_id AND learner_id = NEW.learner_id AND domain_id = '' LIMIT 1;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER zz_concept_states_fill_learning_scope BEFORE INSERT ON concept_states
FOR EACH ROW EXECUTE FUNCTION tutor_fill_concept_learning_scope();
CREATE TRIGGER zz_interactions_fill_learning_scope BEFORE INSERT ON interactions
FOR EACH ROW EXECUTE FUNCTION tutor_fill_concept_learning_scope();
CREATE TRIGGER zz_assessment_attempts_fill_learning_scope BEFORE INSERT ON assessment_attempts
FOR EACH ROW EXECUTE FUNCTION tutor_fill_concept_learning_scope();
CREATE TRIGGER zz_learning_sessions_fill_learning_scope BEFORE INSERT ON learning_sessions
FOR EACH ROW EXECUTE FUNCTION tutor_fill_session_learning_scope();
CREATE TRIGGER zz_affect_states_fill_learning_scope BEFORE INSERT ON affect_states
FOR EACH ROW EXECUTE FUNCTION tutor_fill_affect_learning_scope();
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'calibration_records','transfer_records','implementation_intentions',
        'pedagogical_snapshots','narrative_objects','narrative_mutations'
    ] LOOP
        EXECUTE format('CREATE TRIGGER zz_fill_learning_scope BEFORE INSERT ON %I FOR EACH ROW EXECUTE FUNCTION tutor_fill_enrollment_learning_scope()', table_name);
    END LOOP;
END $$;
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['legacy_domain_enrollments','legacy_concept_sources','legacy_concept_mappings'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %1$I USING (tenant_id = current_setting(''app.current_tenant'', true)) WITH CHECK (tenant_id = current_setting(''app.current_tenant'', true))', table_name);
    END LOOP;
END $$;`,
	},
	{
		Version: "postgres_0042_saas_runtime_control_plane",
		Body:    postgresSaaSControlPlaneMigration,
	},
	{
		Version: "postgres_0043_service_account_credentials",
		Body:    postgresServiceAccountCredentialMigration,
	},
	{
		Version: "postgres_0044_enrollment_learning_state",
		Body:    postgresEnrollmentLearningStateMigration,
	},
	{
		Version: "postgres_0045_identity_federation",
		Body:    postgresIdentityFederationMigration,
	},
	{
		Version: "postgres_0046_support_access",
		Body:    postgresSupportAccessMigration,
	},
	{
		Version: "postgres_0047_catalog_admin_api",
		Body:    postgresCatalogAdminMigration,
	},
	{
		Version: "postgres_0048_shared_oauth_csrf",
		Body:    postgresOAuthCSRFMigration,
	},
	{
		Version: "postgres_0049_worker_tenant_runs",
		Body:    postgresWorkerTenantMigration,
	},
	{
		Version: "postgres_0050_tenant_integration_secret_history",
		Body:    postgresTenantIntegrationSecretHistoryMigration,
	},
	{
		Version: "postgres_0051_saas_commercial_invariants",
		Body:    postgresSaaSCommercialMigration,
	},
	{
		Version: "postgres_0052_saas_governance",
		Body:    postgresSaaSGovernanceMigration,
	},
	{
		Version: "postgres_0053_canonical_narrative_keys",
		Body:    postgresCanonicalNarrativeKeyMigration,
	},
	{
		Version: "postgres_0054_platform_audit",
		Body:    postgresPlatformAuditMigration,
	},
}

// OpenPostgres opens a PostgreSQL-backed *sql.DB via the pgx database/sql
// driver and configures the connection pool for a networked, MVCC database
// (unlike the single-writer SQLite path, Postgres handles concurrent writers).
// maxConns bounds the pool; pass <= 0 for a sensible default.
func OpenPostgres(dsn string, maxConns int) (*sql.DB, error) {
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if maxConns <= 0 {
		maxConns = 10
	}
	d.SetMaxOpenConns(maxConns)
	d.SetMaxIdleConns(2)
	d.SetConnMaxLifetime(30 * time.Minute)
	d.SetConnMaxIdleTime(5 * time.Minute)
	return d, nil
}

// MigratePostgres applies the consolidated Postgres schema. Every statement is
// idempotent (IF NOT EXISTS), so it is safe to call on every startup. database/
// sql's extended protocol runs one command per Exec, so the schema is split on
// statement boundaries.
func MigratePostgres(ctx context.Context, d *sql.DB) error {
	// Serialize migration across the fleet. CREATE TABLE IF NOT EXISTS is not
	// atomic against the pg_type catalog, so two instances booting at once race
	// and one hits 23505 (duplicate key on pg_type_typname_nsp_index). A
	// session-level advisory lock, held on a single pinned connection for the
	// whole apply, makes migration single-flight: the loser waits, then its
	// IF NOT EXISTS statements are no-ops.
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("postgres migrate: acquire conn: %w", err)
	}
	defer conn.Close()
	const migrateLockKey = 0x7E50_0001 // arbitrary constant, stable across instances
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("postgres migrate: lock: %w", err)
	}
	defer conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrateLockKey)

	// Anti-drift guard, mirroring the SQLite migration runner: record the
	// SHA-256 of the embedded schema in schema_migrations on first apply, and
	// on subsequent boots refuse to proceed if it changed. The CREATE ... IF
	// NOT EXISTS body cannot ALTER an existing table, so a changed schema_pg.sql
	// would otherwise silently no-op against an already-migrated database and
	// the deployment would diverge from the schema file undetected.
	const schemaVersion = "postgres_schema"
	sum := sha256.Sum256([]byte(postgresSchema))
	current := hex.EncodeToString(sum[:])

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
)`); err != nil {
		return fmt.Errorf("postgres migrate: ensure schema_migrations: %w", err)
	}

	var stored string
	switch err := conn.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE version = $1`, schemaVersion,
	).Scan(&stored); err {
	case nil:
		if stored != current {
			return fmt.Errorf(
				"postgres schema checksum mismatch: stored=%s current=%s "+
					"(schema_pg.sql changed since it was applied; manual migration required)",
				stored, current,
			)
		}
		// Original schema is present and unchanged. Continue with ordered
		// incremental migrations below.
	case sql.ErrNoRows:
		for _, stmt := range splitSQLStatements(postgresSchema) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply postgres schema (%.60s...): %w", stmt, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)
			 ON CONFLICT (version) DO NOTHING`,
			schemaVersion, current,
		); err != nil {
			return fmt.Errorf("postgres migrate: record checksum: %w", err)
		}
	default:
		return fmt.Errorf("postgres migrate: read schema_migrations: %w", err)
	}

	if err := applyPostgresMigrations(ctx, conn, postgresMigrations); err != nil {
		return fmt.Errorf("postgres migrate: %w", err)
	}
	return nil
}

// VerifyPostgresSchemaCurrent checks the immutable ledger without acquiring
// migration locks or issuing DDL. API and worker startup use this fail-closed
// compatibility gate.
func VerifyPostgresSchemaCurrent(ctx context.Context, d *sql.DB) error {
	const schemaVersion = "postgres_schema"
	sum := sha256.Sum256([]byte(postgresSchema))
	expectedSchema := hex.EncodeToString(sum[:])
	var current string
	if err := d.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = $1`, schemaVersion).Scan(&current); err != nil {
		return fmt.Errorf("schema compatibility: PostgreSQL base schema is missing: %w", err)
	}
	if current != expectedSchema {
		return fmt.Errorf("schema compatibility: PostgreSQL base schema checksum mismatch")
	}
	for _, item := range postgresMigrations {
		if err := d.QueryRowContext(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = $1`, item.Version).Scan(&current); err != nil {
			return fmt.Errorf("schema compatibility: migration %s is missing: %w", item.Version, err)
		}
		if current != item.checksum() {
			return fmt.Errorf("schema compatibility: migration %s checksum mismatch", item.Version)
		}
	}
	return nil
}

func applyPostgresMigrations(ctx context.Context, conn *sql.Conn, migrations []migration) error {
	for _, m := range migrations {
		sum := sha256.Sum256([]byte(m.Body))
		checksum := hex.EncodeToString(sum[:])

		var stored string
		err := conn.QueryRowContext(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = $1`,
			m.Version,
		).Scan(&stored)
		switch err {
		case nil:
			if stored != checksum {
				return fmt.Errorf("migration %s checksum mismatch: stored=%s current=%s", m.Version, stored, checksum)
			}
			continue
		case sql.ErrNoRows:
			// Apply below.
		default:
			return fmt.Errorf("read migration %s: %w", m.Version, err)
		}

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.Version, err)
		}
		for _, stmt := range splitSQLStatements(m.Body) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply migration %s (%.60s...): %w", m.Version, stmt, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
			m.Version, checksum,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Version, err)
		}
	}
	return nil
}

// splitSQLStatements splits PostgreSQL migration text without cutting quoted
// strings, identifiers, comments, or dollar-quoted PL/pgSQL function bodies.
func splitSQLStatements(sqlText string) []string {
	var out []string
	start := 0
	inSingle, inDouble, inLineComment, inBlockComment := false, false, false, false
	dollarTag := ""
	for i := 0; i < len(sqlText); i++ {
		if dollarTag != "" {
			if strings.HasPrefix(sqlText[i:], dollarTag) {
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		}
		if inLineComment {
			if sqlText[i] == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if sqlText[i] == '*' && i+1 < len(sqlText) && sqlText[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inSingle {
			if sqlText[i] == '\'' {
				if i+1 < len(sqlText) && sqlText[i+1] == '\'' {
					i++
				} else {
					inSingle = false
				}
			}
			continue
		}
		if inDouble {
			if sqlText[i] == '"' {
				if i+1 < len(sqlText) && sqlText[i+1] == '"' {
					i++
				} else {
					inDouble = false
				}
			}
			continue
		}

		switch {
		case sqlText[i] == '-' && i+1 < len(sqlText) && sqlText[i+1] == '-':
			inLineComment = true
			i++
		case sqlText[i] == '/' && i+1 < len(sqlText) && sqlText[i+1] == '*':
			inBlockComment = true
			i++
		case sqlText[i] == '\'':
			inSingle = true
		case sqlText[i] == '"':
			inDouble = true
		case sqlText[i] == '$':
			if tag := postgresDollarTag(sqlText[i:]); tag != "" {
				dollarTag = tag
				i += len(tag) - 1
			}
		case sqlText[i] == ';':
			if stmt := strings.TrimSpace(sqlText[start:i]); stmt != "" {
				out = append(out, stmt)
			}
			start = i + 1
		}
	}
	if stmt := strings.TrimSpace(sqlText[start:]); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

func postgresDollarTag(s string) string {
	if len(s) < 2 || s[0] != '$' {
		return ""
	}
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '$':
			return s[:i+1]
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 1 && c >= '0' && c <= '9':
			continue
		default:
			return ""
		}
	}
	return ""
}
