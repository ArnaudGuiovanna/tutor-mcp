// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// migration is a single versioned schema change. Body is the SQL executed when
// the migration is first applied; Checksum is computed from Body and persisted
// in schema_migrations to detect drift on subsequent startups.
//
// IgnoreExecErrors is set for ALTER-style migrations whose statements may
// already have been applied to a pre-existing database (e.g. "duplicate column"
// from sqlite). For those, errors during Exec are intentionally swallowed so
// the migration can still be recorded as applied.
type migration struct {
	Version          string
	Body             string
	IgnoreExecErrors bool
}

// checksum returns the lowercase hex SHA-256 of the migration body. Whitespace
// is preserved deliberately so editing a body — even cosmetically — surfaces as
// a checksum mismatch on the next startup. Operators who knowingly want to
// rewrite history must update the row in schema_migrations themselves.
func (m migration) checksum() string {
	sum := sha256.Sum256([]byte(m.Body))
	return hex.EncodeToString(sum[:])
}

type migrationTx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func ensureSchemaMigrationsTableInTx(ctx context.Context, tx migrationTx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

// applyMigrationInTx applies one immutable migration or verifies the checksum
// recorded by a previous run. The caller owns the surrounding transaction.
func applyMigrationInTx(ctx context.Context, tx migrationTx, m migration) error {
	var storedChecksum string
	row := tx.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = ?`, m.Version)
	switch err := row.Scan(&storedChecksum); err {
	case nil:
		if storedChecksum != m.checksum() {
			return fmt.Errorf(
				"schema_migrations: checksum mismatch for version %q: stored=%s current=%s "+
					"(migration body changed since it was applied; manual intervention required)",
				m.Version, storedChecksum, m.checksum(),
			)
		}
		return nil
	case sql.ErrNoRows:
		// fall through to apply
	default:
		return fmt.Errorf("schema_migrations: read version %q: %w", m.Version, err)
	}

	if _, err := tx.ExecContext(ctx, `SAVEPOINT migration_body`); err != nil {
		return fmt.Errorf("start migration savepoint %q: %w", m.Version, err)
	}
	if _, execErr := tx.ExecContext(ctx, m.Body); execErr != nil {
		if err := rollbackMigrationBody(ctx, tx, m.Version, execErr); err != nil {
			return err
		}
		if !m.IgnoreExecErrors {
			return fmt.Errorf("apply migration %q: %w", m.Version, execErr)
		}
		// IgnoreExecErrors covers ALTERs already applied on legacy DBs
		// ("duplicate column name", "no such column" on DROP COLUMN,
		// etc.). Roll back any partial body effects, then record the
		// bookkeeping row in the still-open transaction — subsequent runs
		// then take the "checksum already matches, skip" branch.
	} else if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT migration_body`); err != nil {
		return fmt.Errorf("release migration savepoint %q: %w", m.Version, err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO schema_migrations (version, checksum) VALUES (?, ?)`,
		m.Version, m.checksum(),
	); err != nil {
		return fmt.Errorf("record migration %q: %w", m.Version, err)
	}
	return nil
}

func rollbackMigrationBody(ctx context.Context, tx migrationTx, version string, cause error) error {
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT migration_body`); err != nil {
		return fmt.Errorf("rollback migration savepoint %q after %v: %w", version, cause, err)
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT migration_body`); err != nil {
		return fmt.Errorf("release migration savepoint %q after rollback from %v: %w", version, cause, err)
	}
	return nil
}

// buildMigrations assembles the ordered migration list from the embedded
// schema.sql, the historical ALTER list, the BKT data migration, and the
// idempotent CREATE TABLE/INDEX list. The order here is the canonical apply
// order; appending new entries is safe, reordering is not.
func buildMigrations() []migration {
	out := make([]migration, 0, 2+len(alterMigrations)+1+len(idempotentMigrations)+3)
	out = append(out, migration{
		Version: "0001_base_schema",
		Body:    schemaSQL,
	})
	for i, body := range alterMigrations {
		out = append(out, migration{
			Version: fmt.Sprintf("0002_alter_%03d_%s", i+1, alterShortName(body)),
			Body:    body,
			// ALTERs may already be present on legacy DBs that ran the old
			// "ignore errors" migrator. Swallow per-statement errors so the
			// row still gets recorded.
			IgnoreExecErrors: true,
		})
	}
	out = append(out, migration{
		Version: "0003_data_plearn_default_0_15",
		Body:    `UPDATE concept_states SET p_learn = 0.15 WHERE p_learn = 0.3`,
	})
	// Issue #61: scrub the legacy `level`, `background`, `learning_style`
	// keys out of profile_json. The tool no longer accepts them so the
	// re-introduction surface is closed; json_remove is idempotent.
	out = append(out, migration{
		Version: "0003_data_drop_legacy_learner_profile_fields",
		Body: `UPDATE learners
		 SET profile_json = json_remove(profile_json, '$.level', '$.background', '$.learning_style')
		 WHERE profile_json IS NOT NULL
		   AND profile_json != ''
		   AND (json_extract(profile_json, '$.level') IS NOT NULL
		     OR json_extract(profile_json, '$.background') IS NOT NULL
		     OR json_extract(profile_json, '$.learning_style') IS NOT NULL)`,
	})
	for i, body := range idempotentMigrations {
		out = append(out, migration{
			Version: fmt.Sprintf("0004_idempotent_%03d_%s", i+1, alterShortName(body)),
			Body:    body,
		})
	}
	out = append(out, migration{
		Version:          "0005_alter_pedagogical_snapshots_interpretation_brief",
		Body:             `ALTER TABLE pedagogical_snapshots ADD COLUMN interpretation_brief TEXT NOT NULL DEFAULT ''`,
		IgnoreExecErrors: true,
	})
	out = append(out, migration{
		Version: "0006_create_pending_consolidations",
		Body: `CREATE TABLE IF NOT EXISTS pending_consolidations (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    learner_id     TEXT    NOT NULL,
    period_type    TEXT    NOT NULL CHECK (period_type IN ('monthly','quarterly','annual')),
    period_key     TEXT    NOT NULL,
    status         TEXT    NOT NULL CHECK (status IN ('pending','delivered','completed','failed')) DEFAULT 'pending',
    detected_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    delivered_at   TIMESTAMP,
    completed_at   TIMESTAMP,
    UNIQUE(learner_id, period_type, period_key)
)`,
	})
	out = append(out, migration{
		Version: "0007_index_pending_consolidations_learner_status",
		Body:    `CREATE INDEX IF NOT EXISTS idx_pending_consolidations_learner_status ON pending_consolidations(learner_id, status)`,
	})
	// R001: persist learner consent for an OAuth client + redirect_uri so
	// returning logins don't re-prompt and the consent screen stays
	// meaningful (i.e. only shown when there's genuinely something to
	// approve). Keyed on (learner_id, client_id, redirect_uri) — a new
	// redirect_uri re-prompts even for the same (learner, client) pair.
	out = append(out, migration{
		Version: "0008_create_learner_approved_clients",
		Body: `CREATE TABLE IF NOT EXISTS learner_approved_clients (
    learner_id   TEXT     NOT NULL REFERENCES learners(id),
    client_id    TEXT     NOT NULL REFERENCES oauth_clients(client_id),
    redirect_uri TEXT     NOT NULL,
    approved_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (learner_id, client_id, redirect_uri)
)`,
	})
	// R002: fold existing learner emails to lowercase so case-variants
	// share a single row. If two rows differ only by case (e.g. legacy
	// data created when the handler accepted both Bob@x.com and
	// bob@x.com), this UPDATE fails on the existing UNIQUE(email)
	// constraint with "UNIQUE constraint failed: learners.email" — the
	// operator must resolve the duplicate manually before Migrate can
	// re-run. We refuse to guess which row "wins".
	out = append(out, migration{
		Version: "0009_lowercase_learner_emails",
		Body:    `UPDATE learners SET email = lower(email) WHERE email != lower(email)`,
	})
	// R002: defence-in-depth against direct-DB inserts that skip the
	// handler-side NormalizeEmail. A functional UNIQUE index on lower(email)
	// rejects any future row whose case-folded form collides with an
	// existing learner, independently of how it arrives.
	out = append(out, migration{
		Version: "0010_index_learners_email_lower",
		Body:    `CREATE UNIQUE INDEX IF NOT EXISTS idx_learners_email_lower ON learners(lower(email))`,
	})
	// Concept labels are only identities inside a domain. Rebuild the SQLite
	// table because its historical UNIQUE(learner_id, concept) constraint
	// cannot be dropped in place. Existing progress is copied to every active
	// domain graph that contains the label; states with no matching domain are
	// retained under the legacy empty domain for operator visibility.
	out = append(out, migration{
		Version: "0011_scope_concept_states_by_domain",
		Body: `CREATE TABLE concept_states_domain_scoped (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    domain_id      TEXT NOT NULL DEFAULT '',
    concept        TEXT NOT NULL,
    stability      REAL DEFAULT 1.0,
    difficulty     REAL DEFAULT 0.3,
    elapsed_days   INTEGER DEFAULT 0,
    scheduled_days INTEGER DEFAULT 1,
    reps           INTEGER DEFAULT 0,
    lapses         INTEGER DEFAULT 0,
    card_state     TEXT DEFAULT 'new',
    last_review    DATETIME,
    next_review    DATETIME,
    p_mastery      REAL DEFAULT 0.1,
    p_learn        REAL DEFAULT 0.15,
    p_forget       REAL DEFAULT 0.05,
    p_slip         REAL DEFAULT 0.1,
    p_guess        REAL DEFAULT 0.2,
    theta          REAL DEFAULT 0.0,
    updated_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(learner_id, domain_id, concept)
);
INSERT INTO concept_states_domain_scoped
    (learner_id, domain_id, concept, stability, difficulty, elapsed_days,
     scheduled_days, reps, lapses, card_state, last_review, next_review,
     p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
SELECT cs.learner_id, d.id, cs.concept, cs.stability, cs.difficulty,
       cs.elapsed_days, cs.scheduled_days, cs.reps, cs.lapses, cs.card_state,
       cs.last_review, cs.next_review, cs.p_mastery, cs.p_learn, cs.p_forget,
       cs.p_slip, cs.p_guess, cs.theta, cs.updated_at
FROM concept_states cs
JOIN domains d ON d.learner_id = cs.learner_id
WHERE EXISTS (
    SELECT 1
    FROM json_each(
        CASE WHEN json_valid(d.graph_json) THEN d.graph_json
             ELSE '{"concepts":[]}' END,
        '$.concepts'
    ) AS concept_json
    WHERE concept_json.value = cs.concept
);
INSERT INTO concept_states_domain_scoped
    (learner_id, domain_id, concept, stability, difficulty, elapsed_days,
     scheduled_days, reps, lapses, card_state, last_review, next_review,
     p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
SELECT cs.learner_id, '', cs.concept, cs.stability, cs.difficulty,
       cs.elapsed_days, cs.scheduled_days, cs.reps, cs.lapses, cs.card_state,
       cs.last_review, cs.next_review, cs.p_mastery, cs.p_learn, cs.p_forget,
       cs.p_slip, cs.p_guess, cs.theta, cs.updated_at
FROM concept_states cs
WHERE NOT EXISTS (
    SELECT 1
    FROM domains d,
         json_each(
             CASE WHEN json_valid(d.graph_json) THEN d.graph_json
                  ELSE '{"concepts":[]}' END,
             '$.concepts'
         ) AS concept_json
    WHERE d.learner_id = cs.learner_id
      AND concept_json.value = cs.concept
);
DROP TABLE concept_states;
ALTER TABLE concept_states_domain_scoped RENAME TO concept_states;
CREATE INDEX idx_concept_states_learner
    ON concept_states(learner_id, domain_id);
CREATE INDEX idx_concept_states_review
    ON concept_states(learner_id, domain_id, next_review)`,
	})
	out = append(out, migration{
		Version: "0012_scope_metacognitive_records_by_domain",
		Body: `ALTER TABLE calibration_records
    ADD COLUMN domain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE transfer_records
    ADD COLUMN domain_id TEXT NOT NULL DEFAULT '';
DROP INDEX IF EXISTS idx_transfer_records_learner_concept;
CREATE INDEX idx_transfer_records_learner_concept
    ON transfer_records(learner_id, domain_id, concept_id, created_at);
DROP INDEX IF EXISTS idx_calibration_records_learner;
CREATE INDEX idx_calibration_records_learner
    ON calibration_records(learner_id, domain_id, created_at)`,
	})
	// Preserve legacy evidence when its domain can be inferred without
	// ambiguity. Shared concept labels deliberately remain unassigned: guessing
	// would recreate the cross-domain contamination these migrations remove.
	out = append(out, migration{
		Version: "0013_backfill_unique_domain_identity",
		Body: `UPDATE interactions
SET domain_id = (
    SELECT MIN(d.id)
    FROM domains d,
         json_each(
             CASE WHEN json_valid(d.graph_json) THEN d.graph_json
                  ELSE '{"concepts":[]}' END,
             '$.concepts'
         ) AS concept_json
    WHERE d.learner_id = interactions.learner_id
      AND concept_json.value = interactions.concept
)
WHERE COALESCE(domain_id, '') = ''
  AND 1 = (
      SELECT COUNT(DISTINCT d.id)
      FROM domains d,
           json_each(
               CASE WHEN json_valid(d.graph_json) THEN d.graph_json
                    ELSE '{"concepts":[]}' END,
               '$.concepts'
           ) AS concept_json
      WHERE d.learner_id = interactions.learner_id
        AND concept_json.value = interactions.concept
  );
UPDATE calibration_records
SET domain_id = (
    SELECT MIN(d.id)
    FROM domains d,
         json_each(
             CASE WHEN json_valid(d.graph_json) THEN d.graph_json
                  ELSE '{"concepts":[]}' END,
             '$.concepts'
         ) AS concept_json
    WHERE d.learner_id = calibration_records.learner_id
      AND concept_json.value = calibration_records.concept_id
)
WHERE domain_id = ''
  AND 1 = (
      SELECT COUNT(DISTINCT d.id)
      FROM domains d,
           json_each(
               CASE WHEN json_valid(d.graph_json) THEN d.graph_json
                    ELSE '{"concepts":[]}' END,
               '$.concepts'
           ) AS concept_json
      WHERE d.learner_id = calibration_records.learner_id
        AND concept_json.value = calibration_records.concept_id
  );
UPDATE transfer_records
SET domain_id = (
    SELECT MIN(d.id)
    FROM domains d,
         json_each(
             CASE WHEN json_valid(d.graph_json) THEN d.graph_json
                  ELSE '{"concepts":[]}' END,
             '$.concepts'
         ) AS concept_json
    WHERE d.learner_id = transfer_records.learner_id
      AND concept_json.value = transfer_records.concept_id
)
WHERE domain_id = ''
  AND 1 = (
      SELECT COUNT(DISTINCT d.id)
      FROM domains d,
           json_each(
               CASE WHEN json_valid(d.graph_json) THEN d.graph_json
                    ELSE '{"concepts":[]}' END,
               '$.concepts'
           ) AS concept_json
      WHERE d.learner_id = transfer_records.learner_id
        AND concept_json.value = transfer_records.concept_id
  )`,
	})
	// Webhook delivery claims must survive beyond the short transaction that
	// selects a queue row.  A dedicated timestamp lets the hourly cleanup
	// recover workers that crashed after claiming but before completing the
	// outbound HTTP request.
	out = append(out, migration{
		Version: "0014_webhook_queue_processing_claim",
		Body:    `ALTER TABLE webhook_message_queue ADD COLUMN claimed_at DATETIME`,
	})
	// Refresh-token rotation must retain the consumed credential so a replay
	// can identify and revoke its still-active descendant. Existing rows become
	// one-token families and remain usable until their first rotation.
	out = append(out, migration{
		Version: "0015_refresh_token_families",
		Body: `ALTER TABLE refresh_tokens ADD COLUMN family_id TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens ADD COLUMN used_at DATETIME;
ALTER TABLE refresh_tokens ADD COLUMN revoked_at DATETIME;
UPDATE refresh_tokens SET family_id = token WHERE family_id = '';
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family ON refresh_tokens(family_id)`,
	})
	// Bind newly-issued authorization codes to the exact redirect URI and PKCE
	// method. Empty defaults preserve redemption of codes minted before upgrade.
	out = append(out, migration{
		Version: "0016_bind_oauth_codes_redirect_pkce",
		Body: `ALTER TABLE oauth_codes ADD COLUMN code_challenge_method TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_codes ADD COLUMN redirect_uri TEXT NOT NULL DEFAULT ''`,
	})
	// Global TTL cleanup queries do not filter by account/bucket first, so they
	// need timestamp-leading indexes rather than the account-scoped read index.
	out = append(out, migration{
		Version: "0017_index_security_state_ttl",
		Body: `CREATE INDEX IF NOT EXISTS idx_rate_limit_buckets_updated_at ON rate_limit_buckets(updated_at);
CREATE INDEX IF NOT EXISTS idx_login_failures_attempted_at ON login_failures(attempted_at)`,
	})
	// A learning episode is an explicit durable entity. The partial unique
	// index is the concurrency guard that makes open/resume converge on one
	// canonical row per learner. Historical events are not guessed into
	// sessions: their correlation remains NULL/legacy unless it was explicit.
	out = append(out, migration{
		Version: "0018_create_learning_sessions",
		Body: `CREATE TABLE learning_sessions (
    id             TEXT PRIMARY KEY,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    domain_id      TEXT REFERENCES domains(id) ON DELETE SET NULL,
    status         TEXT NOT NULL CHECK (status IN ('open','closed')) DEFAULT 'open',
    started_at     DATETIME NOT NULL,
    last_active_at DATETIME NOT NULL,
    closed_at      DATETIME,
    CHECK ((status = 'open' AND closed_at IS NULL) OR
           (status = 'closed' AND closed_at IS NOT NULL))
);
CREATE UNIQUE INDEX idx_learning_sessions_one_open
    ON learning_sessions(learner_id) WHERE status = 'open';
CREATE INDEX idx_learning_sessions_learner_started
    ON learning_sessions(learner_id, started_at DESC)`,
	})
	out = append(out, migration{
		Version: "0019_link_interactions_to_sessions",
		Body: `ALTER TABLE interactions
    ADD COLUMN session_id TEXT REFERENCES learning_sessions(id);
CREATE INDEX idx_interactions_learner_session
    ON interactions(learner_id, session_id, created_at)`,
	})
	// Preserve the nullable honored flag for old API readers while making the
	// lifecycle explicit and one-way for new flows. A legacy false is a missed
	// commitment, true is honored, and NULL remains pending. session_id stays
	// NULL where the historical association cannot be proven.
	out = append(out, migration{
		Version: "0020_intention_lifecycle_and_session",
		Body: `ALTER TABLE implementation_intentions
    ADD COLUMN session_id TEXT REFERENCES learning_sessions(id);
ALTER TABLE implementation_intentions
    ADD COLUMN status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','honored','missed','cancelled'));
ALTER TABLE implementation_intentions ADD COLUMN resolved_at DATETIME;
ALTER TABLE implementation_intentions ADD COLUMN updated_at DATETIME;
UPDATE implementation_intentions
SET status = CASE
        WHEN honored = 1 THEN 'honored'
        WHEN honored = 0 THEN 'missed'
        ELSE 'pending'
    END,
    resolved_at = CASE WHEN honored IS NOT NULL THEN created_at ELSE NULL END,
    updated_at = created_at;
CREATE INDEX idx_impl_intent_learner_status
    ON implementation_intentions(learner_id, status, created_at DESC);
CREATE INDEX idx_impl_intent_session
    ON implementation_intentions(session_id)`,
	})
	// Assessment evidence is a three-stage state machine: immutable task/rubric,
	// committed learner response, then derived evaluation. Host-reported legacy
	// interactions remain readable but carry no assessment_attempt_id and cannot
	// independently establish demonstrated mastery.
	out = append(out, migration{
		Version: "0021_create_assessment_attempts",
		Body: `CREATE TABLE assessment_attempts (
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
    passing_score              REAL NOT NULL CHECK (passing_score > 0),
    status                     TEXT NOT NULL CHECK (status IN ('prepared','submitted','evaluated','cancelled')),
    rubric_score_json          TEXT,
    score                      REAL NOT NULL DEFAULT 0,
    passed                     INTEGER NOT NULL DEFAULT 0 CHECK (passed IN (0,1)),
    evaluator_id               TEXT,
    evaluation_method          TEXT CHECK (evaluation_method IS NULL OR evaluation_method IN ('host_llm','external_service','human_review','deterministic')),
    evaluation_provenance_json TEXT,
    trusted_evaluation         INTEGER NOT NULL DEFAULT 0 CHECK (trusted_evaluation IN (0,1)),
    created_at                 DATETIME NOT NULL,
    submitted_at               DATETIME,
    evaluated_at               DATETIME,
    cancelled_at               DATETIME,
    CHECK (task_text IS NOT NULL OR task_content_hash <> ''),
    CHECK (status <> 'submitted' OR submitted_at IS NOT NULL),
    CHECK (status <> 'evaluated' OR (submitted_at IS NOT NULL AND evaluated_at IS NOT NULL)),
    CHECK (status <> 'cancelled' OR cancelled_at IS NOT NULL)
);
CREATE INDEX idx_assessment_attempts_learning_evidence
    ON assessment_attempts(learner_id, domain_id, concept_id, trusted_evaluation, passed, evaluated_at DESC);
CREATE INDEX idx_assessment_attempts_session
    ON assessment_attempts(learner_id, session_id, created_at);
ALTER TABLE interactions
    ADD COLUMN assessment_attempt_id TEXT REFERENCES assessment_attempts(id);
CREATE UNIQUE INDEX idx_interactions_assessment_attempt
    ON interactions(assessment_attempt_id) WHERE assessment_attempt_id IS NOT NULL;
ALTER TABLE transfer_records
    ADD COLUMN assessment_attempt_id TEXT REFERENCES assessment_attempts(id);
CREATE INDEX idx_transfer_records_assessment_attempt
    ON transfer_records(assessment_attempt_id)`,
	})
	// A close retry must not duplicate the if-then commitment captured for the
	// same durable episode. Legacy rows remain unconstrained because their
	// session_id is NULL.
	out = append(out, migration{
		Version: "0022_one_intention_per_session",
		Body: `CREATE UNIQUE INDEX idx_impl_intent_one_per_session
    ON implementation_intentions(session_id)
    WHERE session_id IS NOT NULL`,
	})
	// Learner-controlled delivery policy. Existing rows deliberately default to
	// no notification consent: a historical webhook URL is not proof of opt-in.
	// High-stakes classification is one-way on the public MCP surface and gates
	// demonstrated claims/proactive suggestions on trusted human review.
	out = append(out, migration{
		Version: "0023_availability_accessibility_high_stakes",
		Body: `ALTER TABLE availability ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';
ALTER TABLE availability ADD COLUMN notification_consent INTEGER NOT NULL DEFAULT 0 CHECK (notification_consent IN (0,1));
ALTER TABLE availability ADD COLUMN notification_frequency TEXT NOT NULL DEFAULT 'daily' CHECK (notification_frequency IN ('as_scheduled','daily','weekly'));
ALTER TABLE availability ADD COLUMN max_notifications_per_day INTEGER NOT NULL DEFAULT 1 CHECK (max_notifications_per_day BETWEEN 1 AND 10);
ALTER TABLE availability ADD COLUMN accessibility_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE availability ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1);
ALTER TABLE availability ADD COLUMN updated_at DATETIME;
ALTER TABLE domains ADD COLUMN high_stakes INTEGER NOT NULL DEFAULT 0 CHECK (high_stakes IN (0,1));
CREATE INDEX idx_domains_learner_high_stakes ON domains(learner_id, high_stakes);
CREATE INDEX idx_assessment_attempts_human_review ON assessment_attempts(learner_id, domain_id, evaluation_method, trusted_evaluation, status)`,
	})
	// Domain deletion is a logical tombstone. Physical deletion is incompatible
	// with immutable curriculum/audit rows and with assessment evidence that
	// references the domain. Readers hide tombstoned rows while their identity
	// remains available to historical foreign keys.
	out = append(out, migration{
		Version: "0024_tombstone_domains",
		Body: `ALTER TABLE domains ADD COLUMN deleted_at DATETIME;
CREATE INDEX idx_domains_learner_deleted ON domains(learner_id, deleted_at)`,
	})
	// A curriculum version is a complete append-only snapshot. Concept IDs and
	// stable keys live in their own immutable registry; labels and metadata can
	// change only by publishing a new snapshot. No FK points from these tables
	// to domains so tombstoned domain history remains independently auditable.
	out = append(out, migration{
		Version: "0025_create_immutable_curriculum",
		Body: `CREATE TABLE curriculum_versions (
    domain_id       TEXT NOT NULL,
    learner_id      TEXT NOT NULL,
    version         INTEGER NOT NULL CHECK (version >= 1),
    parent_version  INTEGER,
    snapshot_json   TEXT NOT NULL CHECK (json_valid(snapshot_json)),
    operation_type  TEXT NOT NULL CHECK (operation_type IN ('create','baseline_import','add','rename','update_metadata','split','merge','remove','legacy_graph_update')),
    operation_json  TEXT NOT NULL CHECK (json_valid(operation_json)),
    provenance_json TEXT NOT NULL CHECK (json_valid(provenance_json)),
    review_json     TEXT NOT NULL CHECK (json_valid(review_json)),
    created_by      TEXT NOT NULL,
    created_at      DATETIME NOT NULL,
    PRIMARY KEY (domain_id, version),
    FOREIGN KEY (domain_id, parent_version)
        REFERENCES curriculum_versions(domain_id, version)
);
CREATE TABLE curriculum_concepts (
    id         TEXT PRIMARY KEY,
    domain_id  TEXT NOT NULL,
    learner_id TEXT NOT NULL,
    stable_key TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    UNIQUE (domain_id, stable_key)
);
CREATE TABLE curriculum_metadata_ids (
    id         TEXT PRIMARY KEY,
    concept_id TEXT NOT NULL REFERENCES curriculum_concepts(id),
    domain_id  TEXT NOT NULL,
    learner_id TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('outcome','criterion')),
    created_at DATETIME NOT NULL
);
CREATE INDEX idx_curriculum_versions_learner_domain
    ON curriculum_versions(learner_id, domain_id, version DESC);
CREATE INDEX idx_curriculum_concepts_learner_domain
    ON curriculum_concepts(learner_id, domain_id);
CREATE INDEX idx_curriculum_metadata_learner_domain
    ON curriculum_metadata_ids(learner_id, domain_id);
CREATE TRIGGER curriculum_versions_no_update
BEFORE UPDATE ON curriculum_versions
BEGIN SELECT RAISE(ABORT, 'curriculum versions are immutable'); END;
CREATE TRIGGER curriculum_versions_no_delete
BEFORE DELETE ON curriculum_versions
BEGIN SELECT RAISE(ABORT, 'curriculum versions are immutable'); END;
CREATE TRIGGER curriculum_concepts_no_update
BEFORE UPDATE ON curriculum_concepts
BEGIN SELECT RAISE(ABORT, 'curriculum concept identities are immutable'); END;
CREATE TRIGGER curriculum_concepts_no_delete
BEFORE DELETE ON curriculum_concepts
BEGIN SELECT RAISE(ABORT, 'curriculum concept identities are immutable'); END;
CREATE TRIGGER curriculum_metadata_ids_no_update
BEFORE UPDATE ON curriculum_metadata_ids
BEGIN SELECT RAISE(ABORT, 'curriculum metadata identities are immutable'); END;
CREATE TRIGGER curriculum_metadata_ids_no_delete
BEFORE DELETE ON curriculum_metadata_ids
BEGIN SELECT RAISE(ABORT, 'curriculum metadata identities are immutable'); END`,
	})
	// Every retryable mutation can reserve a learner-scoped key before applying
	// side effects. Completed responses are replayed byte-for-byte; an ambiguous
	// in-flight reservation is deliberately retained so a disconnect cannot
	// silently duplicate educational state.
	out = append(out, migration{
		Version: "0026_tool_call_idempotency",
		Body: `CREATE TABLE tool_call_idempotency (
    learner_id      TEXT NOT NULL REFERENCES learners(id),
    tool_name       TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('processing','completed')),
    response_text   TEXT NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    completed_at    DATETIME,
    PRIMARY KEY (learner_id, tool_name, idempotency_key),
    CHECK ((status = 'processing' AND completed_at IS NULL) OR
           (status = 'completed' AND completed_at IS NOT NULL))
);
CREATE INDEX idx_tool_call_idempotency_updated
    ON tool_call_idempotency(updated_at)`,
	})
	// Delivery attempts survive process restarts. A claim increments the durable
	// counter; transport failure or stale-claim recovery schedules the next
	// attempt, and the existing terminal `failed` state acts as the dead-letter
	// queue once max_attempts is reached.
	out = append(out, migration{
		Version: "0027_webhook_retry_state",
		Body: `ALTER TABLE webhook_message_queue
    ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0);
ALTER TABLE webhook_message_queue
    ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 5 CHECK (max_attempts BETWEEN 1 AND 100);
ALTER TABLE webhook_message_queue
    ADD COLUMN next_attempt_at DATETIME;
ALTER TABLE webhook_message_queue
    ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_message_queue
    ADD COLUMN dead_lettered_at DATETIME;
UPDATE webhook_message_queue
SET next_attempt_at = scheduled_for
WHERE status = 'pending' AND next_attempt_at IS NULL;
UPDATE webhook_message_queue
SET attempt_count = 1
WHERE status = 'processing' AND attempt_count = 0;
CREATE INDEX idx_wmq_retry_dispatch
    ON webhook_message_queue(learner_id, kind, status, next_attempt_at, scheduled_for)`,
	})
	// Cached mutation responses may contain learner plaintext. Retention can
	// redact that response while this marker preserves an unambiguous completed
	// tombstone: the same key can never acquire a new execution reservation.
	out = append(out, migration{
		Version: "0028_idempotency_response_expiry",
		Body: `ALTER TABLE tool_call_idempotency
    ADD COLUMN response_expired_at DATETIME;
CREATE INDEX idx_tool_call_idempotency_completed
    ON tool_call_idempotency(status, completed_at)`,
	})
	// Tokens issued before client binding and token-family replay detection were
	// introduced cannot be safely adopted by a newly registered client. Revoke
	// them proactively instead of waiting for a rotation attempt to discover the
	// missing provenance.
	out = append(out, migration{
		Version: "0029_revoke_unbound_refresh_tokens",
		Body: `WITH unsafe_families AS (
    SELECT DISTINCT family_id
    FROM refresh_tokens
    WHERE family_id <> ''
      AND (TRIM(COALESCE(client_id, '')) = ''
        OR token NOT LIKE 'sha256:%')
)
UPDATE refresh_tokens
SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP)
WHERE family_id IN (SELECT family_id FROM unsafe_families)
   OR TRIM(COALESCE(client_id, '')) = ''
   OR TRIM(COALESCE(family_id, '')) = ''
   OR token NOT LIKE 'sha256:%'`,
	})
	// Normalize the optional domain association carried by structured webhook
	// payloads. Lifecycle operations can now cancel all unsent work for an
	// archived/deleted domain without parsing learner-authored JSON in a query.
	out = append(out, migration{
		Version: "0030_webhook_domain_scope",
		Body: `ALTER TABLE webhook_message_queue
    ADD COLUMN domain_id TEXT NOT NULL DEFAULT '';
