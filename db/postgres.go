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
