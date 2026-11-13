// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type routeCleanupFixture struct {
	tenant                string
	principal             models.Principal
	expiredCode, liveCode string
	rotatedAncestor       string // used, expired, family still live
	liveDescendant        string
	deadFamily            string // whole family expired
	expiredReset          string
}

var routeCleanupWorker = models.WorkerPrincipal{ActorID: "route-cleanup"}

const (
	routeCleanupClient   = "route-cleanup-client"
	routeCleanupResource = "https://tutor.test/mcp"
)

func seedRouteCleanupTenant(t *testing.T, s *Store, slug string, key byte) routeCleanupFixture {
	t.Helper()
	ctx := t.Context()
	f := routeCleanupFixture{tenant: nativeTestTenant(t, s, slug)}
	member := nativeTestMember(t, s, f.tenant, key, []string{models.RoleLearner})
	principal := models.Principal{
		UserID: member.UserID, TenantID: f.tenant, MembershipID: member.MembershipID,
		LearnerID: member.LearnerID, Roles: []string{models.RoleLearner},
	}
	if err := s.queryRow(ctx, `SELECT version FROM tenant_memberships WHERE tenant_id = ? AND id = ?`,
		f.tenant, member.MembershipID).Scan(&principal.TokenVersion); err != nil {
		t.Fatal(err)
	}
	f.principal = principal
	now := time.Now().UTC()
	f.expiredCode, f.liveCode = slug+"-expired-code", slug+"-live-code"
	for code, expiresAt := range map[string]time.Time{f.expiredCode: now.Add(-time.Hour), f.liveCode: now.Add(time.Hour)} {
		if err := s.CreateAuthCodeForPrincipal(ctx, code, principal, "challenge", "S256", routeCleanupClient,
			"https://client.test/callback", routeCleanupResource, models.OAuthScopeLearner, expiresAt); err != nil {
			t.Fatal(err)
		}
	}
	ancestor, err := s.CreateRefreshTokenForPrincipal(ctx, principal, routeCleanupClient, routeCleanupResource, models.OAuthScopeLearner)
	if err != nil {
		t.Fatal(err)
	}
	descendant, err := s.RotateRefreshToken(ctx, ancestor.Token, routeCleanupClient, routeCleanupResource)
	if err != nil {
		t.Fatal(err)
	}
	f.rotatedAncestor, f.liveDescendant = ancestor.Token, descendant.Token
	dead, err := s.CreateRefreshTokenForPrincipal(ctx, principal, routeCleanupClient, routeCleanupResource, models.OAuthScopeLearner)
	if err != nil {
		t.Fatal(err)
	}
	f.deadFamily = dead.Token
	// Expire the rotated ancestor and the dead family, token row and route alike.
	for _, token := range []string{f.rotatedAncestor, f.deadFamily} {
		if _, err := s.exec(ctx, `UPDATE refresh_tokens SET expires_at = ? WHERE token = ?`, now.Add(-time.Hour), refreshTokenHash(token)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.exec(ctx, `UPDATE credential_tenant_routes SET expires_at = ? WHERE kind = ? AND credential_key = ?`,
			now.Add(-time.Hour), credentialKindRefreshToken, credentialRouteKey(token)); err != nil {
			t.Fatal(err)
		}
	}
	f.expiredReset = slug + "-expired-reset"
	if err := s.insertCredentialRoute(ctx, credentialKindPasswordReset, f.expiredReset, principal.TenantScope(), now.Add(-time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	return f
}

func routeExists(t *testing.T, s *Store, kind, raw string) bool {
	t.Helper()
	var count int
	if err := s.queryRow(t.Context(), `SELECT COUNT(*) FROM credential_tenant_routes WHERE kind = ? AND credential_key = ?`,
		kind, credentialRouteKey(raw)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

func assertRoutes(t *testing.T, s *Store, f routeCleanupFixture, want map[string]bool) {
	t.Helper()
	kinds := map[string]string{
		f.expiredCode: credentialKindAuthorizationCode, f.liveCode: credentialKindAuthorizationCode,
		f.rotatedAncestor: credentialKindRefreshToken, f.liveDescendant: credentialKindRefreshToken,
		f.deadFamily: credentialKindRefreshToken, f.expiredReset: credentialKindPasswordReset,
	}
	for raw, present := range want {
		if got := routeExists(t, s, kinds[raw], raw); got != present {
			t.Errorf("tenant %s route %s present=%v, want %v", f.tenant, raw, got, present)
		}
	}
}

// runRouteCleanup reproduces the scheduler's per-tenant cleanup order: token
// cleanups first, then routes, in one tenant transaction.
func runRouteCleanup(t *testing.T, s *Store, tenant string, tokensFirst bool) int64 {
	t.Helper()
	scope := models.TenantScope{TenantID: tenant, UserID: "worker_route-cleanup", MembershipID: "worker_process"}
	var purged int64
	asWorker(t, s, func() { purged = runRouteCleanupTx(t, s, scope, tokensFirst) })
	return purged
}

// asWorker runs fn as the delivered tutor_worker role on PostgreSQL (the root
// pool is pinned to one connection by nativeDeliveredRoles). Assertions run
// outside it, as the fixture owner, because the worker cannot read routes.
func asWorker(t *testing.T, s *Store, fn func()) {
	t.Helper()
	if s.dialect != DialectPostgres {
		fn()
		return
	}
	if _, err := s.root.Exec(`SET ROLE tutor_worker`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.root.Exec(`RESET ROLE`); err != nil {
			t.Fatalf("reset role: %v", err)
		}
	}()
	fn()
}

func runRouteCleanupTx(t *testing.T, s *Store, scope models.TenantScope, tokensFirst bool) int64 {
	t.Helper()
	var purged int64
	if err := s.WithTenantTx(t.Context(), scope, func(ctx context.Context, scoped storeport.Store) error {
		tx := scoped.(*Store)
		if tokensFirst {
			if _, err := tx.CleanupExpiredCodes(ctx); err != nil {
				return err
			}
			if _, err := tx.CleanupExpiredRefreshTokens(ctx); err != nil {
				return err
			}
		}
		var err error
		purged, err = tx.CleanupExpiredCredentialRoutes(ctx, routeCleanupWorker)
		return err
	}); err != nil {
		t.Fatalf("route cleanup in %s: %v", scope.TenantID, err)
	}
	return purged
}

func TestCleanupExpiredCredentialRoutesKeepsReplayEvidence(t *testing.T) {
	s := setupTestDB(t)
	postgres := s.dialect == DialectPostgres
	if _, err := s.CleanupExpiredCredentialRoutes(t.Context(), models.WorkerPrincipal{}); err == nil {
		t.Fatal("route purge accepted an invalid worker")
	}
	a := seedRouteCleanupTenant(t, s, "route-cleanup-a", 61)
	b := seedRouteCleanupTenant(t, s, "route-cleanup-b", 63)
	if postgres {
		nativeDeliveredRoles(t, s)
		asWorker(t, s, func() {
			// The worker can purge routes without reading any credential digest.
			for _, query := range []string{
				`SELECT credential_key FROM credential_tenant_routes LIMIT 1`,
				`SELECT token FROM refresh_tokens LIMIT 1`,
				`DELETE FROM credential_tenant_routes`,
			} {
				if _, err := s.root.Exec(query); err == nil || !strings.Contains(err.Error(), "permission denied") {
					t.Fatalf("worker privilege %q: %v", query, err)
				}
			}
			// Unscoped, the function has no tenant to act on.
			if purged, err := s.CleanupExpiredCredentialRoutes(t.Context(), routeCleanupWorker); err != nil || purged != 0 {
				t.Fatalf("unscoped purge=%d err=%v", purged, err)
			}
		})
	}

	// Before the token cleanup the dead family's token row still exists, so its
	// route is kept; the expired code and reset routes go immediately.
	runRouteCleanup(t, s, a.tenant, false)
	stage1 := map[string]bool{
		a.expiredCode: false, a.liveCode: true, a.rotatedAncestor: true,
		a.liveDescendant: true, a.deadFamily: true, a.expiredReset: false,
	}
	assertRoutes(t, s, a, stage1)
	if postgres {
		// Scoped to tenant A only: tenant B is untouched.
		assertRoutes(t, s, b, map[string]bool{b.expiredCode: true, b.expiredReset: true, b.deadFamily: true})
	}

	for _, f := range []routeCleanupFixture{a, b} {
		runRouteCleanup(t, s, f.tenant, true)
		assertRoutes(t, s, f, map[string]bool{
			f.expiredCode: false, f.liveCode: true,
			f.rotatedAncestor: true, f.liveDescendant: true,
			f.deadFamily: false, f.expiredReset: false,
		})
	}
	if again := runRouteCleanup(t, s, a.tenant, true); again != 0 {
		t.Fatalf("second purge removed %d routes", again)
	}

	// Replay detection still works for the retained expired ancestor: the
	// whole family, including the live descendant, is revoked.
	if _, err := s.RotateRefreshToken(t.Context(), a.rotatedAncestor, routeCleanupClient, routeCleanupResource); !errors.Is(err, storeport.ErrRefreshTokenReuse) {
		t.Fatalf("replay of retained ancestor: %v", err)
	}
	if _, err := s.GetRefreshToken(t.Context(), a.liveDescendant); err == nil {
		t.Fatal("descendant survived replay of its ancestor")
	}
	// The live code still resolves to its tenant.
	if scope, err := s.credentialScope(t.Context(), credentialKindAuthorizationCode, a.liveCode); err != nil || scope.TenantID != a.tenant {
		t.Fatalf("live code route after purge: %+v %v", scope, err)
	}
}

func TestCredentialRoutePurgeFunctionIsWorkerOnly(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("PostgreSQL required")
	}
	s := setupTestDB(t)
	nativeDeliveredRoles(t, s)
	var definer bool
	var config string
	if err := s.root.QueryRow(`SELECT prosecdef, array_to_string(proconfig, ',')
		FROM pg_proc WHERE oid = 'tutor_purge_expired_credential_routes()'::regprocedure`).Scan(&definer, &config); err != nil {
		t.Fatal(err)
	}
	if !definer || !strings.HasPrefix(config, "search_path=") {
		t.Fatalf("purge function definer=%v config=%q", definer, config)
	}
	for role, want := range map[string]bool{"tutor_worker": true, "tutor_api": false, "tutor_restore": false, "public": false} {
		var allowed bool
		query := `SELECT has_function_privilege($1, 'tutor_purge_expired_credential_routes()', 'EXECUTE')`
		if role == "public" {
			query = `SELECT EXISTS (SELECT 1 FROM pg_proc p, aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) acl
				WHERE p.oid = 'tutor_purge_expired_credential_routes()'::regprocedure AND acl.grantee = 0 AND $1 <> '')`
		}
		if err := s.root.QueryRow(query, role).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed != want {
			t.Errorf("%s EXECUTE=%v, want %v", role, allowed, want)
		}
	}
}
