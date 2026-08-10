// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresOAuthToolScopesBackfillPurgeAndRevoke(t *testing.T) {
	base := os.Getenv("TUTOR_TEST_PG_DSN")
	if base == "" {
		t.Skip("set TUTOR_TEST_PG_DSN")
	}

	ctx := context.Background()
	schema := fmt.Sprintf("mig_scope_%d_%d", os.Getpid(), testDBCounter.Add(1))
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	var scoped *sql.DB
	var conn *sql.Conn
	t.Cleanup(func() {
		if conn != nil {
			_ = conn.Close()
		}
		if scoped != nil {
			_ = scoped.Close()
		}
		if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
			t.Errorf("drop test schema %s: %v", schema, err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close postgres admin connection: %v", err)
		}
	})

	if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
		t.Fatalf("drop stale schema %s: %v", schema, err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	scoped, err = sql.Open("pgx", base+separator+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	conn, err = scoped.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, statement := range splitSQLStatements(postgresSchema) {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply postgres base schema (%.60s...): %v", statement, err)
		}
	}

	scopeMigrationIndex := -1
	for i, migration := range postgresMigrations {
		if migration.Version == "postgres_0026_oauth_tool_scopes" {
			scopeMigrationIndex = i
			break
		}
	}
	if scopeMigrationIndex < 0 {
		t.Fatal("postgres OAuth tool-scope migration not found")
	}
	if err := applyPostgresMigrations(ctx, conn, postgresMigrations[:scopeMigrationIndex]); err != nil {
		t.Fatalf("apply migrations before OAuth tool scopes: %v", err)
	}

	scopeMigration := postgresMigrations[scopeMigrationIndex]
	statements := splitSQLStatements(scopeMigration.Body)
	if len(statements) < 4 {
		t.Fatalf("scope migration has only %d statements", len(statements))
	}
	// Add the columns first so the fixture can represent both blank legacy data
	// and corrupt scope values encountered by the data-cleanup portion.
	for _, statement := range statements[:3] {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare scope column (%.60s...): %v", statement, err)
		}
	}

	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	const resource = "https://tutor.test/mcp"
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO learners (id, email, password_hash, objective, email_verified_at)
		 VALUES ('scope-learner', 'scope-learner@test', 'hash', 'objective', $1)`, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO oauth_clients (client_id, client_name, redirect_uris)
		 VALUES ('scope-blank-client', 'blank', '["https://client.test/callback"]'),
		        ('scope-invalid-client', 'invalid', '["https://client.test/callback"]'),
		        ('scope-valid-client', 'valid', '["https://client.test/callback"]'),
		        ('scope-post-client', 'post migration', '["https://client.test/callback"]')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO oauth_codes
		 (code, learner_id, code_challenge, code_challenge_method, client_id,
		  redirect_uri, resource, scope, expires_at)
		 VALUES
		 ('blank-scope-code', 'scope-learner', 'challenge', 'S256', 'scope-blank-client',
		  'https://client.test/callback', $1, '', $2),
		 ('invalid-scope-code', 'scope-learner', 'challenge', 'S256', 'scope-invalid-client',
		  'https://client.test/callback', $1, 'admin', $2),
		 ('valid-scope-code', 'scope-learner', 'challenge', 'S256', 'scope-valid-client',
		  'https://client.test/callback', $1, 'learner:read', $2)`, resource, expires,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO refresh_tokens
		 (token, learner_id, client_id, resource, scope, family_id, expires_at, created_at)
		 VALUES
		 ('sha256:blank-scope-token', 'scope-learner', 'scope-blank-client', $1, '',
		  'blank-family', $2, $3),
		 ('sha256:invalid-scope-token', 'scope-learner', 'scope-invalid-client', $1, 'admin',
		  'invalid-family', $2, $3),
		 ('sha256:valid-scope-token', 'scope-learner', 'scope-valid-client', $1, 'learner:write',
		  'valid-family', $2, $3)`, resource, expires, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO learner_approved_clients (learner_id, client_id, redirect_uri, scope)
		 VALUES
		 ('scope-learner', 'scope-blank-client', 'https://client.test/callback', ''),
		 ('scope-learner', 'scope-invalid-client', 'https://client.test/callback', 'admin'),
		 ('scope-learner', 'scope-valid-client', 'https://client.test/callback', 'learner:read learner:write')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO account_tokens
		 (token_hash, learner_id, purpose, client_id, redirect_uri, resource, scope,
		  code_challenge, code_challenge_method, expires_at, created_at)
		 VALUES
		 ('blank-scope-account', 'scope-learner', 'email_verification', 'scope-blank-client',
		  'https://client.test/callback', $1, '', 'challenge', 'S256', $2, $3),
		 ('invalid-scope-account', 'scope-learner', 'email_verification', 'scope-invalid-client',
		  'https://client.test/callback', $1, 'admin', 'challenge', 'S256', $2, $3),
		 ('valid-scope-account', 'scope-learner', 'email_verification', 'scope-valid-client',
		  'https://client.test/callback', $1, 'learner:read learner:write', 'challenge', 'S256', $2, $3)`,
		resource, expires, now,
	); err != nil {
		t.Fatal(err)
	}

	if err := applyPostgresMigrations(ctx, conn, []migration{scopeMigration}); err != nil {
		t.Fatalf("apply %s: %v", scopeMigration.Version, err)
	}
	var storedChecksum string
	if err := conn.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE version = $1`, scopeMigration.Version,
	).Scan(&storedChecksum); err != nil {
		t.Fatalf("read scope migration record: %v", err)
	}
	if storedChecksum != scopeMigration.checksum() {
		t.Fatalf("scope migration checksum = %q, want %q", storedChecksum, scopeMigration.checksum())
	}

	for _, check := range []struct {
		name  string
		query string
		want  string
	}{
		{"blank code", `SELECT scope FROM oauth_codes WHERE code = 'blank-scope-code'`, "learner"},
		{"valid code", `SELECT scope FROM oauth_codes WHERE code = 'valid-scope-code'`, "learner:read"},
		{"blank refresh", `SELECT scope FROM refresh_tokens WHERE token = 'sha256:blank-scope-token'`, "learner"},
		{"valid refresh", `SELECT scope FROM refresh_tokens WHERE token = 'sha256:valid-scope-token'`, "learner:write"},
		{"blank consent", `SELECT scope FROM learner_approved_clients WHERE client_id = 'scope-blank-client'`, "learner"},
		{"valid consent", `SELECT scope FROM learner_approved_clients WHERE client_id = 'scope-valid-client'`, "learner:read learner:write"},
		{"blank account token", `SELECT scope FROM account_tokens WHERE token_hash = 'blank-scope-account'`, "learner"},
		{"valid account token", `SELECT scope FROM account_tokens WHERE token_hash = 'valid-scope-account'`, "learner:read learner:write"},
	} {
		var got string
		if err := conn.QueryRowContext(ctx, check.query).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", check.name, err)
		}
		if got != check.want {
			t.Fatalf("%s scope = %q, want %q", check.name, got, check.want)
		}
	}

	for _, check := range []struct {
		name  string
		query string
	}{
		{"invalid code", `SELECT COUNT(*) FROM oauth_codes WHERE code = 'invalid-scope-code'`},
		{"invalid consent", `SELECT COUNT(*) FROM learner_approved_clients WHERE client_id = 'scope-invalid-client'`},
	} {
		var count int
		if err := conn.QueryRowContext(ctx, check.query).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", check.name, err)
		}
		if count != 0 {
			t.Fatalf("%s rows = %d, want 0", check.name, count)
		}
	}
	for _, check := range []struct {
		name    string
		query   string
		revoked bool
	}{
		{"blank refresh", `SELECT revoked_at FROM refresh_tokens WHERE token = 'sha256:blank-scope-token'`, false},
		{"valid refresh", `SELECT revoked_at FROM refresh_tokens WHERE token = 'sha256:valid-scope-token'`, false},
		{"invalid refresh", `SELECT revoked_at FROM refresh_tokens WHERE token = 'sha256:invalid-scope-token'`, true},
		{"blank account token", `SELECT consumed_at FROM account_tokens WHERE token_hash = 'blank-scope-account'`, false},
		{"valid account token", `SELECT consumed_at FROM account_tokens WHERE token_hash = 'valid-scope-account'`, false},
		{"invalid account token", `SELECT consumed_at FROM account_tokens WHERE token_hash = 'invalid-scope-account'`, true},
	} {
		var timestamp sql.NullTime
		if err := conn.QueryRowContext(ctx, check.query).Scan(&timestamp); err != nil {
			t.Fatalf("read %s state: %v", check.name, err)
		}
		if timestamp.Valid != check.revoked {
			t.Fatalf("%s terminal state = %v, want %v", check.name, timestamp.Valid, check.revoked)
		}
	}

	// A legacy writer that omits the newly-added scope column must receive the
	// safe broad legacy scope, even after the data migration has completed.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO oauth_codes
		 (code, learner_id, code_challenge, code_challenge_method, client_id,
		  redirect_uri, resource, expires_at)
		 VALUES ('post-migration-code', 'scope-learner', 'challenge', 'S256',
		         'scope-post-client', 'https://client.test/callback', $1, $2)`, resource, expires,
	); err != nil {
		t.Fatalf("legacy oauth-code writer after migration: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO refresh_tokens
		 (token, learner_id, client_id, resource, family_id, expires_at, created_at)
		 VALUES ('sha256:post-migration-token', 'scope-learner', 'scope-post-client', $1,
		         'post-family', $2, $3)`, resource, expires, now,
	); err != nil {
		t.Fatalf("legacy refresh-token writer after migration: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO learner_approved_clients (learner_id, client_id, redirect_uri)
		 VALUES ('scope-learner', 'scope-post-client', 'https://client.test/callback')`,
	); err != nil {
		t.Fatalf("legacy consent writer after migration: %v", err)
	}
	for _, check := range []struct {
		name  string
		query string
	}{
		{"post-migration code", `SELECT scope FROM oauth_codes WHERE code = 'post-migration-code'`},
		{"post-migration refresh", `SELECT scope FROM refresh_tokens WHERE token = 'sha256:post-migration-token'`},
		{"post-migration consent", `SELECT scope FROM learner_approved_clients WHERE client_id = 'scope-post-client'`},
	} {
		var got string
		if err := conn.QueryRowContext(ctx, check.query).Scan(&got); err != nil {
			t.Fatalf("read %s scope: %v", check.name, err)
		}
		if got != "learner" {
			t.Fatalf("%s scope = %q, want learner", check.name, got)
		}
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close scoped connection: %v", err)
	}
	conn = nil
	if err := scoped.Close(); err != nil {
		t.Fatalf("close scoped database: %v", err)
	}
	scoped = nil
	if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
		t.Fatalf("drop test schema %s: %v", schema, err)
	}
	var schemas int
	if err := admin.QueryRow(
		`SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = $1`, schema,
	).Scan(&schemas); err != nil {
		t.Fatalf("verify schema cleanup: %v", err)
	}
	if schemas != 0 {
		t.Fatalf("test schema %s remains after cleanup", schema)
	}
}
