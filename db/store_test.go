package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	storeport "tutor-mcp/store"
)

const testOAuthResource = "https://test.example/mcp"

func TestConsumeAuthCode_WrongClientID(t *testing.T) {
	store := setupTestDB(t)

	if err := store.CreateOAuthClient(context.Background(), "client-A", "Client A", `["https://a.example/cb"]`); err != nil {
		t.Fatalf("create client A: %v", err)
	}
	if err := store.CreateOAuthClient(context.Background(), "client-B", "Client B", `["https://b.example/cb"]`); err != nil {
		t.Fatalf("create client B: %v", err)
	}

	expires := time.Now().Add(5 * time.Minute)
	if err := store.CreateAuthCodeWithBinding(context.Background(), "code-1", "L1", "chal", "", "client-A", "", testOAuthResource, expires); err != nil {
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
	if err := s.CreateAuthCodeWithBinding(ctx, "code-race", "L1", "challenge", "", "client-A", "", testOAuthResource, time.Now().Add(time.Minute)); err != nil {
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

func TestExchangeAuthCodeForRefreshTokenRollsBackConsumeOnInsertFailure(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	if err := s.CreateOAuthClientWithSecretCappedTTL(
		ctx, "client-A", "Ephemeral", `["https://a.example/cb"]`, "", 10,
		time.Now().UTC().Add(time.Hour),
	); err != nil {
		t.Fatalf("create ephemeral client: %v", err)
	}
	if err := s.CreateAuthCodeWithBinding(
		ctx, "code-exchange-rollback", "L1", "challenge", "S256", "client-A",
		"https://a.example/cb", testOAuthResource, time.Now().Add(time.Minute),
	); err != nil {
		t.Fatalf("create auth code: %v", err)
	}
	if s.dialect == DialectPostgres {
		if _, err := s.root.Exec(`
			CREATE FUNCTION fail_exchange_refresh_insert() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'injected refresh insert failure';
			END
			$$`); err != nil {
			t.Fatalf("create fault function: %v", err)
		}
		if _, err := s.root.Exec(`
			CREATE TRIGGER fail_exchange_refresh
			BEFORE INSERT ON refresh_tokens
			FOR EACH ROW EXECUTE FUNCTION fail_exchange_refresh_insert()`); err != nil {
			t.Fatalf("create fault trigger: %v", err)
		}
	} else if _, err := s.root.Exec(`
		CREATE TRIGGER fail_exchange_refresh
		BEFORE INSERT ON refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected refresh insert failure');
		END`); err != nil {
		t.Fatalf("create fault trigger: %v", err)
	}

	if _, _, err := s.ExchangeAuthCodeForRefreshToken(ctx, "code-exchange-rollback", "client-A"); err == nil {
		t.Fatal("exchange unexpectedly succeeded")
	}
	var expiresAt sql.NullTime
	if err := s.root.QueryRow(rb(s, `SELECT expires_at FROM oauth_clients WHERE client_id = ?`), "client-A").Scan(&expiresAt); err != nil || !expiresAt.Valid {
		t.Fatalf("client activated despite rolled-back exchange: expires=%v err=%v", expiresAt, err)
	}
	if _, err := s.ConsumeAuthCode(ctx, "code-exchange-rollback", "client-A"); err != nil {
		t.Fatalf("authorization code was consumed despite rolled-back refresh insert: %v", err)
	}
}

func TestExchangeAuthCodeActivatesDynamicClientIdempotently(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	const clientID = "activate-dcr"
	if err := s.CreateOAuthClientWithSecretCappedTTL(
		ctx, clientID, "Dynamic", `["https://client.example/cb"]`, "", 10,
		time.Now().UTC().Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	exchange := func(code string) {
		t.Helper()
		if err := s.CreateAuthCodeWithBinding(
			ctx, code, "L1", "challenge", "S256", clientID,
			"https://client.example/cb", testOAuthResource, time.Now().UTC().Add(time.Minute),
		); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ExchangeAuthCodeForRefreshToken(ctx, code, clientID); err != nil {
			t.Fatalf("exchange %q: %v", code, err)
		}
		var expiresAt sql.NullTime
		if err := s.root.QueryRow(rb(s, `SELECT expires_at FROM oauth_clients WHERE client_id = ?`), clientID).Scan(&expiresAt); err != nil {
			t.Fatal(err)
		}
		if expiresAt.Valid {
			t.Fatalf("exchange %q left dynamic expiry %v", code, expiresAt.Time)
		}
	}
	exchange("activate-code-1")
	exchange("activate-code-2")
}

func TestExchangeAuthCodeRollsBackCredentialsOnActivationFailure(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	const clientID = "activation-failure"
	if err := s.CreateOAuthClientWithSecretCappedTTL(
		ctx, clientID, "Dynamic", `["https://client.example/cb"]`, "", 10,
		time.Now().UTC().Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAuthCodeWithBinding(
		ctx, "activation-failure-code", "L1", "challenge", "S256", clientID,
		"https://client.example/cb", testOAuthResource, time.Now().UTC().Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if s.dialect == DialectPostgres {
		if _, err := s.root.Exec(`
			CREATE FUNCTION fail_oauth_client_activation() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'injected activation failure';
			END
			$$`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.root.Exec(`
			CREATE TRIGGER fail_oauth_client_activation
			BEFORE UPDATE OF expires_at ON oauth_clients
			FOR EACH ROW WHEN (OLD.client_id = 'activation-failure')
			EXECUTE FUNCTION fail_oauth_client_activation()`); err != nil {
			t.Fatal(err)
		}
	} else if _, err := s.root.Exec(`
		CREATE TRIGGER fail_oauth_client_activation
		BEFORE UPDATE OF expires_at ON oauth_clients
		WHEN OLD.client_id = 'activation-failure'
		BEGIN
			SELECT RAISE(ABORT, 'injected activation failure');
		END`); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ExchangeAuthCodeForRefreshToken(ctx, "activation-failure-code", clientID); err == nil {
		t.Fatal("exchange unexpectedly survived activation failure")
	}
	var refreshCount int
	if err := s.root.QueryRow(rb(s, `SELECT COUNT(*) FROM refresh_tokens WHERE client_id = ?`), clientID).Scan(&refreshCount); err != nil || refreshCount != 0 {
		t.Fatalf("refresh insert was not rolled back: count=%d err=%v", refreshCount, err)
	}
	if _, err := s.ConsumeAuthCode(ctx, "activation-failure-code", clientID); err != nil {
		t.Fatalf("authorization code was consumed despite activation rollback: %v", err)
	}
}

func TestExchangeAuthCodeActivationAllowsUnpersistedCIMDClient(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	const clientID = "https://client.example/oauth-client.json"
	if err := s.CreateAuthCodeWithBinding(
		ctx, "cimd-code", "L1", "challenge", "S256", clientID,
		"https://client.example/cb", testOAuthResource, time.Now().UTC().Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, rt, err := s.ExchangeAuthCodeForRefreshToken(ctx, "cimd-code", clientID); err != nil || rt.ClientID != clientID {
		t.Fatalf("CIMD exchange rt=%+v err=%v", rt, err)
	}
}

func TestExchangeAuthCodeActivatesClientThatExpiresAfterAuthorization(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	const clientID = "expires-between-authorize-and-token"
	if err := s.CreateOAuthClientWithSecretCappedTTL(
		ctx, clientID, "Boundary", `["https://client.example/cb"]`, "", 10,
		time.Now().UTC().Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAuthCodeWithBinding(
		ctx, "boundary-code", "L1", "challenge", "S256", clientID,
		"https://client.example/cb", testOAuthResource, time.Now().UTC().Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.root.Exec(
		rb(s, `UPDATE oauth_clients SET expires_at = ? WHERE client_id = ?`),
		time.Now().UTC().Add(-time.Second), clientID,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ExchangeAuthCodeForRefreshToken(ctx, "boundary-code", clientID); err != nil {
		t.Fatal(err)
	}
	var expiresAt sql.NullTime
	if err := s.root.QueryRow(rb(s, `SELECT expires_at FROM oauth_clients WHERE client_id = ?`), clientID).Scan(&expiresAt); err != nil || expiresAt.Valid {
		t.Fatalf("boundary client not activated: expires=%v err=%v", expiresAt, err)
	}
}

func TestRefreshToken_HashedAtRestAndDigestCannotAuthenticate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	rt, err := s.CreateRefreshToken(ctx, "L1", "client-A", testOAuthResource)
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

func TestCreateRefreshTokenRequiresClientBinding(t *testing.T) {
	s := setupTestDB(t)
	if _, err := s.CreateRefreshToken(context.Background(), "L1", "", testOAuthResource); err == nil {
		t.Fatal("unbound refresh token was issued")
	}
}

func TestCleanupExpiredOAuthClientsInvalidatesRelatedCredentials(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	const clientID = "ephemeral-client"
	if err := s.CreateOAuthClientWithSecretCappedTTL(
		ctx, clientID, "Ephemeral", `["https://client.example/callback"]`, "", 10,
		time.Now().UTC().Add(-time.Minute),
	); err != nil {
		t.Fatalf("create expired client: %v", err)
	}
	if err := s.ApproveClient(ctx, "L1", clientID, "https://client.example/callback"); err != nil {
		t.Fatalf("approve client: %v", err)
	}
	if err := s.CreateAuthCodeWithBinding(
		ctx, "expired-client-code", "L1", "challenge", "S256", clientID,
		"https://client.example/callback", testOAuthResource, time.Now().UTC().Add(time.Hour),
	); err != nil {
		t.Fatalf("create code: %v", err)
	}
	rt, err := s.CreateRefreshToken(ctx, "L1", clientID, testOAuthResource)
	if err != nil {
		t.Fatalf("create refresh token: %v", err)
	}

	removed, err := s.CleanupExpiredOAuthClients(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v, want 1", removed, err)
	}
	if _, err := s.GetOAuthClient(ctx, clientID); err == nil {
		t.Fatal("expired OAuth client survived cleanup")
	}
	if _, err := s.GetAuthCode(ctx, "expired-client-code", clientID); !errors.Is(err, storeport.ErrInvalidAuthCode) {
		t.Fatalf("expired client auth code still valid: %v", err)
	}
	if _, err := s.GetRefreshToken(ctx, rt.Token); err == nil {
		t.Fatal("expired client refresh token still active")
	}
	approved, err := s.IsClientApproved(ctx, "L1", clientID, "https://client.example/callback")
	if err != nil || approved {
		t.Fatalf("expired client approval survived: approved=%v err=%v", approved, err)
	}
}

func TestRotateRefreshToken_RejectsAndRevokesUnboundLegacyToken(t *testing.T) {
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

	if _, err := s.RotateRefreshToken(ctx, "legacy-token", "client-A", testOAuthResource); !errors.Is(err, storeport.ErrInvalidRefreshToken) {
		t.Fatalf("rotate legacy token error = %v, want invalid refresh token", err)
	}
	if _, err := s.GetRefreshToken(ctx, "legacy-token"); err == nil {
		t.Fatal("legacy plaintext token remained usable after rotation")
	}
	var revoked sql.NullTime
	if err := s.root.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE token = 'legacy-token'`).Scan(&revoked); err != nil || !revoked.Valid {
		t.Fatalf("legacy token was not revoked: revoked=%v err=%v", revoked, err)
	}
	var count int
	if err := s.root.QueryRow(`SELECT COUNT(*) FROM refresh_tokens`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unexpected successor created: count=%d err=%v", count, err)
	}
}

func TestRotateRefreshToken_RejectsClientBoundPlaintextLegacyToken(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.root.Exec(rb(s,
		`INSERT INTO refresh_tokens
		 (token, learner_id, client_id, family_id, expires_at, created_at)
		 VALUES ('legacy-bound-plaintext', 'L1', 'client-A', 'legacy-bound-family', ?, ?)`),
		now.Add(time.Hour), now,
	); err != nil {
		t.Fatalf("seed legacy bound token: %v", err)
	}

	if _, err := s.RotateRefreshToken(ctx, "legacy-bound-plaintext", "client-A", testOAuthResource); !errors.Is(err, storeport.ErrInvalidRefreshToken) {
		t.Fatalf("rotate plaintext legacy token error = %v, want invalid refresh token", err)
	}
	if _, err := s.GetRefreshToken(ctx, "legacy-bound-plaintext"); err == nil {
		t.Fatal("client-bound plaintext legacy bearer was accepted")
	}
	var used sql.NullTime
	if err := s.root.QueryRow(`SELECT used_at FROM refresh_tokens WHERE token = 'legacy-bound-plaintext'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used.Valid {
		t.Fatal("rejected plaintext token was consumed")
	}
}

func TestRotateRefreshToken_ConcurrentReuseSingleWinner(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	rt, err := s.CreateRefreshToken(ctx, "L1", "client-A", testOAuthResource)
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
			next, err := s.RotateRefreshToken(ctx, rt.Token, "client-A", testOAuthResource)
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
		if !errors.Is(result.err, storeport.ErrRefreshTokenReuse) {
			t.Fatalf("loser error = %v, want ErrRefreshTokenReuse", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent rotations = %d, want exactly 1", successes)
	}
	if _, err := s.GetRefreshToken(ctx, rt.Token); err == nil {
		t.Fatal("consumed refresh token remained usable")
	}
	if _, err := s.GetRefreshToken(ctx, successor); err == nil {
		t.Fatal("family replay must revoke the winning successor")
	}
	var rows, revoked int
	if err := s.root.QueryRow(`SELECT COUNT(*), COUNT(revoked_at) FROM refresh_tokens`).Scan(&rows, &revoked); err != nil {
		t.Fatalf("count refresh token family: %v", err)
	}
	if rows != 2 || revoked != 2 {
		t.Fatalf("family rows=%d revoked=%d, want 2/2", rows, revoked)
	}
}

func TestRotateRefreshToken_ReplayRevokesFamily(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	root, err := s.CreateRefreshToken(ctx, "L1", "client-A", testOAuthResource)
	if err != nil {
		t.Fatalf("create refresh token: %v", err)
	}
	successor, err := s.RotateRefreshToken(ctx, root.Token, "client-A", testOAuthResource)
	if err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	if _, err := s.GetRefreshToken(ctx, successor.Token); err != nil {
		t.Fatalf("successor should be active before replay: %v", err)
	}

	if _, err := s.RotateRefreshToken(ctx, root.Token, "client-A", testOAuthResource); !errors.Is(err, storeport.ErrRefreshTokenReuse) {
		t.Fatalf("replay error = %v, want ErrRefreshTokenReuse", err)
	}
	if _, err := s.GetRefreshToken(ctx, successor.Token); err == nil {
		t.Fatal("successor remained usable after ancestor replay")
	}

	var families, revoked int
	if err := s.root.QueryRow(`SELECT COUNT(DISTINCT family_id), COUNT(revoked_at) FROM refresh_tokens`).Scan(&families, &revoked); err != nil {
		t.Fatalf("inspect family: %v", err)
	}
	if families != 1 || revoked != 2 {
		t.Fatalf("families=%d revoked=%d, want 1/2", families, revoked)
	}
}

func TestCleanupExpiredRefreshTokens_RetainsAncestorsOfLiveFamily(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	root, err := s.CreateRefreshToken(ctx, "L1", "client-A", testOAuthResource)
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	successor, err := s.RotateRefreshToken(ctx, root.Token, "client-A", testOAuthResource)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.root.Exec(
		rb(s, `UPDATE refresh_tokens SET expires_at = ? WHERE token = ?`),
		time.Now().UTC().Add(-time.Hour), refreshTokenHash(root.Token),
	); err != nil {
		t.Fatalf("expire ancestor: %v", err)
	}

	deleted, err := s.CleanupExpiredRefreshTokens(ctx)
	if err != nil {
		t.Fatalf("cleanup with live descendant: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted=%d, want 0 while descendant is live", deleted)
	}
	if _, err := s.GetRefreshToken(ctx, successor.Token); err != nil {
		t.Fatalf("live descendant unavailable: %v", err)
	}
	var rows int
	if err := s.root.QueryRow(`SELECT COUNT(*) FROM refresh_tokens`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("family rows=%d err=%v, want 2", rows, err)
	}
}

func TestRotateRefreshToken_SuccessorFailureRollsBackConsumption(t *testing.T) {
	s := setupTestDB(t)
	if s.dialect != DialectSQLite {
		t.Skip("SQLite trigger fault injection; transaction semantics are exercised on PostgreSQL by the concurrency test")
	}
	ctx := context.Background()
	rt, err := s.CreateRefreshToken(ctx, "L1", "client-A", testOAuthResource)
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

	if _, err := s.RotateRefreshToken(ctx, rt.Token, "client-A", testOAuthResource); err == nil {
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

func TestCreateOAuthClientWithSecretCappedConcurrent(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	const contenders = 12
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results <- store.CreateOAuthClientWithSecretCapped(
				ctx, fmt.Sprintf("concurrent-%d", i), "client", `["https://x.example/cb"]`, "", 1,
			)
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, storeport.ErrOAuthClientLimitReached):
		default:
			t.Fatalf("unexpected capped registration error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("registration winners = %d, want 1", winners)
	}
	count, err := store.CountOAuthClients(ctx)
	if err != nil || count != 1 {
		t.Fatalf("registered clients = %d, err=%v", count, err)
	}
}
