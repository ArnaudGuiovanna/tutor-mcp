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
	// RFC 8707 resource indicators must survive the complete authorization-code
	// and refresh-token lifecycle. Pre-upgrade credentials have no trustworthy
	// resource binding, so purge short-lived codes and revoke refresh families.
	out = append(out, migration{
		Version: "0032_bind_oauth_credentials_to_resource",
		Body: `ALTER TABLE oauth_codes ADD COLUMN resource TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens ADD COLUMN resource TEXT NOT NULL DEFAULT '';
DELETE FROM oauth_codes WHERE TRIM(resource) = '';
UPDATE refresh_tokens
SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP)
WHERE TRIM(resource) = '';
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_client_resource
    ON refresh_tokens(client_id, resource)`,
	})
	out = append(out, migration{
		Version: "0033_expire_dynamic_oauth_clients",
		Body: `ALTER TABLE oauth_clients ADD COLUMN expires_at DATETIME;
CREATE INDEX IF NOT EXISTS idx_oauth_clients_expires_at
    ON oauth_clients(expires_at)`,
	})
	out = append(out, migration{
		Version: "0034_verified_email_and_account_tokens",
		Body: `ALTER TABLE learners ADD COLUMN email_verified_at DATETIME;
UPDATE learners SET email_verified_at = COALESCE(created_at, CURRENT_TIMESTAMP)
WHERE email_verified_at IS NULL;
CREATE TABLE account_tokens (
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
    expires_at            DATETIME NOT NULL,
    created_at            DATETIME NOT NULL,
    consumed_at           DATETIME
);
CREATE INDEX idx_account_tokens_learner_purpose
    ON account_tokens(learner_id, purpose, expires_at)`,
	})
	// The previous authorization server only issued the bounded legacy
	// `learner` grant, even when the request omitted scope. It is therefore safe
	// to label already-bound credentials and consent explicitly as `learner`.
	// The same default keeps old nodes safe during a rolling deploy: their INSERT
	// statements omit this new column and must keep producing the bounded legacy
	// grant until every process runs the granular-scope build.
	// Anything outside the complete supported vocabulary is purged or revoked
	// rather than guessed.
	out = append(out, migration{
		Version: "0035_oauth_tool_scopes",
		Body: `ALTER TABLE oauth_codes ADD COLUMN scope TEXT NOT NULL DEFAULT 'learner';
ALTER TABLE refresh_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'learner';
ALTER TABLE learner_approved_clients ADD COLUMN scope TEXT NOT NULL DEFAULT 'learner';
UPDATE oauth_codes SET scope = 'learner' WHERE TRIM(scope) = '';
UPDATE refresh_tokens SET scope = 'learner' WHERE TRIM(scope) = '';
UPDATE learner_approved_clients SET scope = 'learner' WHERE TRIM(scope) = '';
UPDATE account_tokens SET scope = 'learner'
WHERE purpose = 'email_verification' AND TRIM(scope) = '';
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
	})
	// A scheduler claim used to be a permanent tombstone: if the claimant died
	// after INSERT, that window could never run again. Preserve legacy rows as
	// completed (replaying them during migration would duplicate notifications),
	// while giving new rows an explicit, recoverable lease lifecycle.
	out = append(out, migration{
		Version: "0036_recoverable_scheduled_job_leases",
		Body: `ALTER TABLE scheduled_job_runs ADD COLUMN status TEXT NOT NULL DEFAULT 'succeeded';
ALTER TABLE scheduled_job_runs ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE scheduled_job_runs ADD COLUMN leased_until DATETIME;
ALTER TABLE scheduled_job_runs ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE scheduled_job_runs ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 3;
ALTER TABLE scheduled_job_runs ADD COLUMN next_attempt_at DATETIME;
ALTER TABLE scheduled_job_runs ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE scheduled_job_runs ADD COLUMN completed_at DATETIME;
ALTER TABLE scheduled_job_runs ADD COLUMN updated_at DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00';
UPDATE scheduled_job_runs
SET completed_at = claimed_at, updated_at = claimed_at
WHERE status = 'succeeded';
CREATE INDEX idx_scheduled_job_runs_runnable
    ON scheduled_job_runs(status, next_attempt_at, leased_until)`,
	})
	// A durable outbound event keeps one stable identity across retries and is
	// linked to its notification reservation. `dispatching` is the persisted
	// pre-HTTP boundary; a stale row beyond it is quarantined as
	// delivery_unknown rather than automatically retried. The transition table
	// intentionally carries no payload or webhook URL.
	out = append(out, migration{
		Version: "0037_webhook_delivery_state_machine",
		Body: `ALTER TABLE scheduled_alerts ADD COLUMN delivery_state TEXT NOT NULL DEFAULT 'reserved';
UPDATE scheduled_alerts
SET delivery_state = CASE WHEN sent = 1 THEN 'delivered' ELSE 'reserved' END;
ALTER TABLE webhook_message_queue ADD COLUMN event_id TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_message_queue ADD COLUMN reservation_id INTEGER REFERENCES scheduled_alerts(id);
ALTER TABLE webhook_message_queue ADD COLUMN dispatch_started_at DATETIME;
UPDATE webhook_message_queue SET event_id = 'legacy-' || id WHERE event_id = '';
CREATE UNIQUE INDEX idx_wmq_event_id
    ON webhook_message_queue(event_id) WHERE event_id <> '';
CREATE INDEX idx_wmq_delivery_reconcile
    ON webhook_message_queue(status, claimed_at, dispatch_started_at, id);
CREATE TABLE webhook_delivery_transitions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    queue_id      INTEGER NOT NULL REFERENCES webhook_message_queue(id) ON DELETE CASCADE,
    event_id      TEXT NOT NULL,
    learner_id    TEXT NOT NULL REFERENCES learners(id),
    attempt_count INTEGER NOT NULL,
    from_status   TEXT NOT NULL DEFAULT '',
    to_status     TEXT NOT NULL,
    reason        TEXT NOT NULL,
    occurred_at   DATETIME NOT NULL
);
CREATE INDEX idx_webhook_delivery_transitions_event
    ON webhook_delivery_transitions(event_id, id DESC);
INSERT INTO webhook_delivery_transitions
    (queue_id, event_id, learner_id, attempt_count, from_status, to_status, reason, occurred_at)
SELECT id, event_id, learner_id, attempt_count, '', status, 'migration_snapshot',
       COALESCE(sent_at, claimed_at, created_at)
FROM webhook_message_queue`,
	})
	// Go-authored fallback payloads must enter the same durable state machine as
	// LLM-authored messages. The explicit format bit lets the scheduler persist
	// and replay the exact Discord payload without interpreting user-authored
	// queue content as an internal transport envelope.
	out = append(out, migration{
		Version: "0038_webhook_content_format",
		Body: `ALTER TABLE webhook_message_queue
    ADD COLUMN content_format TEXT NOT NULL DEFAULT 'message'`,
	})
	// Per-attempt login rows grow with attacker traffic and make every count a
	// range scan. Keep one bounded aggregate per normalized account key instead.
	// Recent legacy events are folded into the first active ten-minute window;
	// old events are intentionally discarded and the legacy journal is emptied.
	out = append(out, migration{
		Version: "0039_bounded_login_failure_windows",
		Body: `CREATE TABLE login_failure_windows (
    account_key       TEXT PRIMARY KEY,
    window_started_at DATETIME NOT NULL,
    last_attempt_at   DATETIME NOT NULL,
    failure_count     INTEGER NOT NULL CHECK (failure_count BETWEEN 1 AND 100),
    updated_at        DATETIME NOT NULL
);
CREATE INDEX idx_login_failure_windows_last_attempt
    ON login_failure_windows(last_attempt_at);
INSERT INTO login_failure_windows
    (account_key, window_started_at, last_attempt_at, failure_count, updated_at)
SELECT account_key, MIN(attempted_at), MAX(attempted_at),
       MIN(COUNT(*), 100), MAX(attempted_at)
FROM login_failures
WHERE attempted_at > DATETIME(CURRENT_TIMESTAMP, '-10 minutes')
GROUP BY account_key;
DELETE FROM login_failures`,
	})
	out = append(out, migration{
		Version: "0040_adaptive_login_challenges",
		Body: `CREATE TABLE login_challenges (
    token_hash            TEXT PRIMARY KEY,
    learner_id            TEXT NOT NULL REFERENCES learners(id),
    client_id             TEXT NOT NULL,
    redirect_uri          TEXT NOT NULL,
    resource              TEXT NOT NULL,
    state                 TEXT NOT NULL DEFAULT '',
    scope                 TEXT NOT NULL DEFAULT '',
    code_challenge        TEXT NOT NULL DEFAULT '',
    code_challenge_method TEXT NOT NULL DEFAULT '',
    expires_at            DATETIME NOT NULL,
    created_at            DATETIME NOT NULL,
    consumed_at           DATETIME,
    trusted_until         DATETIME,
    active                INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1))
);
CREATE UNIQUE INDEX idx_login_challenges_one_active
    ON login_challenges(learner_id) WHERE active = 1;
