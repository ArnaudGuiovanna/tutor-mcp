package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	storeport "tutor-mcp/store"
)

func TestConsumeAuthCode_WrongClientID(t *testing.T) {
	store := setupTestDB(t)

	if err := store.CreateOAuthClient(context.Background(), "client-A", "Client A", `["https://a.example/cb"]`); err != nil {
		t.Fatalf("create client A: %v", err)
	}
	if err := store.CreateOAuthClient(context.Background(), "client-B", "Client B", `["https://b.example/cb"]`); err != nil {
		t.Fatalf("create client B: %v", err)
	}

	expires := time.Now().Add(5 * time.Minute)
	if err := store.CreateAuthCode(context.Background(), "code-1", "L1", "chal", "client-A", expires); err != nil {
		t.Fatalf("create code: %v", err)
	}

	if _, err := store.ConsumeAuthCode(context.Background(), "code-1", "client-B"); err == nil {
		t.Fatal("expected error when consuming with wrong client_id")
	}

	// Code must still be present after failed consume attempt
	var count int
	if err := store.root.QueryRow(rb(store, `SELECT COUNT(*) FROM oauth_codes WHERE code = ?`), "code-1").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected code still present, got count=%d", count)
	}

	ac, err := store.ConsumeAuthCode(context.Background(), "code-1", "client-A")
	if err != nil {
		t.Fatalf("consume with correct client: %v", err)
	}
	if ac.ClientID != "client-A" || ac.LearnerID != "L1" {
		t.Fatalf("unexpected ac: %+v", ac)
	}

	if err := store.root.QueryRow(rb(store, `SELECT COUNT(*) FROM oauth_codes WHERE code = ?`), "code-1").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected code consumed, got count=%d", count)
	}
}

func TestConsumeAuthCode_ConcurrentSingleWinner(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	if err := s.CreateAuthCode(ctx, "code-race", "L1", "challenge", "client-A", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("create code: %v", err)
	}

	const contenders = 12
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.ConsumeAuthCode(ctx, "code-race", "client-A")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent consumptions = %d, want exactly 1", successes)
	}
}

func TestRefreshToken_HashedAtRestAndDigestCannotAuthenticate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	rt, err := s.CreateRefreshToken(ctx, "L1", "client-A")
	if err != nil {
		t.Fatalf("create refresh token: %v", err)
	}

	var stored string
	if err := s.root.QueryRow(`SELECT token FROM refresh_tokens`).Scan(&stored); err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if stored == rt.Token {
		t.Fatal("refresh token was stored in plaintext")
	}
	if stored != refreshTokenHash(rt.Token) {
		t.Fatalf("stored credential = %q, want SHA-256 digest", stored)
	}
	if _, err := s.GetRefreshToken(ctx, stored); err == nil {
		t.Fatal("database digest must not be accepted as a bearer refresh token")
	}
	if got, err := s.GetRefreshToken(ctx, rt.Token); err != nil || got.LearnerID != "L1" {
		t.Fatalf("raw refresh token lookup failed: got=%+v err=%v", got, err)
	}
}

func TestRotateRefreshToken_LegacyPlaintextUpgradesToHash(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := s.root.Exec(
		rb(s, `INSERT INTO refresh_tokens (token, learner_id, client_id, expires_at, created_at)
		 VALUES ('legacy-token', 'L1', NULL, ?, ?)`),
		now.Add(time.Hour), now,
	)
	if err != nil {
		t.Fatalf("seed legacy token: %v", err)
	}

	successor, err := s.RotateRefreshToken(ctx, "legacy-token", "client-A")
	if err != nil {
		t.Fatalf("rotate legacy token: %v", err)
	}
	if _, err := s.GetRefreshToken(ctx, "legacy-token"); err == nil {
		t.Fatal("legacy plaintext token remained usable after rotation")
	}
	var stored string
	if err := s.root.QueryRow(`SELECT token FROM refresh_tokens`).Scan(&stored); err != nil {
		t.Fatalf("read successor: %v", err)
	}
	if stored != refreshTokenHash(successor.Token) {
		t.Fatalf("rotated token was not stored as a digest: %q", stored)
	}
}

