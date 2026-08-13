// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
	replayed, err := store.RegisterDynamicOAuthClient(ctx, replay)
	if err != nil || !replayed.Replayed || replayed.ClientID != registrations[winner].CandidateClientID {
		t.Fatalf("quota-safe replay=%+v err=%v", replayed, err)
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

func TestDCRReplaySecretRotatesAndAuthenticatesCurrentEnvelope(t *testing.T) {
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
	if _, err := store.RegisterDynamicOAuthClient(ctx, registration); err != nil {
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
	retry := registration
	retry.CandidateClientID = "unused-retry"
	replayed, err := store.RegisterDynamicOAuthClient(ctx, retry)
	if err != nil || !replayed.Replayed || replayed.ClientSecret != registration.CandidateClientSecret {
		t.Fatalf("post-rotation replay=%+v err=%v", replayed, err)
	}

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
