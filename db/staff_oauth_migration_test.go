// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"path/filepath"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestStaffOAuthMigrationPreservesLiveLearnerCredentials(t *testing.T) {
	raw, err := OpenDB(filepath.Join(t.TempDir(), "old-credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := ensureSchemaMigrationsTable(raw); err != nil {
		t.Fatal(err)
	}
	for _, migration := range buildMigrations() {
		if migration.Version == "0072_staff_oauth" {
			break
		}
		if err := applyMigration(raw, migration); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(raw)
	seedLearner(t, s, "before-staff")
	if _, err := raw.Exec(`UPDATE learners SET email_verified_at = ? WHERE id = 'before-staff'`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAuthCodeWithBinding(t.Context(), "live-before-staff", "before-staff", "challenge", "S256", "client", "https://client.test/cb", testOAuthResource, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPrincipalForLearner(t.Context(), "before-staff", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := s.CreateRefreshTokenForPrincipal(t.Context(), p, "client", testOAuthResource, models.OAuthScopeLearner)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAuthCode(t.Context(), "live-before-staff", "client"); err != nil {
		t.Fatalf("live authorization code lost: %v", err)
	}
	if _, err := s.GetRefreshToken(t.Context(), rt.Token); err != nil {
		t.Fatalf("live refresh token lost: %v", err)
	}
	if _, err := s.RotateRefreshToken(t.Context(), rt.Token, "client", testOAuthResource); err != nil {
		t.Fatal(err)
	}
	rows, err := raw.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("credential migration damaged foreign keys")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
