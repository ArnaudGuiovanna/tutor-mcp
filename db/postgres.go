// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	_ "embed"
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

	for _, stmt := range splitSQLStatements(postgresSchema) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply postgres schema (%.60s...): %w", stmt, err)
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