CREATE INDEX idx_login_challenges_cleanup
    ON login_challenges(active, expires_at, trusted_until)`,
	})
	// Narrative Markdown becomes a shared, versioned object model. Content is
	// always an authenticated ciphertext; checksum and size describe plaintext
	// so corruption and quota drift can be detected without exposing it.
	out = append(out, migration{
		Version: "0041_shared_narrative_objects",
		Body: `CREATE TABLE narrative_objects (
    learner_id  TEXT NOT NULL REFERENCES learners(id) ON DELETE CASCADE,
    scope       TEXT NOT NULL CHECK (scope IN ('memory','memory_pending','session','concept','archive')),
    domain_id   TEXT NOT NULL DEFAULT '',
    object_key  TEXT NOT NULL DEFAULT '',
    ciphertext  TEXT NOT NULL,
    key_id      TEXT NOT NULL,
    version     INTEGER NOT NULL CHECK (version > 0),
    checksum    TEXT NOT NULL CHECK (LENGTH(checksum) = 64),
    size_bytes  INTEGER NOT NULL CHECK (size_bytes >= 0),
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL,
    PRIMARY KEY (learner_id, scope, domain_id, object_key)
);
CREATE INDEX idx_narrative_objects_list
    ON narrative_objects(learner_id, scope, domain_id, object_key DESC);
CREATE INDEX idx_narrative_objects_retention
    ON narrative_objects(updated_at, learner_id);
CREATE TABLE narrative_mutations (
    learner_id      TEXT NOT NULL REFERENCES learners(id) ON DELETE CASCADE,
    mutation_id     TEXT NOT NULL,
    scope           TEXT NOT NULL,
    domain_id       TEXT NOT NULL DEFAULT '',
    object_key      TEXT NOT NULL DEFAULT '',
    mutation_checksum TEXT NOT NULL CHECK (LENGTH(mutation_checksum) = 64),
    result_version  INTEGER NOT NULL CHECK (result_version > 0),
    created_at      DATETIME NOT NULL,
    PRIMARY KEY (learner_id, mutation_id)
);
CREATE INDEX idx_narrative_mutations_created
    ON narrative_mutations(created_at)`,
	})
	// Dynamic-registration authority is durable and shared. Token digests are
	// capabilities; raw IATs never enter the database. A unique effective
	// metadata fingerprint prevents retry/concurrency fan-out into duplicate
	// OAuth clients, while the audit table survives client/token retirement.
	out = append(out, migration{
		Version: "0042_dcr_token_lifecycle",
		Body: `CREATE TABLE oauth_dcr_initial_access_tokens (
    token_id           TEXT PRIMARY KEY,
    token_hash         TEXT NOT NULL UNIQUE CHECK (LENGTH(token_hash) = 64),
    label              TEXT NOT NULL,
    max_registrations  INTEGER NOT NULL CHECK (max_registrations BETWEEN 1 AND 100000),
    used_registrations INTEGER NOT NULL DEFAULT 0 CHECK (used_registrations >= 0 AND used_registrations <= max_registrations),
    created_at         DATETIME NOT NULL,
    expires_at         DATETIME,
    revoked_at         DATETIME,
    created_by         TEXT NOT NULL
);
CREATE INDEX idx_oauth_dcr_tokens_active
    ON oauth_dcr_initial_access_tokens(revoked_at, expires_at);
CREATE TABLE oauth_dcr_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    action      TEXT NOT NULL,
    actor       TEXT NOT NULL,
    token_id    TEXT NOT NULL DEFAULT '',
    client_id   TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '{}',
    occurred_at DATETIME NOT NULL
);
CREATE INDEX idx_oauth_dcr_audit_time
    ON oauth_dcr_audit(occurred_at DESC, id DESC);