UPDATE webhook_message_queue
SET domain_id = SUBSTR(kind, 5)
WHERE domain_id = '' AND kind LIKE 'olm:%';
UPDATE webhook_message_queue
SET domain_id = COALESCE(json_extract(content, '$.domain_id'), '')
WHERE domain_id = ''
  AND json_valid(content)
  AND json_type(content, '$.domain_id') = 'text';
CREATE INDEX idx_wmq_domain_active
    ON webhook_message_queue(learner_id, domain_id, status)`,
	})
	// Authorization codes minted before exact redirect and S256 PKCE binding
	// cannot prove the authorization request they belong to. Remove them rather
	// than granting a one-time compatibility redemption after upgrade.
	out = append(out, migration{
		Version: "0031_purge_unbound_oauth_codes",
		Body: `DELETE FROM oauth_codes
WHERE TRIM(COALESCE(client_id, '')) = ''
   OR TRIM(COALESCE(redirect_uri, '')) = ''
   OR code_challenge_method <> 'S256'
   OR TRIM(COALESCE(code_challenge, '')) = ''`,
	})
	return out
}

// alterShortName extracts a short, stable token from a SQL statement to make
// the version key human-readable. It is purely cosmetic — the checksum, not the
// version string, is what guards integrity.
func alterShortName(sql string) string {
	// Take the first significant identifier-ish run of characters.
	const maxLen = 40
	var b strings.Builder
	prevSpace := true
	for _, r := range sql {
		switch {
		case r == '\n' || r == '\t':
			if !prevSpace {
				b.WriteByte('_')
				prevSpace = true
			}
		case r == ' ':
			if !prevSpace {
				b.WriteByte('_')
				prevSpace = true
			}
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
			prevSpace = false
		default:
			if !prevSpace {
				b.WriteByte('_')
				prevSpace = true
			}
		}
		if b.Len() >= maxLen {
			break
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "stmt"
	}
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return strings.ToLower(s)
}
