// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var cleanupRLSSchemaCounter atomic.Int64

// TestTenantCleanupPurgesExpiredOAuthCredentialsInEveryTenant runs the cleanup
// job exactly as the production worker registers it (NewTenantDistributedScheduler)
// and, on PostgreSQL, through the delivered tutor_worker role: NOBYPASSRLS,
// FORCE ROW LEVEL SECURITY, column-only grants. Expired authorization codes and
// refresh tokens must disappear from an active and from a suspended tenant,
// while live ones survive in both.
func TestTenantCleanupPurgesExpiredOAuthCredentialsInEveryTenant(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			fixture, runtime := cleanupRLSStores(t, backend)
			ctx := context.Background()
			active := cleanupRLSSeedTenant(t, fixture, "cleanup-rls-active", 41)
			suspended := cleanupRLSSeedTenant(t, fixture, "cleanup-rls-suspended", 43)
			if err := fixture.SetTenantStatus(ctx, cleanupRLSActor(), suspended, models.TenantStatusSuspended); err != nil {
				t.Fatal(err)
			}
			for _, tenant := range []string{active, suspended} {
				cleanupRLSAssertCounts(t, fixture, tenant, 2, 2)
			}

			if backend == "postgres" {
				// The suspected failure mode: an unscoped statement on the
				// worker connection sees no tenant row at all.
				var visible int
				if err := runtime.RawDB().QueryRow(`SELECT COUNT(*) FROM oauth_codes`).Scan(&visible); err != nil {
					t.Fatal(err)
				}
				if visible != 0 {
					t.Fatalf("unscoped worker sees %d tenant authorization codes", visible)
				}
				removed, err := runtime.CleanupExpiredCodes(ctx)
				if err != nil || removed != 0 {
					t.Fatalf("unscoped cleanup removed=%d err=%v, want 0 under RLS", removed, err)
				}

				// Business-job enumeration (active tenants only) leaves the
				// suspended tenant's expired credentials behind.
				legacy := NewTenantDistributedScheduler(runtime, quietLogger(), "cleanup-rls-active-only")
				if result := legacy.scheduledTenantJob("cleanup", func(child *Scheduler) scheduledJobResult {
					return child.cleanupExpiredData()
				})(); result.failed() {
					t.Fatalf("active-only cleanup failed: %s", result.FailureCode)
				}
				cleanupRLSAssertCounts(t, fixture, active, 1, 1)
				cleanupRLSAssertCounts(t, fixture, suspended, 2, 2)
			}

			scheduler := NewTenantDistributedScheduler(runtime, quietLogger(), "cleanup-rls")
			if err := scheduler.Start(); err != nil {
				t.Fatal(err)
			}
			defer scheduler.Stop()
			job := scheduler.registeredDistributed["cleanup"]
			if job == nil {
				t.Fatal("cleanup job is not registered")
			}
			if result := job(); result.failed() {
				t.Fatalf("cleanup failed: %s", result.FailureCode)
			}
			for _, tenant := range []string{active, suspended} {
				cleanupRLSAssertCounts(t, fixture, tenant, 1, 1)
				cleanupRLSAssertNoExpired(t, fixture, tenant)
			}
			// Idempotent: a second tick has nothing left to purge.
			if result := job(); result.failed() {
				t.Fatalf("second cleanup failed: %s", result.FailureCode)
			}
			for _, tenant := range []string{active, suspended} {
				cleanupRLSAssertCounts(t, fixture, tenant, 1, 1)
			}
		})
	}
}

func cleanupRLSActor() models.ControlPlanePrincipal {
	return models.ControlPlanePrincipal{
		ActorID: "cleanup-rls-operator", Roles: []string{models.RolePlatformAdmin},
		Reason: "cleanup RLS regression", RequestID: "cleanup-rls",
	}
}

func cleanupRLSKey(index byte) string {
	secret := make([]byte, 32)
	secret[31] = index
	_, pub := btcec.PrivKeyFromBytes(secret)
	return hex.EncodeToString(schnorr.SerializePubKey(pub))
}