ALTER TABLE oauth_clients ADD COLUMN registration_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_clients ADD COLUMN registration_token_id TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_clients ADD COLUMN registration_secret_ciphertext TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_oauth_clients_registration_fingerprint
    ON oauth_clients(registration_fingerprint)
    WHERE registration_fingerprint <> ''`,
	})
	// Retention crosses the relational/narrative boundary. Persist an immutable
	// manifest, recoverable lease and per-phase checkpoints so a crash can be
	// resumed idempotently. Legal holds are separate durable records and remain
	// auditable after release.
	out = append(out, migration{
		Version: "0043_retention_jobs_and_legal_holds",
		Body: `CREATE TABLE retention_legal_holds (
    hold_id        TEXT PRIMARY KEY,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    reason         TEXT NOT NULL,
    created_by     TEXT NOT NULL,
    created_at     DATETIME NOT NULL,
    released_at    DATETIME,
    released_by    TEXT NOT NULL DEFAULT '',
    release_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_retention_legal_holds_active
    ON retention_legal_holds(learner_id, created_at)
    WHERE released_at IS NULL;
CREATE TABLE retention_jobs (
    job_id            TEXT PRIMARY KEY,
    policy_json       TEXT NOT NULL,
    policy_hash       TEXT NOT NULL CHECK (LENGTH(policy_hash) = 64),
    as_of             DATETIME NOT NULL,
    backup_reference  TEXT NOT NULL,
    backup_created_at DATETIME NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('pending','running','failed','completed')),
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    lease_owner       TEXT NOT NULL DEFAULT '',
    leased_until      DATETIME,
    created_by        TEXT NOT NULL,
    created_at        DATETIME NOT NULL,
    started_at        DATETIME,
    completed_at      DATETIME,
    last_error        TEXT NOT NULL DEFAULT '',
    report_json       TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_retention_jobs_status_lease
    ON retention_jobs(status, leased_until, created_at);
CREATE TABLE retention_job_phases (
    job_id       TEXT NOT NULL REFERENCES retention_jobs(job_id) ON DELETE CASCADE,
    phase        TEXT NOT NULL CHECK (phase IN ('database','narrative')),
    position     INTEGER NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('pending','running','failed','completed','skipped')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    eligible     INTEGER NOT NULL DEFAULT 0,
    applied      INTEGER NOT NULL DEFAULT 0,
    held         INTEGER NOT NULL DEFAULT 0,
    report_json  TEXT NOT NULL DEFAULT '{}',
    started_at   DATETIME,
    completed_at DATETIME,
    last_error   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, phase)
);
CREATE INDEX idx_retention_job_phases_order
    ON retention_job_phases(job_id, position)`,
	})
	// Expand the legacy account model into global users and tenant-local
	// memberships. IDs are derived from the existing learner primary key, never
	// from email, so the migration cannot silently merge identities. The
	// after-insert trigger preserves compatibility with an older application
	// binary during a rolling deployment while assigning every new learner to
	// the real (non-wildcard) legacy tenant.
	out = append(out, migration{
		Version: "0044_tenant_identity_foundation",
		Body: `CREATE TABLE tenants (
    id          TEXT PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('active','suspended','closed')),
    region      TEXT NOT NULL DEFAULT 'default',
    policy_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(policy_json)),
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);
INSERT INTO tenants (id, slug, name, status, region, policy_json, created_at, updated_at)
VALUES ('tenant_legacy', 'legacy', 'Legacy tenant', 'active', 'default', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
CREATE TABLE users (
    id                TEXT PRIMARY KEY,
    email             TEXT NOT NULL,
    normalized_email  TEXT NOT NULL,
    password_hash     TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('pending','active','suspended','revoked')),
    email_verified_at DATETIME,
    token_version     INTEGER NOT NULL DEFAULT 1 CHECK (token_version >= 1),
    created_at        DATETIME NOT NULL,
    updated_at        DATETIME NOT NULL
);
CREATE INDEX idx_users_normalized_email ON users(normalized_email);
CREATE TABLE tenant_memberships (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    user_id      TEXT NOT NULL REFERENCES users(id),
    learner_id   TEXT REFERENCES learners(id),
    roles_json   TEXT NOT NULL CHECK (json_valid(roles_json)),
    status       TEXT NOT NULL CHECK (status IN ('invited','active','suspended','revoked')),
    version      INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at   DATETIME NOT NULL,
    updated_at   DATETIME NOT NULL,
    UNIQUE (tenant_id, user_id),
    UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX idx_tenant_memberships_tenant_learner
    ON tenant_memberships(tenant_id, learner_id) WHERE learner_id IS NOT NULL;
CREATE INDEX idx_tenant_memberships_user_status
    ON tenant_memberships(user_id, status, tenant_id);
CREATE TABLE external_identities (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,
    issuer        TEXT NOT NULL,
    subject       TEXT NOT NULL,
    email_at_link TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL,
    last_seen_at  DATETIME NOT NULL,
    UNIQUE (provider, issuer, subject)
);
ALTER TABLE learners ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE learners ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE learners ADD COLUMN membership_id TEXT NOT NULL DEFAULT '';
UPDATE learners
SET tenant_id = 'tenant_legacy', user_id = id, membership_id = 'membership_legacy_' || id
WHERE tenant_id = 'tenant_legacy' AND (user_id = '' OR membership_id = '');
INSERT INTO users
    (id, email, normalized_email, password_hash, status, email_verified_at, token_version, created_at, updated_at)
SELECT id, email, lower(email), password_hash,
       CASE WHEN email_verified_at IS NULL THEN 'pending' ELSE 'active' END,
       email_verified_at, 1, created_at, COALESCE(last_active, created_at)
FROM learners;
INSERT INTO tenant_memberships
    (id, tenant_id, user_id, learner_id, roles_json, status, version, created_at, updated_at)
SELECT membership_id, tenant_id, user_id, id, '["learner"]',
       CASE WHEN email_verified_at IS NULL THEN 'invited' ELSE 'active' END,
       1, created_at, COALESCE(last_active, created_at)
FROM learners;
CREATE UNIQUE INDEX idx_learners_tenant_id_id ON learners(tenant_id, id);
CREATE UNIQUE INDEX idx_learners_tenant_membership ON learners(tenant_id, membership_id);
CREATE TRIGGER learners_identity_after_insert
AFTER INSERT ON learners
BEGIN
    UPDATE learners
    SET tenant_id = CASE WHEN NEW.tenant_id = '' THEN 'tenant_legacy' ELSE NEW.tenant_id END,
        user_id = CASE WHEN NEW.user_id = '' THEN NEW.id ELSE NEW.user_id END,
        membership_id = CASE WHEN NEW.membership_id = '' THEN 'membership_legacy_' || NEW.id ELSE NEW.membership_id END
    WHERE id = NEW.id;
    INSERT OR IGNORE INTO users
        (id, email, normalized_email, password_hash, status, email_verified_at, token_version, created_at, updated_at)
    VALUES (
        CASE WHEN NEW.user_id = '' THEN NEW.id ELSE NEW.user_id END,
        NEW.email, lower(NEW.email), NEW.password_hash,
        CASE WHEN NEW.email_verified_at IS NULL THEN 'pending' ELSE 'active' END,
        NEW.email_verified_at, 1, NEW.created_at, COALESCE(NEW.last_active, NEW.created_at)
    );
    INSERT OR IGNORE INTO tenant_memberships
        (id, tenant_id, user_id, learner_id, roles_json, status, version, created_at, updated_at)
    VALUES (
        CASE WHEN NEW.membership_id = '' THEN 'membership_legacy_' || NEW.id ELSE NEW.membership_id END,
        CASE WHEN NEW.tenant_id = '' THEN 'tenant_legacy' ELSE NEW.tenant_id END,
        CASE WHEN NEW.user_id = '' THEN NEW.id ELSE NEW.user_id END,
        NEW.id, '["learner"]',
        CASE WHEN NEW.email_verified_at IS NULL THEN 'invited' ELSE 'active' END,
        1, NEW.created_at, COALESCE(NEW.last_active, NEW.created_at)
    );
END;
CREATE TRIGGER learners_identity_after_verification
AFTER UPDATE OF email_verified_at ON learners
WHEN OLD.email_verified_at IS NULL AND NEW.email_verified_at IS NOT NULL
BEGIN
    UPDATE users
    SET status = 'active', email_verified_at = NEW.email_verified_at,
        updated_at = CURRENT_TIMESTAMP
    WHERE id = NEW.user_id AND status = 'pending';
    UPDATE tenant_memberships
    SET status = 'active', version = version + 1, updated_at = CURRENT_TIMESTAMP
    WHERE tenant_id = NEW.tenant_id AND id = NEW.membership_id AND status = 'invited';
END;
CREATE TRIGGER learners_identity_after_password_change
AFTER UPDATE OF password_hash ON learners
WHEN OLD.password_hash <> NEW.password_hash
BEGIN
    UPDATE users
    SET password_hash = NEW.password_hash, token_version = token_version + 1,
        updated_at = CURRENT_TIMESTAMP
    WHERE id = NEW.user_id;
    UPDATE tenant_memberships
    SET version = version + 1, updated_at = CURRENT_TIMESTAMP
    WHERE user_id = NEW.user_id AND status IN ('invited','active');
END;`,
	})
	// SQLite remains the single-node compatibility backend. Tenant columns and
	// tenant-first indexes mirror the PostgreSQL contract; the stable legacy
	// default keeps older binaries writable during expand. PostgreSQL owns the
	// enforced composite FKs and RLS used by SaaS deployments.
	out = append(out, migration{
		Version: "0045_expand_tenant_columns",
		Body: `CREATE TABLE tenant_migration_quarantine (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    source_table   TEXT NOT NULL,
    source_key     TEXT NOT NULL,
    reason         TEXT NOT NULL,
    content_hash   TEXT NOT NULL,
    detected_at    DATETIME NOT NULL,
    resolved_at    DATETIME,
    resolution     TEXT NOT NULL DEFAULT '',
    UNIQUE (source_table, source_key, reason)
);
ALTER TABLE refresh_tokens ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE domains ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE concept_states ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE interactions ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE availability ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE scheduled_alerts ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE oauth_codes ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE learner_approved_clients ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE affect_states ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE calibration_records ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE transfer_records ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE implementation_intentions ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE webhook_message_queue ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE pedagogical_snapshots ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE pending_consolidations ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE webhook_push_log ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE learning_sessions ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE assessment_attempts ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE curriculum_versions ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE curriculum_concepts ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE curriculum_metadata_ids ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE tool_call_idempotency ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE account_tokens ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE login_challenges ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE narrative_objects ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE narrative_mutations ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE retention_legal_holds ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
ALTER TABLE webhook_delivery_transitions ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy';
CREATE INDEX idx_refresh_tokens_tenant_learner ON refresh_tokens(tenant_id, learner_id);
CREATE INDEX idx_domains_tenant_learner ON domains(tenant_id, learner_id);
CREATE INDEX idx_concept_states_tenant_learner ON concept_states(tenant_id, learner_id, domain_id);
CREATE INDEX idx_interactions_tenant_learner ON interactions(tenant_id, learner_id, created_at);
CREATE INDEX idx_learning_sessions_tenant_learner ON learning_sessions(tenant_id, learner_id, started_at);
CREATE INDEX idx_assessment_attempts_tenant_learner ON assessment_attempts(tenant_id, learner_id, created_at);
CREATE INDEX idx_oauth_codes_tenant_learner ON oauth_codes(tenant_id, learner_id, expires_at);
CREATE INDEX idx_webhook_queue_tenant_dispatch ON webhook_message_queue(tenant_id, status, next_attempt_at);
CREATE INDEX idx_narrative_objects_tenant_learner ON narrative_objects(tenant_id, learner_id, scope);
CREATE INDEX idx_tool_idempotency_tenant_learner ON tool_call_idempotency(tenant_id, learner_id, tool_name);
CREATE INDEX idx_retention_holds_tenant_learner ON retention_legal_holds(tenant_id, learner_id);
CREATE TRIGGER domains_tenant_insert_guard BEFORE INSERT ON domains
WHEN NEW.tenant_id <> (SELECT tenant_id FROM learners WHERE id = NEW.learner_id)
BEGIN SELECT RAISE(ABORT, 'cross-tenant domains row'); END;
CREATE TRIGGER concept_states_tenant_insert_guard BEFORE INSERT ON concept_states
WHEN NEW.tenant_id <> (SELECT tenant_id FROM learners WHERE id = NEW.learner_id)
BEGIN SELECT RAISE(ABORT, 'cross-tenant concept_states row'); END;
CREATE TRIGGER interactions_tenant_insert_guard BEFORE INSERT ON interactions
WHEN NEW.tenant_id <> (SELECT tenant_id FROM learners WHERE id = NEW.learner_id)
BEGIN SELECT RAISE(ABORT, 'cross-tenant interactions row'); END;
CREATE TRIGGER learning_sessions_tenant_insert_guard BEFORE INSERT ON learning_sessions
WHEN NEW.tenant_id <> (SELECT tenant_id FROM learners WHERE id = NEW.learner_id)
BEGIN SELECT RAISE(ABORT, 'cross-tenant learning_sessions row'); END;`,
	})
	// OAuth capabilities need a non-secret global route to select their tenant
	// before RLS can expose the credential row. Pre-upgrade capabilities are
	// invalidated because SQLite has no built-in SHA-256 with which to create a
	// safe route key from their raw values.
	out = append(out, migration{
		Version: "0046_tenant_scope_oauth_credentials",
		Body: `CREATE TABLE credential_tenant_routes (
    kind           TEXT NOT NULL CHECK (kind IN ('authorization_code','refresh_token','email_verification','password_reset','login_challenge')),
    credential_key TEXT NOT NULL,
    tenant_id      TEXT NOT NULL REFERENCES tenants(id),
    user_id        TEXT NOT NULL REFERENCES users(id),
    membership_id  TEXT NOT NULL,
    learner_id     TEXT NOT NULL,
    expires_at     DATETIME NOT NULL,
    created_at     DATETIME NOT NULL,
    PRIMARY KEY (kind, credential_key),
    FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id),
    FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id)
);
CREATE INDEX idx_credential_tenant_routes_expiry
    ON credential_tenant_routes(expires_at, kind);
