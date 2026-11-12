// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func dcrTestHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", hash[:])
}

func TestDCRTokenQuotaAndRegistrationAreAtomic(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	token := models.DCRInitialAccessToken{
		TokenID: "quota-token", TokenHash: dcrTestHash("quota-token-secret"),
		Label: "quota test", MaxRegistrations: 1, CreatedAt: now, CreatedBy: "test",
	}
	if err := store.CreateDCRInitialAccessToken(ctx, token, ""); err != nil {
		t.Fatal(err)
	}

	registrations := []models.DynamicClientRegistration{
		{
			TokenID: token.TokenID, Fingerprint: dcrTestHash("profile-a"), CandidateClientID: "client-a",
			ClientName: "A", RedirectURIsJSON: `["https://a.example/cb"]`, AuthMethod: "none", ApplicationType: "web",
			MaxClients: 10, Now: now, ExpiresAt: now.Add(time.Hour),
		},
		{
			TokenID: token.TokenID, Fingerprint: dcrTestHash("profile-b"), CandidateClientID: "client-b",
			ClientName: "B", RedirectURIsJSON: `["https://b.example/cb"]`, AuthMethod: "none", ApplicationType: "web",
			MaxClients: 10, Now: now, ExpiresAt: now.Add(time.Hour),
		},
	}
	type outcome struct {
		index  int
		result *models.DynamicClientRegistrationResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(registrations))
	var wg sync.WaitGroup
	for index := range registrations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := store.RegisterDynamicOAuthClient(ctx, registrations[index])
			outcomes <- outcome{index: index, result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	winner := -1
	for item := range outcomes {
		switch {
		case item.err == nil:
			if winner != -1 {
				t.Fatal("more than one registration consumed a one-client token")
			}
			winner = item.index
		case errors.Is(item.err, storeport.ErrDCRInitialAccessTokenQuota):
		default:
			t.Fatalf("unexpected concurrent registration error: %v", item.err)
		}
	}
	if winner == -1 {
		t.Fatal("no registration consumed the available quota")
	}
	replay := registrations[winner]
	replay.CandidateClientID = "unused-retry-candidate"
	if _, err := store.RegisterDynamicOAuthClient(ctx, replay); !errors.Is(err, storeport.ErrDCRInitialAccessTokenQuota) {
		t.Fatalf("replay at exhausted quota err=%v", err)
	}
	var used, clients int
	if err := store.root.QueryRow(rb(store,
		`SELECT used_registrations FROM oauth_dcr_initial_access_tokens WHERE token_id = ?`), token.TokenID,
	).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := store.root.QueryRow(`SELECT COUNT(*) FROM oauth_clients`).Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if used != 1 || clients != 1 {
		t.Fatalf("used=%d clients=%d, want 1/1", used, clients)
	}

	applied, err := store.RevokeDCRInitialAccessToken(ctx, token.TokenID, "operator", "test complete", now.Add(time.Minute))
	if err != nil || !applied {
		t.Fatalf("first revoke applied=%v err=%v", applied, err)
	}
	applied, err = store.RevokeDCRInitialAccessToken(ctx, token.TokenID, "operator", "idempotent replay", now.Add(2*time.Minute))
	if err != nil || applied {
		t.Fatalf("second revoke applied=%v err=%v", applied, err)
	}
	if _, err := store.GetActiveDCRInitialAccessToken(ctx, token.TokenHash, now.Add(3*time.Minute)); !errors.Is(err, storeport.ErrDCRInvalidInitialAccessToken) {
		t.Fatalf("revoked token remained active: %v", err)
	}
}

func TestCleanupExpiredOAuthClientsWritesDurableDCRAudit(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateOAuthClientWithSecretCappedTTL(
		ctx, "expired-audited-client", "Expired", `["https://expired.example/cb"]`, "", 10, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Exec(rb(store,
		`UPDATE oauth_clients
		 SET expires_at = ?, registration_token_id = ?, registration_fingerprint = ?
		 WHERE client_id = ?`),
		now.Add(-time.Hour), "retired-token", dcrTestHash("expired-profile"), "expired-audited-client",
	); err != nil {
		t.Fatal(err)
	}
	removed, err := store.CleanupExpiredOAuthClients(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	events, err := store.ListDCRAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "client_expired_deleted" || events[0].ClientID != "expired-audited-client" || events[0].TokenID != "retired-token" {
		t.Fatalf("cleanup audit=%+v", events)
	}
}

func TestDCRConfidentialRegistrationNeverReplaysStoredSecret(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	store.SetIntegrationSecretKeyring(testSecretKeyring(t, "old:"+encodedSecretKey(30), "old"))
	registration := models.DynamicClientRegistration{
		Fingerprint: dcrTestHash("confidential-profile"), CandidateClientID: "confidential-client",
		ClientName: "Confidential", RedirectURIsJSON: `["https://client.example/cb"]`,
		AuthMethod: "client_secret_basic", ApplicationType: "web",
		CandidateClientSecret: "private-client-secret", CandidateClientSecretHash: "$2a$04$placeholder",
		MaxClients: 10, Now: now, ExpiresAt: now.Add(time.Hour),
	}
	first, err := store.RegisterDynamicOAuthClient(ctx, registration)
	if err != nil || first.Replayed || first.ClientSecret != registration.CandidateClientSecret {
		t.Fatalf("first registration=%+v err=%v", first, err)
	}
	var envelope string
	if err := store.root.QueryRow(`SELECT registration_secret_ciphertext FROM oauth_clients WHERE client_id = 'confidential-client'`).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope != "" {
		t.Fatal("confidential registration persisted a recoverable secret")
	}
	retry := registration
	retry.CandidateClientID = "second-confidential-client"
	retry.CandidateClientSecret = "another-client-secret"
	second, err := store.RegisterDynamicOAuthClient(ctx, retry)
	if err != nil || second.Replayed || second.ClientID != retry.CandidateClientID || second.ClientSecret != retry.CandidateClientSecret {
		t.Fatalf("equivalent confidential registration=%+v err=%v", second, err)
	}
}

func TestDCRConfidentialRegistrationFromOtherTokenNeverReceivesSecret(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"token-a", "token-b"} {
		if err := store.CreateDCRInitialAccessToken(ctx, models.DCRInitialAccessToken{
			TokenID: id, TokenHash: dcrTestHash(id + "-secret"), Label: id,
			MaxRegistrations: 1, CreatedAt: now, CreatedBy: "test",
		}, ""); err != nil {
			t.Fatal(err)
		}
	}
	base := models.DynamicClientRegistration{
		TokenID: "token-a", Fingerprint: dcrTestHash("shared-confidential"), CandidateClientID: "client-a",
		ClientName: "Shared", RedirectURIsJSON: `["https://shared.example/cb"]`,
		AuthMethod: "client_secret_post", ApplicationType: "web",
		CandidateClientSecret: "secret-a", CandidateClientSecretHash: "$2a$04$placeholder",
		MaxClients: 10, Now: now, ExpiresAt: now.Add(time.Hour),
	}
	if _, err := store.RegisterDynamicOAuthClient(ctx, base); err != nil {
		t.Fatal(err)
	}
	other := base
	other.TokenID, other.CandidateClientID, other.CandidateClientSecret = "token-b", "client-b", "secret-b"
	result, err := store.RegisterDynamicOAuthClient(ctx, other)
	if err != nil || result.Replayed || result.ClientID != "client-b" || result.ClientSecret != "secret-b" {
		t.Fatalf("cross-token registration=%+v err=%v", result, err)
	}
	// Both capabilities are now exhausted; an equivalent request is refused
	// before any replay lookup.
	again := base
	again.CandidateClientID = "client-a-retry"
	if _, err := store.RegisterDynamicOAuthClient(ctx, again); !errors.Is(err, storeport.ErrDCRInitialAccessTokenQuota) {
		t.Fatalf("registration at quota err=%v", err)
	}
}

func TestDCRPublicReplayAtQuotaIsRefused(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.CreateDCRInitialAccessToken(ctx, models.DCRInitialAccessToken{
		TokenID: "one-shot", TokenHash: dcrTestHash("one-shot-secret"), Label: "one",
		MaxRegistrations: 1, CreatedAt: now, CreatedBy: "test",
	}, ""); err != nil {
		t.Fatal(err)
	}
	registration := models.DynamicClientRegistration{
		TokenID: "one-shot", Fingerprint: dcrTestHash("public-profile"), CandidateClientID: "public-client",
		ClientName: "Public", RedirectURIsJSON: `["https://public.example/cb"]`, AuthMethod: "none",
		ApplicationType: "web", MaxClients: 10, Now: now, ExpiresAt: now.Add(time.Hour),
	}
	if _, err := store.RegisterDynamicOAuthClient(ctx, registration); err != nil {
		t.Fatal(err)
	}
	registration.CandidateClientID = "unused"
	if _, err := store.RegisterDynamicOAuthClient(ctx, registration); !errors.Is(err, storeport.ErrDCRInitialAccessTokenQuota) {
		t.Fatalf("replay at quota err=%v", err)
	}
}

func TestDCRCapEvictsOldestProvisionalClient(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	register := func(id string, at time.Time) error {
		_, err := store.RegisterDynamicOAuthClient(ctx, models.DynamicClientRegistration{
			Fingerprint: dcrTestHash(id), CandidateClientID: id, ClientName: id,
			RedirectURIsJSON: `["https://` + id + `.example/cb"]`, AuthMethod: "none", ApplicationType: "web",
			MaxClients: 3, Now: at, ExpiresAt: at.Add(30 * 24 * time.Hour),
		})
		return err
	}
	if err := register("oldest", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := register("activated", now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Exec(`UPDATE oauth_clients SET expires_at = NULL WHERE client_id = 'activated'`); err != nil {
		t.Fatal(err)
	}
	if err := register("recent", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := register("incoming", now); err != nil {
		t.Fatalf("registration at cap with an evictable client: %v", err)
	}
	var remaining []string
	rows, err := store.root.Query(`SELECT client_id FROM oauth_clients ORDER BY client_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		remaining = append(remaining, id)
	}
	_ = rows.Close()
	if strings.Join(remaining, ",") != "activated,incoming,recent" {
		t.Fatalf("remaining clients=%v", remaining)
	}
	// Only activated and in-grace clients remain: the cap now holds.
	if err := register("overflow", now.Add(time.Second)); !errors.Is(err, storeport.ErrOAuthClientLimitReached) {
		t.Fatalf("registration without evictable client err=%v", err)
	}
}

func TestDCRLegacyReplayEnvelopeRotatesAndAuthenticates(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	store.SetIntegrationSecretKeyring(testSecretKeyring(t, "old:"+encodedSecretKey(30), "old"))
	fingerprint := dcrTestHash("confidential-profile")
	if err := store.CreateOAuthClientWithSecretCappedTTL(
		ctx, "confidential-client", "Confidential", `["https://client.example/cb"]`, "$2a$04$placeholder", 10, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.secretKeyring.encryptWithAAD("private-client-secret", dcrRegistrationSecretAAD("confidential-client", fingerprint))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Exec(rb(store,
		`UPDATE oauth_clients SET registration_fingerprint = ?, registration_secret_ciphertext = ? WHERE client_id = 'confidential-client'`),
		fingerprint, legacy,
	); err != nil {
		t.Fatal(err)
	}
	store.SetIntegrationSecretKeyring(testSecretKeyring(t,
		"old:"+encodedSecretKey(30)+",new:"+encodedSecretKey(31), "new",
	))
	rotated, err := store.RotateIntegrationSecrets(ctx)
	if err != nil || rotated != 1 {
		t.Fatalf("rotated=%d err=%v", rotated, err)
	}
	var envelope string
	if err := store.root.QueryRow(`SELECT registration_secret_ciphertext FROM oauth_clients WHERE client_id = 'confidential-client'`).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(envelope, integrationSecretPrefix+"new:") {
		t.Fatalf("DCR envelope did not rotate: %q", envelope)
	}
	store.SetIntegrationSecretKeyring(testSecretKeyring(t, "new:"+encodedSecretKey(31), "new"))

	alterAt := len(integrationSecretPrefix) + len("new:") + 5
	replacement := byte('A')
	if envelope[alterAt] == replacement {
		replacement = 'B'
	}
	tampered := envelope[:alterAt] + string(replacement) + envelope[alterAt+1:]
	if _, err := store.root.Exec(rb(store,
		`UPDATE oauth_clients SET registration_secret_ciphertext = ? WHERE client_id = 'confidential-client'`), tampered,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RotateIntegrationSecrets(ctx); err == nil {
		t.Fatal("rotation accepted a tampered current-key DCR envelope")
	}
}

// hideApprovalsFromGlobalDeletes makes SQLite behave like PostgreSQL forced
// row-level security for a transaction without a tenant: the global delete of
// learner approvals silently affects nothing, so the client row stays
// referenced.
func hideApprovalsFromGlobalDeletes(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.root.Exec(`CREATE TRIGGER hide_learner_approvals BEFORE DELETE ON learner_approved_clients
		BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
}

func registerDCRTestClient(store *Store, id string, at time.Time, maxClients int) error {
	_, err := store.RegisterDynamicOAuthClient(context.Background(), models.DynamicClientRegistration{
		Fingerprint: dcrTestHash(id), CandidateClientID: id, ClientName: id,
		RedirectURIsJSON: `["https://` + id + `.example/cb"]`, AuthMethod: "none", ApplicationType: "web",
		MaxClients: maxClients, Now: at, ExpiresAt: at.Add(30 * 24 * time.Hour),
	})
	return err
}

func dcrClientIDs(t *testing.T, store *Store) string {
	t.Helper()
	rows, err := store.root.Query(`SELECT client_id FROM oauth_clients ORDER BY client_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

func TestDCRCapEvictionSkipsClientWithUnremovableApproval(t *testing.T) {
	store := setupTestDB(t)
	if store.dialect != DialectSQLite {
		t.Skip("SQLite emulation of hidden tenant rows")
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedLearner(t, store, "L-evict")
	if err := registerDCRTestClient(store, "approved-old", now.Add(-72*time.Hour), 2); err != nil {
		t.Fatal(err)
	}
	if err := store.ApproveClient(ctx, "L-evict", "approved-old", "https://approved-old.example/cb"); err != nil {
		t.Fatal(err)
	}
	if err := registerDCRTestClient(store, "recent", now.Add(-time.Minute), 2); err != nil {
		t.Fatal(err)
	}
	hideApprovalsFromGlobalDeletes(t, store)

	if err := registerDCRTestClient(store, "incoming", now, 2); !errors.Is(err, storeport.ErrOAuthClientLimitReached) {
		t.Fatalf("blocked eviction err=%v, want client limit reached", err)
	}
	if got := dcrClientIDs(t, store); got != "approved-old,recent" {
		t.Fatalf("clients after blocked eviction=%s", got)
	}
	if ok, err := store.IsClientApproved(ctx, "L-evict", "approved-old", "https://approved-old.example/cb"); err != nil || !ok {
		t.Fatalf("rolled-back eviction lost the approval: ok=%v err=%v", ok, err)
	}

	// A later evictable candidate is still used when the oldest is blocked.
	if _, err := store.root.Exec(`DROP TRIGGER hide_learner_approvals`); err != nil {
		t.Fatal(err)
	}
	if err := registerDCRTestClient(store, "free-old", now.Add(-48*time.Hour), 3); err != nil {
		t.Fatal(err)
	}
	hideApprovalsFromGlobalDeletes(t, store)
	if err := registerDCRTestClient(store, "incoming", now, 3); err != nil {
		t.Fatalf("registration with a second evictable candidate: %v", err)
	}
	if got := dcrClientIDs(t, store); got != "approved-old,incoming,recent" {
		t.Fatalf("clients after eviction=%s", got)
	}
}

func TestCleanupExpiredOAuthClientsSkipsClientWithUnremovableApproval(t *testing.T) {
	store := setupTestDB(t)
	if store.dialect != DialectSQLite {
		t.Skip("SQLite emulation of hidden tenant rows")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	seedLearner(t, store, "L-cleanup")
	for _, id := range []string{"expired-approved", "expired-free"} {
		if err := store.CreateOAuthClientWithSecretCappedTTL(ctx, id, id, `["https://`+id+`.example/cb"]`, "", 10, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.root.Exec(`UPDATE oauth_clients SET expires_at = ? WHERE client_id = ?`, now.Add(-time.Hour), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ApproveClient(ctx, "L-cleanup", "expired-approved", "https://expired-approved.example/cb"); err != nil {
		t.Fatal(err)
	}
	hideApprovalsFromGlobalDeletes(t, store)

	removed, err := store.CleanupExpiredOAuthClients(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	if got := dcrClientIDs(t, store); got != "expired-approved" {
		t.Fatalf("clients after cleanup=%s", got)
	}
	events, err := store.ListDCRAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ClientID != "expired-free" {
		t.Fatalf("cleanup audit=%+v", events)
	}
}

func TestDCRCapEvictionPostgresForcedRLS(t *testing.T) {
	dsn := os.Getenv("TUTOR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TUTOR_TEST_PG_DSN is not configured")
	}
	store := setupTestPG(t, dsn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedLearner(t, store, "L-evict")
	for _, id := range []string{"approved-old", "recent"} {
		at := now.Add(-time.Minute)
		if id == "approved-old" {
			at = now.Add(-72 * time.Hour)
		}
		if err := registerDCRTestClient(store, id, at, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ApproveClient(ctx, "L-evict", "approved-old", "https://approved-old.example/cb"); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := store.root.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf(`"tutor_dcr_rls_%d"`, testDBCounter.Add(1))
	if _, err := store.root.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.root.ExecContext(context.Background(), `DROP OWNED BY `+role)
		_, _ = store.root.ExecContext(context.Background(), `DROP ROLE `+role)
	}()
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role,
	} {
		if _, err := store.root.ExecContext(ctx, grant); err != nil {
			t.Fatal(err)
		}
	}
	asLimitedRole := func(fn func(scoped *Store) error) error {
		return store.WithTx(ctx, func(tx storeport.Store) error {
			scoped := tx.(*Store)
			if _, err := scoped.exec(ctx, `SET LOCAL ROLE `+role); err != nil {
				return err
			}
			return fn(scoped)
		})
	}

	err := asLimitedRole(func(scoped *Store) error {
		return registerDCRTestClient(scoped, "incoming", now, 2)
	})
	if !errors.Is(err, storeport.ErrOAuthClientLimitReached) {
		t.Fatalf("eviction blocked by a hidden approval err=%v, want client limit reached", err)
	}
	if got := dcrClientIDs(t, store); got != "approved-old,recent" {
		t.Fatalf("clients after blocked eviction=%s", got)
	}

	if _, err := store.root.ExecContext(ctx, `UPDATE oauth_clients SET expires_at = $1`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var removed int64
	if err := asLimitedRole(func(scoped *Store) error {
		var err error
		removed, err = scoped.CleanupExpiredOAuthClients(ctx)
		return err
	}); err != nil || removed != 1 {
		t.Fatalf("cleanup under forced RLS removed=%d err=%v", removed, err)
	}
	if got := dcrClientIDs(t, store); got != "approved-old" {
		t.Fatalf("clients after cleanup=%s", got)
	}
}
