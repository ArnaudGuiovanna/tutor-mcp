package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func verificationToken(learnerID, hash string) *models.AccountToken {
	now := time.Now().UTC()
	return &models.AccountToken{
		TokenHash:           hash,
		LearnerID:           learnerID,
		Purpose:             accountTokenEmailVerification,
		ClientID:            "client-A",
		RedirectURI:         "https://client.example/callback",
		Resource:            testOAuthResource,
		State:               "opaque-state",
		Scope:               "learner",
		CodeChallenge:       "pkce-challenge",
		CodeChallengeMethod: "S256",
		ExpiresAt:           now.Add(30 * time.Minute),
		CreatedAt:           now,
	}
}

func TestExpiredAccountTokenIsRejected(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	learner, err := s.CreateUnverifiedLearner(ctx, "expired@example.test", "hash", "learn", "")
	if err != nil {
		t.Fatal(err)
	}
	token := verificationToken(learner.ID, "sha256:expired")
	token.CreatedAt = time.Now().Add(-time.Hour)
	token.ExpiresAt = time.Now().Add(-time.Minute)
	if err := s.CreateAccountToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccountToken(ctx, token.TokenHash, accountTokenEmailVerification); !errors.Is(err, storeport.ErrInvalidAccountToken) {
		t.Fatalf("get expired token error=%v", err)
	}
	if _, err := s.ActivateLearnerAndCreateAuthCode(ctx, token.TokenHash, "owner-hash", "expired-code", time.Now().Add(time.Minute), false); !errors.Is(err, storeport.ErrInvalidAccountToken) {
		t.Fatalf("verify expired token error=%v", err)
	}
}