ALTER TABLE oauth_codes ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_codes ADD COLUMN membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_codes ADD COLUMN membership_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE refresh_tokens ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens ADD COLUMN membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens ADD COLUMN membership_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE learner_approved_clients ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE learner_approved_clients ADD COLUMN membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE account_tokens ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE account_tokens ADD COLUMN membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE login_challenges ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE login_challenges ADD COLUMN membership_id TEXT NOT NULL DEFAULT '';
UPDATE oauth_codes SET user_id = (SELECT user_id FROM learners WHERE id = oauth_codes.learner_id),
    membership_id = (SELECT membership_id FROM learners WHERE id = oauth_codes.learner_id);
UPDATE refresh_tokens SET user_id = (SELECT user_id FROM learners WHERE id = refresh_tokens.learner_id),
    membership_id = (SELECT membership_id FROM learners WHERE id = refresh_tokens.learner_id);
UPDATE learner_approved_clients SET user_id = (SELECT user_id FROM learners WHERE id = learner_approved_clients.learner_id),
    membership_id = (SELECT membership_id FROM learners WHERE id = learner_approved_clients.learner_id);
UPDATE account_tokens SET user_id = (SELECT user_id FROM learners WHERE id = account_tokens.learner_id),
    membership_id = (SELECT membership_id FROM learners WHERE id = account_tokens.learner_id);
UPDATE login_challenges SET user_id = (SELECT user_id FROM learners WHERE id = login_challenges.learner_id),
    membership_id = (SELECT membership_id FROM learners WHERE id = login_challenges.learner_id);
