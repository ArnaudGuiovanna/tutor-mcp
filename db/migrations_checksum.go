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

// ensureSchemaMigrationsTable creates the bookkeeping table used by Migrate to
// track which migrations have been applied and the checksum they were applied
// with. Called unconditionally before any other migration runs.
func ensureSchemaMigrationsTable(db *sql.DB) error {
	return ensureSchemaMigrationsTableInTx(context.Background(), db)
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

// applyMigration executes one migration if it has not been applied yet, or
// verifies its stored checksum if it has. A mismatch returns an error
// containing "checksum mismatch" so callers (and tests) can detect drift.
//
// Issue #118: the body Exec and the bookkeeping INSERT are wrapped in a
// single transaction so a crash (or any error) between them cannot leave the
// schema mutated but unrecorded. Issue #126: Migrate calls applyMigrationInTx
// while holding a BEGIN EXCLUSIVE transaction, serialising the check-then-
// insert path across processes.
func applyMigration(db *sql.DB, m migration) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx %q: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := applyMigrationInTx(ctx, tx, m); err != nil {
		return err
	}
	return tx.Commit()
}

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
