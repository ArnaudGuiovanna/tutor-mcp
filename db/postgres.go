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
		// Already applied and unchanged: nothing to do.
		return nil
	case sql.ErrNoRows:
		// First apply: fall through.
	default:
		return fmt.Errorf("postgres migrate: read schema_migrations: %w", err)
	}

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