// cleanupRLSSeedTenant provisions one tenant with one learner and stores one
// expired and one live authorization code and refresh token for that learner.
func cleanupRLSSeedTenant(t *testing.T, fixture *db.Store, slug string, keyIndex byte) string {
	t.Helper()
	ctx := context.Background()
	tenant, err := fixture.ProvisionTenant(ctx, cleanupRLSActor(), slug, slug, "test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	member, err := fixture.ProvisionNativeMember(ctx, cleanupRLSActor(), models.NativeOwnerProvision{
		TenantID: tenant.ID, DisplayName: "Cleanup learner",
		PublicKey: cleanupRLSKey(keyIndex), RecoveryPublicKey: cleanupRLSKey(keyIndex + 1),
	}, []string{models.RoleLearner})
	if err != nil {
		t.Fatal(err)
	}
	principal := models.Principal{
		UserID: member.UserID, TenantID: tenant.ID, MembershipID: member.MembershipID,
		LearnerID: member.LearnerID, Roles: []string{models.RoleLearner}, TokenVersion: 1,
	}
	now := time.Now().UTC()
	for label, expiresAt := range map[string]time.Time{"expired": now.Add(-time.Hour), "live": now.Add(time.Hour)} {
		code := fmt.Sprintf("%s-%s-code", slug, label)
		if err := fixture.CreateAuthCodeForPrincipal(ctx, code, principal, "challenge", "S256",
			"cleanup-client", "https://client.test/callback", "https://tutor.test/mcp",
			models.OAuthScopeLearner, expiresAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.CreateRefreshTokenForPrincipal(ctx, principal, "cleanup-client",
		"https://tutor.test/mcp", models.OAuthScopeLearner); err != nil {
		t.Fatal(err)
	}
	// Age the first token's whole family; the next token starts a live family.
	if _, err := fixture.RawDB().Exec(cleanupRLSRebind(fixture,
		`UPDATE refresh_tokens SET expires_at = ? WHERE tenant_id = ?`), now.Add(-time.Hour), tenant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.RawDB().Exec(cleanupRLSRebind(fixture,
		`UPDATE credential_tenant_routes SET expires_at = ? WHERE tenant_id = ? AND kind = 'refresh_token'`), now.Add(-time.Hour), tenant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.CreateRefreshTokenForPrincipal(ctx, principal, "cleanup-client",
		"https://tutor.test/mcp", models.OAuthScopeLearner); err != nil {
		t.Fatal(err)
	}
	return tenant.ID
}

// cleanupRLSRebind numbers placeholders for PostgreSQL. RawDB bypasses the
// Store's own rebinding, and pgx accepts '?' only on simple-protocol queries.
func cleanupRLSRebind(store *db.Store, query string) string {
	if !cleanupRLSPostgres(store) {
		return query
	}
	var out strings.Builder
	n := 0
	for _, ch := range query {
		if ch == '?' {
			n++
			fmt.Fprintf(&out, "$%d", n)
			continue
		}
		out.WriteRune(ch)
	}
	return out.String()
}

func cleanupRLSPostgres(store *db.Store) bool {
	var version string
	return store.RawDB().QueryRow(`SELECT version()`).Scan(&version) == nil && strings.HasPrefix(version, "PostgreSQL")
}

// cleanupRLSAssertCounts checks the credential rows and their tenant routes,
// which the cleanup removes together (a route outlives nothing it serves).
func cleanupRLSAssertCounts(t *testing.T, fixture *db.Store, tenant string, codes, tokens int) {
	t.Helper()
	var gotCodes, gotTokens, codeRoutes, tokenRoutes int
	if err := fixture.RawDB().QueryRow(cleanupRLSRebind(fixture,
		`SELECT (SELECT COUNT(*) FROM oauth_codes WHERE tenant_id = ?),
		        (SELECT COUNT(*) FROM refresh_tokens WHERE tenant_id = ?),
		        (SELECT COUNT(*) FROM credential_tenant_routes WHERE tenant_id = ? AND kind = 'authorization_code'),
		        (SELECT COUNT(*) FROM credential_tenant_routes WHERE tenant_id = ? AND kind = 'refresh_token')`),
		tenant, tenant, tenant, tenant).Scan(&gotCodes, &gotTokens, &codeRoutes, &tokenRoutes); err != nil {
		t.Fatal(err)
	}
	if gotCodes != codes || gotTokens != tokens || codeRoutes != codes || tokenRoutes != tokens {
		t.Fatalf("tenant %s codes=%d tokens=%d code_routes=%d token_routes=%d, want %d/%d",
			tenant, gotCodes, gotTokens, codeRoutes, tokenRoutes, codes, tokens)
	}
}

func cleanupRLSAssertNoExpired(t *testing.T, fixture *db.Store, tenant string) {
	t.Helper()
	var expired int
	if err := fixture.RawDB().QueryRow(cleanupRLSRebind(fixture,
		`SELECT (SELECT COUNT(*) FROM oauth_codes WHERE tenant_id = ? AND expires_at < ?)
		      + (SELECT COUNT(*) FROM refresh_tokens WHERE tenant_id = ? AND expires_at < ?)`),
		tenant, time.Now().UTC(), tenant, time.Now().UTC()).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 0 {
		t.Fatalf("tenant %s retains %d expired credentials", tenant, expired)
	}
}

// cleanupRLSStores returns a fixture store (SQLite, or a PostgreSQL superuser
// that bypasses RLS) and the runtime store the scheduler uses. On PostgreSQL
// the runtime pool runs as the delivered tutor_worker role from
// deploy/postgres-roles.sql, in a private schema.
func cleanupRLSStores(t *testing.T, backend string) (*db.Store, *db.Store) {
	t.Helper()
	if backend == "sqlite" {
		_, store, _ := rawTestSetup(t, "")
		return store, store
	}
	dsn := os.Getenv("TUTOR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set TUTOR_TEST_PG_DSN to run the PostgreSQL worker-role cleanup regression")
	}
	schema := fmt.Sprintf("cleanup_rls_%d_%d", os.Getpid(), cleanupRLSSchemaCounter.Add(1))
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE; CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	raw, err := db.OpenPostgres(dsn+separator+"search_path="+schema, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MigratePostgres(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../deploy/postgres-roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	roles := strings.ReplaceAll(string(script), "\\set ON_ERROR_STOP on", "")
	roles = strings.ReplaceAll(roles, "SCHEMA public", "SCHEMA "+schema)
	if _, err := raw.Exec(roles); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	runtimeDB := stdlib.OpenDB(*config, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE tutor_worker`)
		return err
	}))
	runtimeDB.SetMaxOpenConns(2)
	t.Cleanup(func() {
		_ = runtimeDB.Close()
		_ = raw.Close()
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	var super, bypass bool
	if err := runtimeDB.QueryRow(`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatal("runtime worker bypasses RLS")
	}
	for _, table := range []string{"oauth_codes", "refresh_tokens"} {
		var enabled, forced bool
		if err := runtimeDB.QueryRow(`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, table).Scan(&enabled, &forced); err != nil || !enabled || !forced {
			t.Fatalf("%s RLS enabled=%v forced=%v err=%v", table, enabled, forced, err)
		}
	}
	return db.NewStoreWithDialect(raw, db.DialectPostgres), db.NewStoreWithDialect(runtimeDB, db.DialectPostgres)
}