DELETE FROM oauth_codes;
UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP);
DELETE FROM account_tokens;
DELETE FROM login_challenges;
CREATE INDEX idx_oauth_codes_tenant_membership ON oauth_codes(tenant_id, membership_id, expires_at);
CREATE INDEX idx_refresh_tokens_tenant_membership ON refresh_tokens(tenant_id, membership_id, expires_at);
CREATE INDEX idx_approved_clients_tenant_membership ON learner_approved_clients(tenant_id, membership_id);`,
	})
	// SQLite has no RLS; keep the migration ledger aligned with PostgreSQL's
	// identity-selection policy change.
	out = append(out, migration{
		Version: "0047_membership_identity_selection_policy",
		Body:    `SELECT 1`,
	})
	out = append(out, migration{
		Version: "0048_invitations_mfa_services_audit",
		Body: `CREATE TABLE tenant_invitations (
    id             TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL UNIQUE,
    tenant_id      TEXT NOT NULL REFERENCES tenants(id),
    email          TEXT NOT NULL,
    normalized_email TEXT NOT NULL,
    roles_json     TEXT NOT NULL CHECK (json_valid(roles_json)),
    status         TEXT NOT NULL CHECK (status IN ('pending','accepted','revoked','expired')),
    created_by     TEXT NOT NULL,
    created_at     DATETIME NOT NULL,
    expires_at     DATETIME NOT NULL,
    accepted_at    DATETIME,
    accepted_user_id TEXT,
    accepted_membership_id TEXT
);
CREATE INDEX idx_tenant_invitations_tenant_status ON tenant_invitations(tenant_id, status, expires_at);
CREATE TABLE invitation_tenant_routes (
    token_hash TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id),
    expires_at DATETIME NOT NULL
);
CREATE INDEX idx_invitation_tenant_routes_expiry ON invitation_tenant_routes(expires_at);
CREATE TABLE mfa_credentials (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('totp','webauthn')),
    label          TEXT NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    key_id         TEXT NOT NULL,
    credential_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(credential_json)),
    created_at     DATETIME NOT NULL,
    last_used_at   DATETIME,
    revoked_at     DATETIME
);
CREATE INDEX idx_mfa_credentials_user_active ON mfa_credentials(user_id, revoked_at);
ALTER TABLE tenant_memberships ADD COLUMN mfa_required INTEGER NOT NULL DEFAULT 0 CHECK (mfa_required IN (0,1));
ALTER TABLE tenant_memberships ADD COLUMN mfa_verified_at DATETIME;
UPDATE tenant_memberships SET mfa_required = 1
WHERE EXISTS (SELECT 1 FROM json_each(roles_json) WHERE value IN ('owner','admin'));
CREATE TABLE service_accounts (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL REFERENCES tenants(id),
    name          TEXT NOT NULL,
    client_id     TEXT NOT NULL REFERENCES oauth_clients(client_id),
    roles_json    TEXT NOT NULL CHECK (json_valid(roles_json)),
    status        TEXT NOT NULL CHECK (status IN ('active','suspended','revoked')),
    version       INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_by    TEXT NOT NULL,
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL,
    UNIQUE (tenant_id, client_id)
);
CREATE TABLE audit_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id),
    actor_user_id   TEXT NOT NULL,
    membership_id   TEXT NOT NULL,
    action          TEXT NOT NULL,
    target_type     TEXT NOT NULL,
    target_id       TEXT NOT NULL,
    request_id      TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    details_json    TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(details_json)),
    occurred_at     DATETIME NOT NULL
);
CREATE INDEX idx_audit_events_tenant_time ON audit_events(tenant_id, occurred_at DESC, id DESC);
CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit events are append-only'); END;
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit events are append-only'); END;`,
	})
	out = append(out, migration{
		Version: "0049_formation_catalog_cohorts_enrollments",
		Body: `CREATE TABLE formations (
    id          TEXT NOT NULL,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL CHECK (status IN ('draft','active','archived')),
    created_by  TEXT NOT NULL,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE TABLE formation_versions (
    id             TEXT NOT NULL,
    tenant_id      TEXT NOT NULL,
    formation_id   TEXT NOT NULL,
    version        INTEGER NOT NULL CHECK (version >= 1),
    status         TEXT NOT NULL CHECK (status IN ('draft','published','superseded')),
    metadata_json  TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    created_by     TEXT NOT NULL,
    created_at     DATETIME NOT NULL,
    published_at   DATETIME,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, formation_id, version),
    FOREIGN KEY (tenant_id, formation_id) REFERENCES formations(tenant_id, id)
);
CREATE TABLE formation_modules (
    id                   TEXT NOT NULL,
    tenant_id            TEXT NOT NULL,
    formation_version_id TEXT NOT NULL,
    stable_key           TEXT NOT NULL,
    title                TEXT NOT NULL,
    position             INTEGER NOT NULL CHECK (position >= 0),
    metadata_json        TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, formation_version_id, stable_key),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id)
);
CREATE TABLE formation_concepts (
    id                   TEXT NOT NULL,
    tenant_id            TEXT NOT NULL,
    formation_version_id TEXT NOT NULL,
    module_id            TEXT NOT NULL,
    stable_key           TEXT NOT NULL,
    label                TEXT NOT NULL,
    position             INTEGER NOT NULL CHECK (position >= 0),
    metadata_json        TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, formation_version_id, stable_key),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
    FOREIGN KEY (tenant_id, module_id) REFERENCES formation_modules(tenant_id, id)
);
CREATE TABLE concept_prerequisites (
    tenant_id            TEXT NOT NULL,
    formation_version_id TEXT NOT NULL,
    concept_id           TEXT NOT NULL,
    prerequisite_id      TEXT NOT NULL,
    PRIMARY KEY (tenant_id, formation_version_id, concept_id, prerequisite_id),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
    FOREIGN KEY (tenant_id, concept_id) REFERENCES formation_concepts(tenant_id, id),
    FOREIGN KEY (tenant_id, prerequisite_id) REFERENCES formation_concepts(tenant_id, id),
    CHECK (concept_id <> prerequisite_id)
);
CREATE TABLE cohorts (
    id                   TEXT NOT NULL,
    tenant_id            TEXT NOT NULL,
    formation_version_id TEXT NOT NULL,
    name                 TEXT NOT NULL,
    starts_at            DATETIME,
    ends_at              DATETIME,
    capacity             INTEGER NOT NULL CHECK (capacity > 0),
    reserved_seats       INTEGER NOT NULL DEFAULT 0 CHECK (reserved_seats >= 0 AND reserved_seats <= capacity),
    status               TEXT NOT NULL CHECK (status IN ('planned','open','closed','archived')),
    version              INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_by           TEXT NOT NULL,
    created_at           DATETIME NOT NULL,
    updated_at           DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
    CHECK (ends_at IS NULL OR starts_at IS NULL OR ends_at > starts_at)
);
CREATE TABLE cohort_trainers (
    tenant_id     TEXT NOT NULL,
    cohort_id     TEXT NOT NULL,
    membership_id TEXT NOT NULL,
    assigned_by   TEXT NOT NULL,
    assigned_at   DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, cohort_id, membership_id),
    FOREIGN KEY (tenant_id, cohort_id) REFERENCES cohorts(tenant_id, id),
    FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id)
);
CREATE TABLE enrollments (
    id                   TEXT NOT NULL,
    tenant_id            TEXT NOT NULL,
    cohort_id            TEXT NOT NULL,
    formation_version_id TEXT NOT NULL,
    user_id              TEXT NOT NULL,
    membership_id        TEXT NOT NULL,
    learner_id           TEXT,
    status               TEXT NOT NULL CHECK (status IN ('invited','active','completed','suspended','cancelled')),
    objectives_json      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(objectives_json)),
    seat_reserved        INTEGER NOT NULL DEFAULT 1 CHECK (seat_reserved IN (0,1)),
    created_at           DATETIME NOT NULL,
    updated_at           DATETIME NOT NULL,
    completed_at         DATETIME,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, cohort_id, user_id),
    FOREIGN KEY (tenant_id, cohort_id) REFERENCES cohorts(tenant_id, id),
    FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
    FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id),
    FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id)
);
CREATE INDEX idx_formation_versions_tenant_formation ON formation_versions(tenant_id, formation_id, version DESC);
CREATE INDEX idx_cohorts_tenant_status ON cohorts(tenant_id, status, starts_at);
CREATE INDEX idx_enrollments_tenant_user ON enrollments(tenant_id, user_id, status);
CREATE INDEX idx_enrollments_tenant_cohort ON enrollments(tenant_id, cohort_id, status);
CREATE TRIGGER formation_versions_published_no_update
BEFORE UPDATE ON formation_versions WHEN OLD.status = 'published'
BEGIN SELECT RAISE(ABORT, 'published formation versions are immutable'); END;
CREATE TRIGGER formation_versions_published_no_delete
BEFORE DELETE ON formation_versions WHEN OLD.status = 'published'
BEGIN SELECT RAISE(ABORT, 'published formation versions are immutable'); END;
CREATE TRIGGER formation_modules_published_no_update
BEFORE UPDATE ON formation_modules
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = OLD.tenant_id AND v.id = OLD.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;
CREATE TRIGGER formation_modules_published_no_delete
BEFORE DELETE ON formation_modules
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = OLD.tenant_id AND v.id = OLD.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;
CREATE TRIGGER formation_modules_published_no_insert
BEFORE INSERT ON formation_modules
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = NEW.tenant_id AND v.id = NEW.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;
CREATE TRIGGER formation_concepts_published_no_update
BEFORE UPDATE ON formation_concepts
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = OLD.tenant_id AND v.id = OLD.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;
CREATE TRIGGER formation_concepts_published_no_delete
BEFORE DELETE ON formation_concepts
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = OLD.tenant_id AND v.id = OLD.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;
CREATE TRIGGER formation_concepts_published_no_insert
BEFORE INSERT ON formation_concepts
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = NEW.tenant_id AND v.id = NEW.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;
CREATE TRIGGER concept_prerequisites_published_no_insert
BEFORE INSERT ON concept_prerequisites
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = NEW.tenant_id AND v.id = NEW.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;
CREATE TRIGGER concept_prerequisites_published_no_update
BEFORE UPDATE ON concept_prerequisites
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = OLD.tenant_id AND v.id = OLD.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;
CREATE TRIGGER concept_prerequisites_published_no_delete
BEFORE DELETE ON concept_prerequisites
WHEN EXISTS (SELECT 1 FROM formation_versions v WHERE v.tenant_id = OLD.tenant_id AND v.id = OLD.formation_version_id AND v.status = 'published')
BEGIN SELECT RAISE(ABORT, 'published formation content is immutable'); END;`,
	})
	out = append(out, migration{
		Version: "0050_legacy_enrollment_backfill",
		Body: `CREATE TABLE legacy_domain_enrollments (
    tenant_id    TEXT NOT NULL,
    learner_id   TEXT NOT NULL,
    domain_id    TEXT NOT NULL DEFAULT '',
    enrollment_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, learner_id, domain_id),
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id)
);
CREATE TABLE legacy_concept_mappings (
    tenant_id    TEXT NOT NULL,
    enrollment_id TEXT NOT NULL,
    domain_id    TEXT NOT NULL DEFAULT '',
    concept_label TEXT NOT NULL,
    concept_id   TEXT NOT NULL,
    PRIMARY KEY (tenant_id, enrollment_id, domain_id, concept_label),
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id),
    FOREIGN KEY (tenant_id, concept_id) REFERENCES formation_concepts(tenant_id, id)
);
CREATE TABLE legacy_concept_sources (
    tenant_id     TEXT NOT NULL,
    learner_id    TEXT NOT NULL,
    enrollment_id TEXT NOT NULL,
    domain_id     TEXT NOT NULL DEFAULT '',
    concept_label TEXT NOT NULL,
    concept_id    TEXT NOT NULL,
    PRIMARY KEY (tenant_id, enrollment_id, domain_id, concept_label),
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id)
);
INSERT INTO formations
    (id, tenant_id, name, description, status, created_by, created_at, updated_at)
SELECT 'legacy_formation_' || d.id, d.tenant_id, d.name, d.personal_goal,
       'active', l.user_id, d.created_at, d.created_at
FROM domains d JOIN learners l ON l.tenant_id = d.tenant_id AND l.id = d.learner_id;
INSERT INTO formation_versions
    (id, tenant_id, formation_id, version, status, metadata_json, created_by, created_at, published_at)
SELECT 'legacy_version_' || d.id, d.tenant_id, 'legacy_formation_' || d.id,
       1, 'draft', '{}', l.user_id, d.created_at, NULL
FROM domains d JOIN learners l ON l.tenant_id = d.tenant_id AND l.id = d.learner_id;
INSERT INTO formation_modules
    (id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
SELECT 'legacy_module_' || d.id, d.tenant_id, 'legacy_version_' || d.id,
       'legacy', d.name, 0, '{}'
FROM domains d;
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
       '{}', 1, d.created_at, d.created_at
FROM domains d JOIN learners l ON l.tenant_id = d.tenant_id AND l.id = d.learner_id;
INSERT INTO legacy_domain_enrollments (tenant_id, learner_id, domain_id, enrollment_id)
SELECT d.tenant_id, d.learner_id, d.id, 'legacy_enrollment_' || d.id FROM domains d;
INSERT INTO formations
    (id, tenant_id, name, description, status, created_by, created_at, updated_at)
SELECT 'legacy_recovery_formation_' || l.id, l.tenant_id, 'Legacy recovery',
       'Quarantined evidence with no unambiguous historical domain', 'active',
       l.user_id, l.created_at, l.created_at FROM learners l;
INSERT INTO formation_versions
    (id, tenant_id, formation_id, version, status, metadata_json, created_by, created_at, published_at)
SELECT 'legacy_recovery_version_' || l.id, l.tenant_id,
       'legacy_recovery_formation_' || l.id, 1, 'draft', '{"quarantine":true}',
       l.user_id, l.created_at, NULL FROM learners l;
INSERT INTO formation_modules
    (id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
SELECT 'legacy_recovery_module_' || l.id, l.tenant_id,
       'legacy_recovery_version_' || l.id, 'recovery', 'Recovery', 0, '{"quarantine":true}'
FROM learners l;
INSERT INTO cohorts
    (id, tenant_id, formation_version_id, name, capacity, reserved_seats, status,
     version, created_by, created_at, updated_at)
SELECT 'legacy_recovery_cohort_' || l.id, l.tenant_id,
       'legacy_recovery_version_' || l.id, 'Legacy recovery', 1, 1, 'archived',
       1, l.user_id, l.created_at, l.created_at FROM learners l;
INSERT INTO enrollments
    (id, tenant_id, cohort_id, formation_version_id, user_id, membership_id,
     learner_id, status, objectives_json, seat_reserved, created_at, updated_at)
SELECT 'legacy_recovery_enrollment_' || l.id, l.tenant_id,
       'legacy_recovery_cohort_' || l.id, 'legacy_recovery_version_' || l.id,
       l.user_id, l.membership_id, l.id, 'completed', '{"quarantine":true}', 1,
       l.created_at, l.created_at FROM learners l;
INSERT INTO legacy_domain_enrollments (tenant_id, learner_id, domain_id, enrollment_id)
SELECT l.tenant_id, l.id, '', 'legacy_recovery_enrollment_' || l.id FROM learners l;
INSERT OR IGNORE INTO legacy_concept_sources
    (tenant_id, learner_id, enrollment_id, domain_id, concept_label, concept_id)
SELECT source.tenant_id, source.learner_id, mapping.enrollment_id, source.domain_id,
       source.concept_label,
       'legacy_concept_' || lower(hex(mapping.enrollment_id || char(31) || source.concept_label))
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
WHERE source.concept_label <> '';
INSERT OR IGNORE INTO legacy_concept_sources
    (tenant_id, learner_id, enrollment_id, domain_id, concept_label, concept_id)
SELECT mapping.tenant_id, mapping.learner_id, mapping.enrollment_id, mapping.domain_id, '',
       'legacy_unmapped_' || lower(hex(mapping.enrollment_id))
FROM legacy_domain_enrollments mapping;
INSERT INTO formation_concepts
    (id, tenant_id, formation_version_id, module_id, stable_key, label, position, metadata_json)
SELECT source.concept_id, source.tenant_id, enrollment.formation_version_id,
       module.id,
       CASE WHEN source.concept_label = '' THEN 'legacy_unmapped'
            ELSE 'legacy_' || lower(hex(source.concept_label)) END,
       CASE WHEN source.concept_label = '' THEN 'Unmapped legacy evidence'
            ELSE source.concept_label END,
       ROW_NUMBER() OVER (
           PARTITION BY source.tenant_id, enrollment.formation_version_id
           ORDER BY source.concept_label
       ) - 1,
       CASE WHEN source.domain_id = '' OR source.concept_label = ''
            THEN '{"quarantine":true,"unmapped":true}' ELSE '{}' END
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
INSERT OR IGNORE INTO tenant_migration_quarantine
    (source_table, source_key, reason, content_hash, detected_at)
SELECT 'concept_states', CAST(id AS TEXT), 'missing_or_ambiguous_domain',
       'legacy-concept-state-' || id, CURRENT_TIMESTAMP FROM concept_states WHERE domain_id = '';
ALTER TABLE concept_states ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE concept_states ADD COLUMN formation_concept_id TEXT NOT NULL DEFAULT '';
ALTER TABLE learning_sessions ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE interactions ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE interactions ADD COLUMN formation_concept_id TEXT NOT NULL DEFAULT '';
ALTER TABLE assessment_attempts ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE assessment_attempts ADD COLUMN formation_concept_id TEXT NOT NULL DEFAULT '';
ALTER TABLE affect_states ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE calibration_records ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE transfer_records ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE implementation_intentions ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pedagogical_snapshots ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE narrative_objects ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE narrative_mutations ADD COLUMN enrollment_id TEXT NOT NULL DEFAULT '';
UPDATE concept_states SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = concept_states.tenant_id AND m.learner_id = concept_states.learner_id AND m.domain_id = concept_states.domain_id), ''),
    formation_concept_id = COALESCE((SELECT concept_id FROM legacy_concept_sources source
        WHERE source.tenant_id = concept_states.tenant_id
          AND source.learner_id = concept_states.learner_id
          AND source.domain_id = concept_states.domain_id
          AND source.concept_label = concept_states.concept), '');
UPDATE learning_sessions SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = learning_sessions.tenant_id AND m.learner_id = learning_sessions.learner_id
      AND m.domain_id = COALESCE(learning_sessions.domain_id, '')), 'legacy_recovery_enrollment_' || learner_id);
UPDATE interactions SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = interactions.tenant_id AND m.learner_id = interactions.learner_id
      AND m.domain_id = COALESCE(interactions.domain_id, '')), 'legacy_recovery_enrollment_' || learner_id);
UPDATE interactions SET formation_concept_id = COALESCE((SELECT concept_id FROM legacy_concept_mappings m
    WHERE m.tenant_id = interactions.tenant_id AND m.enrollment_id = interactions.enrollment_id
      AND m.domain_id = COALESCE(interactions.domain_id, '') AND m.concept_label = interactions.concept), '');
UPDATE assessment_attempts SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = assessment_attempts.tenant_id AND m.learner_id = assessment_attempts.learner_id
      AND m.domain_id = assessment_attempts.domain_id), 'legacy_recovery_enrollment_' || learner_id);
UPDATE assessment_attempts SET formation_concept_id = COALESCE((SELECT concept_id FROM legacy_concept_mappings m
    WHERE m.tenant_id = assessment_attempts.tenant_id AND m.enrollment_id = assessment_attempts.enrollment_id
      AND m.domain_id = assessment_attempts.domain_id AND m.concept_label = assessment_attempts.concept_id), '');
UPDATE affect_states SET enrollment_id = COALESCE((SELECT ls.enrollment_id FROM learning_sessions ls
    WHERE ls.tenant_id = affect_states.tenant_id AND ls.learner_id = affect_states.learner_id AND ls.id = affect_states.session_id),
    'legacy_recovery_enrollment_' || learner_id);
UPDATE calibration_records SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = calibration_records.tenant_id AND m.learner_id = calibration_records.learner_id
      AND m.domain_id = calibration_records.domain_id), 'legacy_recovery_enrollment_' || learner_id);
UPDATE transfer_records SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = transfer_records.tenant_id AND m.learner_id = transfer_records.learner_id
      AND m.domain_id = transfer_records.domain_id), 'legacy_recovery_enrollment_' || learner_id);
UPDATE implementation_intentions SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = implementation_intentions.tenant_id AND m.learner_id = implementation_intentions.learner_id
      AND m.domain_id = implementation_intentions.domain_id), 'legacy_recovery_enrollment_' || learner_id);
UPDATE pedagogical_snapshots SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = pedagogical_snapshots.tenant_id AND m.learner_id = pedagogical_snapshots.learner_id
      AND m.domain_id = pedagogical_snapshots.domain_id), 'legacy_recovery_enrollment_' || learner_id);
UPDATE narrative_objects SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = narrative_objects.tenant_id AND m.learner_id = narrative_objects.learner_id
      AND m.domain_id = narrative_objects.domain_id), 'legacy_recovery_enrollment_' || learner_id);
UPDATE narrative_mutations SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
    WHERE m.tenant_id = narrative_mutations.tenant_id AND m.learner_id = narrative_mutations.learner_id
      AND m.domain_id = narrative_mutations.domain_id), 'legacy_recovery_enrollment_' || learner_id);
CREATE INDEX idx_concept_states_enrollment_concept ON concept_states(tenant_id, enrollment_id, formation_concept_id);
CREATE INDEX idx_interactions_enrollment_created ON interactions(tenant_id, enrollment_id, created_at);
CREATE INDEX idx_sessions_enrollment_started ON learning_sessions(tenant_id, enrollment_id, started_at);
CREATE INDEX idx_assessment_enrollment_concept ON assessment_attempts(tenant_id, enrollment_id, formation_concept_id);
CREATE TRIGGER concept_states_learning_scope_after_insert AFTER INSERT ON concept_states
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE concept_states SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = NEW.domain_id),
        'legacy_recovery_enrollment_' || NEW.learner_id),
        formation_concept_id = COALESCE((SELECT concept_id FROM legacy_concept_mappings m
        WHERE m.tenant_id = NEW.tenant_id
          AND m.enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments d
              WHERE d.tenant_id = NEW.tenant_id AND d.learner_id = NEW.learner_id AND d.domain_id = NEW.domain_id), '')
          AND m.domain_id = NEW.domain_id AND m.concept_label = NEW.concept), '')
    WHERE id = NEW.id;
END;
CREATE TRIGGER interactions_learning_scope_after_insert AFTER INSERT ON interactions
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE interactions SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = COALESCE(NEW.domain_id, '')),
        'legacy_recovery_enrollment_' || NEW.learner_id),
        formation_concept_id = COALESCE((SELECT concept_id FROM legacy_concept_mappings m
        WHERE m.tenant_id = NEW.tenant_id
          AND m.enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments d
              WHERE d.tenant_id = NEW.tenant_id AND d.learner_id = NEW.learner_id
                AND d.domain_id = COALESCE(NEW.domain_id, '')), '')
          AND m.domain_id = COALESCE(NEW.domain_id, '') AND m.concept_label = NEW.concept), '')
    WHERE id = NEW.id;
END;
CREATE TRIGGER learning_sessions_learning_scope_after_insert AFTER INSERT ON learning_sessions
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE learning_sessions SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id
          AND m.domain_id = COALESCE(NEW.domain_id, '')), 'legacy_recovery_enrollment_' || NEW.learner_id)
    WHERE id = NEW.id;
END;
CREATE TRIGGER assessment_attempts_learning_scope_after_insert AFTER INSERT ON assessment_attempts
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE assessment_attempts SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = NEW.domain_id),
        'legacy_recovery_enrollment_' || NEW.learner_id),
        formation_concept_id = COALESCE((SELECT concept_id FROM legacy_concept_mappings m
        WHERE m.tenant_id = NEW.tenant_id
          AND m.enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments d
              WHERE d.tenant_id = NEW.tenant_id AND d.learner_id = NEW.learner_id AND d.domain_id = NEW.domain_id), '')
          AND m.domain_id = NEW.domain_id AND m.concept_label = NEW.concept_id), '')
    WHERE id = NEW.id;
END;
CREATE TRIGGER affect_states_learning_scope_after_insert AFTER INSERT ON affect_states
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE affect_states SET enrollment_id = COALESCE((SELECT enrollment_id FROM learning_sessions s
        WHERE s.tenant_id = NEW.tenant_id AND s.learner_id = NEW.learner_id AND s.id = NEW.session_id),
        'legacy_recovery_enrollment_' || NEW.learner_id) WHERE id = NEW.id;
END;
CREATE TRIGGER calibration_records_learning_scope_after_insert AFTER INSERT ON calibration_records
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE calibration_records SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = NEW.domain_id),
        'legacy_recovery_enrollment_' || NEW.learner_id) WHERE prediction_id = NEW.prediction_id;
END;
CREATE TRIGGER transfer_records_learning_scope_after_insert AFTER INSERT ON transfer_records
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE transfer_records SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = NEW.domain_id),
        'legacy_recovery_enrollment_' || NEW.learner_id) WHERE id = NEW.id;
END;
CREATE TRIGGER implementation_intentions_learning_scope_after_insert AFTER INSERT ON implementation_intentions
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE implementation_intentions SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = NEW.domain_id),
        'legacy_recovery_enrollment_' || NEW.learner_id) WHERE id = NEW.id;
END;
CREATE TRIGGER pedagogical_snapshots_learning_scope_after_insert AFTER INSERT ON pedagogical_snapshots
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE pedagogical_snapshots SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = NEW.domain_id),
        'legacy_recovery_enrollment_' || NEW.learner_id) WHERE id = NEW.id;
END;
CREATE TRIGGER narrative_objects_learning_scope_after_insert AFTER INSERT ON narrative_objects
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE narrative_objects SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = NEW.domain_id),
        'legacy_recovery_enrollment_' || NEW.learner_id)
    WHERE learner_id = NEW.learner_id AND scope = NEW.scope
      AND domain_id = NEW.domain_id AND object_key = NEW.object_key;
END;
CREATE TRIGGER narrative_mutations_learning_scope_after_insert AFTER INSERT ON narrative_mutations
WHEN NEW.enrollment_id = '' BEGIN
    UPDATE narrative_mutations SET enrollment_id = COALESCE((SELECT enrollment_id FROM legacy_domain_enrollments m
        WHERE m.tenant_id = NEW.tenant_id AND m.learner_id = NEW.learner_id AND m.domain_id = NEW.domain_id),
        'legacy_recovery_enrollment_' || NEW.learner_id)
    WHERE learner_id = NEW.learner_id AND mutation_id = NEW.mutation_id;
END;`,
	})
	out = append(out, migration{
		Version: "0051_saas_runtime_control_plane",
		Body:    sqliteSaaSControlPlaneMigration,
	})
	out = append(out, migration{
		Version: "0052_service_account_credentials",
		Body:    sqliteServiceAccountCredentialMigration,
	})
	out = append(out, migration{
		Version: "0053_enrollment_learning_state",
		Body:    sqliteEnrollmentLearningStateMigration,
	})
	out = append(out, migration{
		Version: "0054_identity_federation",
		Body:    sqliteIdentityFederationMigration,
	})
	out = append(out, migration{
		Version: "0055_support_access",
		Body:    sqliteSupportAccessMigration,
	})
	out = append(out, migration{
		Version: "0056_catalog_admin_api",
		Body:    sqliteCatalogAdminMigration,
	})
	out = append(out, migration{
		Version: "0057_shared_oauth_csrf",
		Body:    sqliteOAuthCSRFMigration,
	})
	out = append(out, migration{
		Version: "0058_worker_tenant_runs",
		Body:    sqliteWorkerTenantMigration,
	})
	out = append(out, migration{
		Version: "0059_tenant_integration_secret_history",
		Body:    sqliteTenantIntegrationSecretHistoryMigration,
	})
	out = append(out, migration{
		Version: "0060_saas_commercial_invariants",
		Body:    sqliteSaaSCommercialMigration,
	})
	out = append(out, migration{
		Version: "0061_saas_governance",
		Body:    sqliteSaaSGovernanceMigration,
	})
	out = append(out, migration{
		Version: "0062_canonical_narrative_keys",
		Body:    sqliteCanonicalNarrativeKeyMigration,
	})
	out = append(out, migration{
		Version: "0063_platform_audit",
		Body:    sqlitePlatformAuditMigration,
	})
	return out
}

// VerifySQLiteSchemaCurrent is the read-only startup gate used by API and
// worker processes. Only the dedicated migrator may apply DDL in production.
func VerifySQLiteSchemaCurrent(ctx context.Context, database *sql.DB) error {
	return verifyMigrationLedger(ctx, database, buildMigrations())
}

func verifyMigrationLedger(ctx context.Context, database *sql.DB, expected []migration) error {
	for _, item := range expected {
		var checksum string
		err := database.QueryRowContext(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = ?`, item.Version).Scan(&checksum)
		if err != nil {
			return fmt.Errorf("schema compatibility: migration %s is missing: %w", item.Version, err)
		}
		if checksum != item.checksum() {
			return fmt.Errorf("schema compatibility: migration %s checksum mismatch", item.Version)
		}
	}
	return nil
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
