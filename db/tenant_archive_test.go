// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
)

func TestPostgresTenantLogicalArchiveRestoresOneTenantAndRejectsTampering(t *testing.T) {
	dsn := os.Getenv("TUTOR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set TUTOR_TEST_PG_DSN")
	}
	source := setupTestPG(t, dsn)
	ctx := context.Background()
	operator := models.ControlPlanePrincipal{ActorID: "tenant-archive-test", Roles: []string{models.RolePlatformAdmin},
		Reason: "tenant logical restore acceptance", RequestID: "tenant-archive-test-1"}
	tenant, err := source.ProvisionTenant(ctx, operator, "archive-tenant", "Archive tenant", "eu-test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := source.exec(ctx, `INSERT INTO users
		(id, email, normalized_email, password_hash, status, email_verified_at,
		 token_version, created_at, updated_at)
		VALUES ('archive-user', 'archive@example.test', 'archive@example.test',
		 'archive-hash', 'active', ?, 1, ?, ?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := source.exec(ctx, `INSERT INTO learners
		(id, email, password_hash, objective, webhook_url, profile_json, created_at,
		 last_active, email_verified_at, tenant_id, user_id, membership_id)
		VALUES ('archive-learner', 'archive@example.test', 'archive-hash', 'archive objective',
		 '', '{}', ?, ?, ?, ?, 'archive-user', 'archive-membership')`, now, now, now, tenant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := source.exec(ctx, `INSERT INTO external_identities
		(id, user_id, provider, issuer, subject, email_at_link, created_at, last_seen_at)
		VALUES ('archive-identity', 'archive-user', 'oidc', 'https://issuer.example',
		 'archive-subject', 'archive@example.test', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := source.EnsureRecoveryEnrollment(ctx, "archive-learner"); err != nil {
		t.Fatal(err)
	}
	keyring := testSecretKeyring(t, "archive:"+encodedSecretKey(67), "archive")
	source.SetIntegrationSecretKeyring(keyring)
	narrativeKey := memory.NarrativeKey{
		TenantID: tenant.ID, EnrollmentID: "legacy_recovery_enrollment_archive-learner",
		LearnerID: "archive-learner", Scope: memory.ScopeMemory,
	}
	if _, _, err := source.CompareAndSwapNarrative(ctx, narrativeKey, 0, "isolated narrative",
		"archive-narrative", narrativeChecksum("archive-narrative"), narrativeTestLimits()); err != nil {
		t.Fatal(err)
	}

	encoded, err := source.ExportPostgresTenantArchive(ctx, operator, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	var archive TenantLogicalArchive
	if err := json.Unmarshal(encoded, &archive); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"tenant_id": "tenant_legacy"`) ||
		strings.Contains(string(encoded), `"test@test.com"`) || archive.DataSHA256 == "" {
		t.Fatal("logical archive exposed another tenant or omitted its digest")
	}
	// The historical migration ledger names constraints without a schema
	// predicate, so keep only one isolated conformance schema alive while a
	// second is materialized. Production uses one application schema as well.
	var sourceSchema string
	if err := source.queryRow(ctx, `SELECT current_schema()`).Scan(&sourceSchema); err != nil {
		t.Fatal(err)
	}
	if err := source.root.Close(); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + sourceSchema + ` CASCADE`); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if err := admin.Close(); err != nil {
		t.Fatal(err)
	}
	target := setupTestPG(t, dsn)

	result, err := target.RestorePostgresTenantArchive(ctx, operator, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if result.TenantID != tenant.ID || !result.ForeignKeysOK || !result.IsolationCheck || result.RestoredRows == 0 {
		t.Fatalf("restore result=%+v", result)
	}
	target.SetIntegrationSecretKeyring(keyring)
	object, err := target.GetNarrative(ctx, narrativeKey)
	if err != nil || object.Content != "isolated narrative" {
		t.Fatalf("restored narrative=%+v err=%v", object, err)
	}
	var legacyCount, restoredCount int
	if err := target.queryRow(ctx, `SELECT COUNT(*) FROM learners WHERE tenant_id = 'tenant_legacy'`).Scan(&legacyCount); err != nil {
		t.Fatal(err)
	}
	if err := target.queryRow(ctx, `SELECT COUNT(*) FROM learners WHERE tenant_id = ?`, tenant.ID).Scan(&restoredCount); err != nil {
		t.Fatal(err)
	}
	if legacyCount != 1 || restoredCount != 1 {
		t.Fatalf("tenant isolation counts legacy=%d restored=%d", legacyCount, restoredCount)
	}
	if _, err := target.RestorePostgresTenantArchive(ctx, operator, encoded); err == nil {
		t.Fatal("restore over an existing tenant was accepted")
	}

	var tampered map[string]any
	if err := json.Unmarshal(encoded, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["tenant_id"] = "different-tenant"
	tamperedEncoded, _ := json.Marshal(tampered)
	if _, err := target.RestorePostgresTenantArchive(ctx, operator, tamperedEncoded); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered archive error=%v", err)
	}
}