func TestRotateRefreshToken_ConcurrentReuseSingleWinner(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	rt, err := s.CreateRefreshToken(ctx, "L1", "client-A")
	if err != nil {
		t.Fatalf("create refresh token: %v", err)
	}

	const contenders = 12
	start := make(chan struct{})
	type result struct {
		token string
		err   error
	}
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			next, err := s.RotateRefreshToken(ctx, rt.Token, "client-A")
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{token: next.Token}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var successor string
	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
			successor = result.token
			continue
		}
		if !errors.Is(result.err, storeport.ErrInvalidRefreshToken) {
			t.Fatalf("loser error = %v, want ErrInvalidRefreshToken", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent rotations = %d, want exactly 1", successes)
	}
	if _, err := s.GetRefreshToken(ctx, rt.Token); err == nil {
		t.Fatal("consumed refresh token remained usable")
	}
	if _, err := s.GetRefreshToken(ctx, successor); err != nil {
		t.Fatalf("winning successor is not usable: %v", err)
	}
	var active int
	if err := s.root.QueryRow(`SELECT COUNT(*) FROM refresh_tokens`).Scan(&active); err != nil {
		t.Fatalf("count active refresh tokens: %v", err)
	}
	if active != 1 {
		t.Fatalf("active refresh token rows = %d, want 1", active)
	}
}

func TestRotateRefreshToken_SuccessorFailureRollsBackConsumption(t *testing.T) {
	s := setupTestDB(t)
	if s.dialect != DialectSQLite {
		t.Skip("SQLite trigger fault injection; transaction semantics are exercised on PostgreSQL by the concurrency test")
	}
	ctx := context.Background()
	rt, err := s.CreateRefreshToken(ctx, "L1", "client-A")
	if err != nil {
		t.Fatalf("create refresh token: %v", err)
	}
	if _, err := s.root.Exec(`
		CREATE TRIGGER fail_refresh_successor
		BEFORE INSERT ON refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected successor failure');
		END`); err != nil {
		t.Fatalf("create fault trigger: %v", err)
	}

	if _, err := s.RotateRefreshToken(ctx, rt.Token, "client-A"); err == nil {
		t.Fatal("rotation unexpectedly succeeded despite injected insert failure")
	}
	if got, err := s.GetRefreshToken(ctx, rt.Token); err != nil || got.Token != rt.Token {
		t.Fatalf("old token was not restored by rollback: got=%+v err=%v", got, err)
	}
}

func TestGetOAuthClient(t *testing.T) {
	store := setupTestDB(t)
	if err := store.CreateOAuthClient(context.Background(), "c1", "n1", `["https://x.example/cb"]`); err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := store.GetOAuthClient(context.Background(), "c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if c.ClientID != "c1" || c.ClientName != "n1" || c.RedirectURIs != `["https://x.example/cb"]` {
		t.Fatalf("unexpected client: %+v", c)
	}
	if _, err := store.GetOAuthClient(context.Background(), "missing"); err == nil {
		t.Fatal("expected error for missing client")
	}
}

func TestCountOAuthClients(t *testing.T) {
	store := setupTestDB(t)
	if err := store.CreateOAuthClient(context.Background(), "c1", "n1", `["https://x.example/cb"]`); err != nil {
		t.Fatalf("create c1: %v", err)
	}
	if err := store.CreateOAuthClient(context.Background(), "c2", "n2", `["https://y.example/cb"]`); err != nil {
		t.Fatalf("create c2: %v", err)
	}

	got, err := store.CountOAuthClients(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
}

func TestCreateOAuthClientWithSecretCappedRejectsAtLimit(t *testing.T) {
	store := setupTestDB(t)
	if err := store.CreateOAuthClientWithSecretCapped(context.Background(), "c1", "n1", `["https://x.example/cb"]`, "", 1); err != nil {
		t.Fatalf("create c1: %v", err)
	}

	err := store.CreateOAuthClientWithSecretCapped(context.Background(), "c2", "n2", `["https://y.example/cb"]`, "", 1)
	if !errors.Is(err, storeport.ErrOAuthClientLimitReached) {
		t.Fatalf("err = %v, want ErrOAuthClientLimitReached", err)
	}
	got, err := store.CountOAuthClients(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}
