// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func preNativeSQLite(t *testing.T) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", fmt.Sprintf("file:native_upgrade_%d?mode=memory&cache=shared", testDBCounter.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { raw.Close() })
	if err = ensureSchemaMigrationsTable(raw); err != nil {
		t.Fatal(err)
	}
	for _, m := range buildMigrations() {
		if m.Version == "0081_native_identities" {
			break
		}
		if err = applyMigration(raw, m); err != nil {
			t.Fatalf("previous migration %s: %v", m.Version, err)
		}
	}
	return raw
}

func TestNativeMigrationPreservesHistoricalRowsAndForeignKeys(t *testing.T) {
	raw := preNativeSQLite(t)
	now := time.Now().UTC()
	if _, err := raw.Exec(`INSERT INTO learners(id,email,password_hash,objective,profile_json,created_at,email_verified_at) VALUES('historical','historical@example.test','old-password','learning objective','{"name":"Existing"}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO external_identities(id,user_id,provider,issuer,subject,email_at_link,created_at,last_seen_at) VALUES('external-old','historical','old','https://old.test','subject','historical@example.test',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO console_sessions(token_hash,user_id,tenant_id,membership_id,membership_version,created_at,last_seen_at,expires_at) VALUES('old-console','historical','tenant_legacy','membership_legacy_historical',1,?,?,?)`, now, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(raw); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`SELECT COUNT(*) FROM users WHERE id='historical' AND email='historical@example.test' AND password_hash='old-password' AND identity_mode='email'`, `SELECT COUNT(*) FROM learners WHERE id='historical' AND objective='learning objective' AND profile_json='{"name":"Existing"}'`, `SELECT COUNT(*) FROM external_identities WHERE id='external-old'`, `SELECT COUNT(*) FROM console_sessions WHERE token_hash='old-console'`} {
		var count int
		if err := raw.QueryRow(query).Scan(&count); err != nil || count != 1 {
			t.Fatalf("historical row changed: count=%d err=%v query=%s", count, err, query)
		}
	}
	var foreignKeys, legacy int
	if err := raw.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`PRAGMA legacy_alter_table`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || legacy != 0 {
		t.Fatalf("successful migration leaked PRAGMAs: %d/%d", foreignKeys, legacy)
	}
	if err := Migrate(raw); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	if err := checkSQLiteForeignKeys(context.Background(), mustSQLiteConn(t, raw)); err != nil {
		t.Fatal(err)
	}
}

func TestNativeMigrationRepairsRecordedLiveColumnBeforeRebuild(t *testing.T) {
	raw := preNativeSQLite(t)
	if _, err := raw.Exec(`ALTER TABLE learners DROP COLUMN profile_json`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(raw); err != nil {
		t.Fatalf("repair before native rebuild: %v", err)
	}
	if _, err := raw.Exec(`SELECT profile_json FROM learners`); err != nil {
		t.Fatalf("live profile column was retired by rebuild metadata: %v", err)
	}
}

func mustSQLiteConn(t *testing.T, raw *sql.DB) *sql.Conn {
	t.Helper()
	conn, err := raw.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestNativeMigrationRollsBackAndRestoresPragmasOnInvalidForeignKey(t *testing.T) {
	raw := preNativeSQLite(t)
	if _, err := raw.Exec(`INSERT INTO external_identities(id,user_id,provider,issuer,subject,email_at_link,created_at,last_seen_at) VALUES('orphan','missing','old','https://old.test','subject','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	err := Migrate(raw)
	if err == nil || !strings.Contains(err.Error(), "foreign_key_check") {
		t.Fatalf("invalid upgrade=%v", err)
	}
	var enabled, legacy, count int
	if err = raw.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if err = raw.QueryRow(`PRAGMA legacy_alter_table`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || legacy != 0 {
		t.Fatalf("migration leaked PRAGMAs: foreign_keys=%d legacy_alter=%d", enabled, legacy)
	}
	if err = raw.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version='0081_native_identities'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration recorded: count=%d err=%v", count, err)
	}
	if err = raw.QueryRow(`SELECT COUNT(*) FROM external_identities WHERE id='orphan'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed migration lost old row: count=%d err=%v", count, err)
	}
	var schema string
	if err = raw.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='users'`).Scan(&schema); err != nil || strings.Contains(schema, "pubkey") {
		t.Fatalf("failed migration changed original table: %v", err)
	}
}
