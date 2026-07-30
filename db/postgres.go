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

// splitSQLStatements splits a multi-statement SQL string on ';' boundaries,
// dropping blank statements and SQL line comments. The tutor-mcp schema never
// embeds a ';' inside a string literal, so a plain split is correct here.
func splitSQLStatements(sqlText string) []string {
	var out []string
	for _, raw := range strings.Split(sqlText, ";") {
		var lines []string
		for _, line := range strings.Split(raw, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			lines = append(lines, line)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