func TestLegacyBlankEmailVerificationScopeIsReadAsBoundedLearnerGrant(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	learner, err := s.CreateUnverifiedLearner(ctx, "rolling-scope@example.test", "hash", "learn", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.root.Exec(rb(s, `INSERT INTO account_tokens
		(token_hash, learner_id, purpose, client_id, redirect_uri, resource, state, scope,
		 code_challenge, code_challenge_method, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, '', '', ?, 'S256', ?, ?)`),
		"sha256:rolling-account-scope", learner.ID, accountTokenEmailVerification,
		"client-A", "https://client.example/callback", testOAuthResource,
		"pkce-challenge", now.Add(time.Hour), now,
	); err != nil {
		t.Fatal(err)
	}
	token, err := s.GetAccountToken(ctx, "sha256:rolling-account-scope", accountTokenEmailVerification)
	if err != nil {
		t.Fatal(err)
	}
	if token.Scope != models.OAuthScopeLearner {
		t.Fatalf("legacy blank account-token scope = %q, want %q", token.Scope, models.OAuthScopeLearner)
	}
}

func TestConcurrentEmailVerificationHasSingleWinner(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	learner, err := s.CreateUnverifiedLearner(ctx, "race@example.test", "hash", "learn", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccountToken(ctx, verificationToken(learner.ID, "sha256:race")); err != nil {
		t.Fatal(err)
	}

	const contenders = 8
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, verifyErr := s.ActivateLearnerAndCreateAuthCode(
				ctx, "sha256:race", "owner-hash", fmt.Sprintf("race-code-%d", i), time.Now().Add(time.Minute), false,
			)
			results <- verifyErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, storeport.ErrInvalidAccountToken) {
			t.Fatalf("unexpected contender error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("verification winners=%d, want 1", winners)
	}
	var codeCount int
	if err := s.root.QueryRow(rb(s, `SELECT COUNT(*) FROM oauth_codes WHERE learner_id = ?`), learner.ID).Scan(&codeCount); err != nil || codeCount != 1 {
		t.Fatalf("authorization codes=%d err=%v", codeCount, err)
	}
}

func TestActivateLearnerAndCreateAuthCodeIsAtomicAndSingleUse(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	learner, err := s.CreateUnverifiedLearner(ctx, "new@example.test", "attacker-hash", "learn", "")
	if err != nil {
		t.Fatalf("create learner: %v", err)
	}
	if learner.EmailVerifiedAt != nil {
		t.Fatal("new public learner unexpectedly verified")
	}
	if err := s.CreateAccountToken(ctx, verificationToken(learner.ID, "sha256:verification")); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := s.CreateOAuthClient(ctx, "client-A", "Owner-approved client", `["https://client.example/callback"]`); err != nil {
		t.Fatalf("create approved client: %v", err)
	}
	if err := s.CreateOAuthClient(ctx, "attacker-client", "Attacker", `["https://attacker.example/callback"]`); err != nil {
		t.Fatalf("create attacker client: %v", err)
	}
	if err := s.ApproveClient(ctx, learner.ID, "attacker-client", "https://attacker.example/callback"); err != nil {
		t.Fatalf("seed stale attacker approval: %v", err)
	}

	token, err := s.ActivateLearnerAndCreateAuthCode(
		ctx, "sha256:verification", "owner-hash", "auth-code", time.Now().Add(5*time.Minute), true,
	)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if token.State != "opaque-state" || token.Resource != testOAuthResource {
		t.Fatalf("OAuth continuation changed: %+v", token)
	}
	got, err := s.GetLearnerByID(ctx, learner.ID)
	if err != nil || got.EmailVerifiedAt == nil || got.PasswordHash != "owner-hash" {
		t.Fatalf("learner not verified: learner=%+v err=%v", got, err)
	}
	approved, err := s.IsClientApproved(ctx, learner.ID, "client-A", "https://client.example/callback")
	if err != nil || !approved {
		t.Fatalf("verified client approval missing: approved=%v err=%v", approved, err)
	}
	stale, err := s.IsClientApproved(ctx, learner.ID, "attacker-client", "https://attacker.example/callback")
	if err != nil || stale {
		t.Fatalf("stale attacker approval survived activation: approved=%v err=%v", stale, err)
	}
	if _, err := s.GetAuthCode(ctx, "auth-code", "client-A"); err != nil {
		t.Fatalf("created code not redeemable: %v", err)
	}
	if _, err := s.ActivateLearnerAndCreateAuthCode(
		ctx, "sha256:verification", "other-hash", "second-code", time.Now().Add(5*time.Minute), true,
	); !errors.Is(err, storeport.ErrInvalidAccountToken) {
		t.Fatalf("replay error = %v, want invalid account token", err)
	}
}

func TestActivationTokenCannotOverwriteAlreadyVerifiedCredential(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	learner, err := s.CreateLearner(ctx, "verified@example.test", "owner-existing-hash", "learn", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccountToken(ctx, verificationToken(learner.ID, "sha256:stale-verification")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateLearnerAndCreateAuthCode(
		ctx, "sha256:stale-verification", "attacker-replacement-hash", "stale-code",
		time.Now().Add(5*time.Minute), false,
	); !errors.Is(err, storeport.ErrInvalidAccountToken) {
		t.Fatalf("stale activation error=%v, want invalid account token", err)
	}
	got, err := s.GetLearnerByID(ctx, learner.ID)
	if err != nil || got.PasswordHash != "owner-existing-hash" {
		t.Fatalf("stale token changed verified credential: learner=%+v err=%v", got, err)
	}
	var codeCount int
	if err := s.root.QueryRow(rb(s, `SELECT COUNT(*) FROM oauth_codes WHERE learner_id = ?`), learner.ID).Scan(&codeCount); err != nil || codeCount != 0 {
		t.Fatalf("stale token created %d codes: %v", codeCount, err)
	}
}

func TestUnverifiedLearnerCannotRedeemAuthorizationCode(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	learner, err := s.CreateUnverifiedLearner(ctx, "unverified@example.test", "hash", "learn", "")
	if err != nil {
		t.Fatalf("create learner: %v", err)
	}
	if err := s.CreateAuthCodeWithBinding(
		ctx, "should-be-rejected", learner.ID, "challenge", "S256", "client-A",
		"https://client.example/callback", testOAuthResource, time.Now().Add(time.Minute),
	); err != nil {
		t.Fatalf("seed code: %v", err)
	}
	if _, err := s.GetAuthCode(ctx, "should-be-rejected", "client-A"); !errors.Is(err, storeport.ErrInvalidAuthCode) {
		t.Fatalf("get code error = %v, want invalid auth code", err)
	}
	if _, err := s.ConsumeAuthCode(ctx, "should-be-rejected", "client-A"); !errors.Is(err, storeport.ErrInvalidAuthCode) {
		t.Fatalf("consume code error = %v, want invalid auth code", err)
	}
}

func TestResetPasswordRevokesAllDurableOAuthCredentials(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	rt, err := s.CreateRefreshToken(ctx, "L1", "client-A", testOAuthResource)
	if err != nil {
		t.Fatalf("create refresh token: %v", err)
	}
	if err := s.CreateAuthCodeWithBinding(
		ctx, "pending-code", "L1", "challenge", "S256", "client-A",
		"https://client.example/callback", testOAuthResource, time.Now().Add(time.Minute),
	); err != nil {
		t.Fatalf("create auth code: %v", err)
	}
	now := time.Now().UTC()
	if err := s.CreateAccountToken(ctx, &models.AccountToken{
		TokenHash: "sha256:reset", LearnerID: "L1", Purpose: accountTokenPasswordReset,
		CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}); err != nil {
		t.Fatalf("create reset token: %v", err)
	}

	learnerID, err := s.ResetPasswordWithToken(ctx, "sha256:reset", "new-password-hash")
	if err != nil || learnerID != "L1" {
		t.Fatalf("reset result learner=%q err=%v", learnerID, err)
	}
	learner, err := s.GetLearnerByID(ctx, "L1")
	if err != nil || learner.PasswordHash != "new-password-hash" {
		t.Fatalf("password not updated: learner=%+v err=%v", learner, err)
	}
	if _, err := s.GetRefreshToken(ctx, rt.Token); err == nil {
		t.Fatal("refresh token remains active after password reset")
	}
	if _, err := s.GetAuthCode(ctx, "pending-code", "client-A"); err == nil {
		t.Fatal("authorization code remains active after password reset")
	}
	if _, err := s.ResetPasswordWithToken(ctx, "sha256:reset", "another-hash"); !errors.Is(err, storeport.ErrInvalidAccountToken) {
		t.Fatalf("reset replay error = %v, want invalid account token", err)
	}
}
