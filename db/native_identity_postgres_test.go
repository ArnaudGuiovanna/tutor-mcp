// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func nativeDeliveredRoles(t *testing.T, s *Store) {
	t.Helper()
	var schema string
	if err := s.queryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../deploy/postgres-roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(script), "\\set ON_ERROR_STOP on", "")
	text = strings.ReplaceAll(text, "SCHEMA public", "SCHEMA "+schema)
	if _, err = s.root.ExecContext(t.Context(), text); err != nil {
		t.Fatal(err)
	}
	s.root.SetMaxOpenConns(1)
	s.root.SetMaxIdleConns(1)
}
func nativeAssumeRole(t *testing.T, s *Store, role string) {
	t.Helper()
	if _, err := s.root.ExecContext(t.Context(), `SET ROLE `+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.root.Exec(`RESET ROLE`); err != nil {
			t.Errorf("reset role: %v", err)
		}
	})
}

func TestNativePostgresDeliveredAPIRoleAndForcedIsolation(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("PostgreSQL required")
	}
	s := setupTestDB(t)
	a := nativeTestTenant(t, s, "native-rls-a")
	b := nativeTestTenant(t, s, "native-rls-b")
	n := nativeTestMember(t, s, a, 25, []string{models.RoleLearner})
	other := nativeTestMember(t, s, b, 27, []string{models.RoleLearner})
	cOther := nativeTestChallenge(other, "other-rls")
	if err := s.CreateNativeChallenge(t.Context(), cOther); err != nil {
		t.Fatal(err)
	}
	nativeDeliveredRoles(t, s)
	nativeAssumeRole(t, s, "tutor_api")
	for _, table := range []string{"native_challenges", "native_sessions"} {
		var enabled, forced bool
		if err := s.root.QueryRow(`SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, table).Scan(&enabled, &forced); err != nil || !enabled || !forced {
			t.Fatalf("RLS %s: %v %v %v", table, enabled, forced, err)
		}
	}
	if _, err := s.GetNativeIdentity(t.Context(), a, n.PublicKey, "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNativeIdentity(t.Context(), b, n.PublicKey, "session"); !errors.Is(err, storeport.ErrInvalidNativeIdentity) {
		t.Fatalf("unrelated membership=%v", err)
	}
	c := nativeTestChallenge(n, "api-rls")
	if err := s.CreateNativeChallenge(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeNativeChallenge(t.Context(), c.TokenHash, time.Now().UTC()); err != nil {
		t.Fatalf("pre-tenant nonce consumption with API role=%v", err)
	}
	session := nativeTestSession(n, "api-rls-session", []string{models.OAuthScopeLearnerRead})
	p, err := s.CreateNativeSession(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateNativeSession(t.Context(), session.ID, p, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	err = s.withNativeTx(t.Context(), a, "", "", func(tx *Store) error {
		var count int
		if err := tx.queryRow(t.Context(), `SELECT COUNT(*) FROM native_challenges WHERE tenant_id=?`, b).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("foreign challenges visible=%d", count)
		}
		result, err := tx.exec(t.Context(), `DELETE FROM native_challenges WHERE tenant_id=?`, b)
		if err != nil {
			return err
		}
		affected, _ := result.RowsAffected()
		if affected != 0 {
			t.Fatal("foreign challenge deleted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RotateNativeIdentity(t.Context(), n.ID, n.Version, nativeTestKey(29), nativeTestKey(30), time.Now().UTC()); err != nil {
		t.Fatalf("cross-tenant membership version fencing with API role=%v", err)
	}
	if err = s.ValidateNativeSession(t.Context(), session.ID, p, time.Now().UTC()); !errors.Is(err, storeport.ErrInvalidNativeSession) {
		t.Fatalf("old API grant after rotation=%v", err)
	}
	var count int
	if err = s.root.QueryRow(`SELECT COUNT(*) FROM native_challenges`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("transaction tenant setting leaked: %d %v", count, err)
	}
}

func TestNativePostgresErasureWithDeliveredWorkerPrivileges(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("PostgreSQL required")
	}
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-worker")
	owner, err := s.BootstrapNativeOwner(t.Context(), nativeTestActor(), models.NativeOwnerProvision{TenantID: tenant, DisplayName: "Direction", PublicKey: nativeTestKey(31), RecoveryPublicKey: nativeTestKey(32)})
	if err != nil {
		t.Fatal(err)
	}
	ownerP, err := s.GetPrincipal(t.Context(), models.TenantScope{TenantID: owner.TenantID, UserID: owner.UserID, MembershipID: owner.MembershipID}, []string{models.OAuthScopeFormationWrite})
	if err != nil {
		t.Fatal(err)
	}
	n := nativeTestMember(t, s, tenant, 33, []string{models.RoleLearner})
	challenge := nativeTestChallenge(n, "worker-erasure")
	if err = s.CreateNativeChallenge(t.Context(), challenge); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateNativeSession(t.Context(), nativeTestSession(n, "worker-erasure-session", []string{models.OAuthScopeLearnerRead})); err != nil {
		t.Fatal(err)
	}
	request, err := s.RequestTenantDSAR(t.Context(), ownerP, n.LearnerID, "erase", "native member erasure")
	if err != nil {
		t.Fatal(err)
	}
	nativeDeliveredRoles(t, s)
	nativeAssumeRole(t, s, "tutor_worker")
	for _, query := range []string{`SELECT token_hash FROM native_challenges LIMIT 1`, `SELECT scopes_json FROM native_sessions LIMIT 1`, `SELECT public_key FROM native_identity_keys LIMIT 1`} {
		rows, err := s.root.Query(query)
		if err == nil {
			rows.Close()
			t.Fatalf("worker reads identity material: %s", query)
		}
		if !strings.Contains(err.Error(), "permission denied") {
			t.Fatal(err)
		}
	}
	finishSafetyErasure(t, s, ownerP, request.ID)
	if _, err = s.root.Exec(`RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	var total int
	if err = s.queryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM native_challenges WHERE identity_id=?)+(SELECT COUNT(*) FROM native_sessions WHERE identity_id=?)`, n.ID, n.ID).Scan(&total); err != nil || total != 0 {
		t.Fatalf("erasure retained grants: %d %v", total, err)
	}
	if _, err = s.GetNativeIdentity(t.Context(), tenant, n.PublicKey, "session"); !errors.Is(err, storeport.ErrInvalidNativeIdentity) {
		t.Fatalf("erased membership can authenticate=%v", err)
	}
	var global int
	if err = s.queryRow(t.Context(), `SELECT COUNT(*) FROM native_identities WHERE id=? AND status='active'`, n.ID).Scan(&global); err != nil || global != 1 {
		t.Fatalf("tenant erasure destroyed global identity: %d %v", global, err)
	}
}

func TestNativePostgresArchiveContainsPublicIdentityWithoutLiveGrants(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("PostgreSQL required")
	}
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-archive")
	n := nativeTestMember(t, s, tenant, 35, []string{models.RoleLearner})
	if err := s.CreateNativeChallenge(t.Context(), nativeTestChallenge(n, "archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNativeSession(t.Context(), nativeTestSession(n, "archive-session", []string{models.OAuthScopeLearnerRead})); err != nil {
		t.Fatal(err)
	}
	raw, err := s.ExportPostgresTenantArchive(context.Background(), nativeTestActor(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	var archive TenantLogicalArchive
	if err = json.Unmarshal(raw, &archive); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"native_challenges", "native_sessions"} {
		if string(archive.Tables[table]) != "[]" {
			t.Fatalf("backup resurrects native grant: %s", table)
		}
	}
	if !strings.Contains(string(archive.Globals["native_identities"]), n.PublicKey) || !strings.Contains(string(archive.Globals["native_identity_keys"]), n.RecoveryPublicKey) {
		t.Fatal("backup omitted public identity dependency")
	}
}
