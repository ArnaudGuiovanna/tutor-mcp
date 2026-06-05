package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"tutor-mcp/db"

	_ "modernc.org/sqlite"
)

var testDBCounter int

func newTestServer(t *testing.T) (*OAuthServer, *db.Store) {
	t.Helper()
	testDBCounter++
	dsn := fmt.Sprintf("file:oauth_memdb_%s_%d?mode=memory&cache=shared", t.Name(), testDBCounter)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { sqldb.Close() })

	store := db.NewStore(sqldb)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewOAuthServer(store, "https://test.example", logger), store
}

func seedClient(t *testing.T, store *db.Store, clientID, redirectURI string) {
	t.Helper()
	if err := store.CreateOAuthClient(context.Background(), clientID, "Test Client", fmt.Sprintf(`[%q]`, redirectURI)); err != nil {
		t.Fatalf("seed client: %v", err)
	}
}

func seedLearner(t *testing.T, store *db.Store, email, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	l, err := store.CreateLearner(context.Background(), email, string(hash), "", "")
	if err != nil {
		t.Fatalf("create learner: %v", err)
	}
	return l.ID
}

// ─── validateRegistrationRedirectURIs ────────────────────────────────────────

func TestValidateRegistrationRedirectURIs(t *testing.T) {
	cases := []struct {
		name    string
		uris    []string
		wantErr bool
	}{
		{"https accepted", []string{"https://claude.ai/cb"}, false},
		{"http public rejected", []string{"http://example.com/cb"}, true},
		{"localhost http accepted", []string{"http://localhost:8080/cb"}, false},
		{"127.0.0.1 http accepted", []string{"http://127.0.0.1:8080/cb"}, false},
		{"private 10/8 rejected", []string{"https://10.0.0.1/cb"}, true},
		{"private 192.168 rejected", []string{"https://192.168.1.1/cb"}, true},
		{"private 172.16 rejected", []string{"https://172.16.5.5/cb"}, true},
		{"link-local 169.254 rejected", []string{"https://169.254.169.254/cb"}, true},
		{"public IP https accepted", []string{"https://8.8.8.8/cb"}, false},
		{"too many uris", []string{"https://a/1", "https://a/2", "https://a/3", "https://a/4", "https://a/5", "https://a/6"}, true},
		{"too long uri", []string{"https://a.example/" + strings.Repeat("x", 513)}, true},
		{"malformed uri", []string{"://bad"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRegistrationRedirectURIs(tc.uris)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.1.1", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"fc00::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP: %s", tc.ip)
			}
			if got := isPrivateIP(ip); got != tc.private {
				t.Fatalf("isPrivateIP(%s) = %v, want %v", tc.ip, got, tc.private)
			}
		})
	}
}

// ─── redirect_uri enforcement on GET + POST ─────────────────────────────────

func TestAuthorizeGet_UnregisteredRedirectURI(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	req := httptest.NewRequest("GET", "/authorize?client_id=cid&redirect_uri=https://attacker.example/evil&response_type=code", nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorizeGet(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("unexpected redirect: %s", loc)
	}
}

func TestAuthorizeGet_ValidRedirectURI_SetsCSRFCookie(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	req := httptest.NewRequest("GET", "/authorize?client_id=cid&redirect_uri=https://good.example/cb&response_type=code&state=xyz&code_challenge=abc&code_challenge_method=S256", nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorizeGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "csrf_token" {
			csrfCookie = c
		}
	}
	if csrfCookie == nil {
		t.Fatal("csrf_token cookie not set")
	}
	if !csrfCookie.HttpOnly || !csrfCookie.Secure || csrfCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags wrong: %+v", csrfCookie)
	}
	if csrfCookie.Path != "/authorize" {
		t.Fatalf("cookie path = %s", csrfCookie.Path)
	}
	if !strings.Contains(rec.Body.String(), csrfCookie.Value) {
		t.Fatal("csrf token not rendered in page body")
	}
}

func TestAuthorizePost_UnregisteredRedirectURI(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://attacker.example/evil")
	form.Set("email", "u@e.com")
	form.Set("password", "password123")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("unexpected redirect: %s", loc)
	}
}

// ─── CSRF on POST /authorize ───────────────────────────────────────────────

func TestAuthorizePost_NoCSRFCookie(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "field-only")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAuthorizePost_CSRFMismatch(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "field-value")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "cookie-value-different"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAuthorizeGet_RendersStrictCSPWithoutInlineHandlers(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	req := httptest.NewRequest("GET", "/authorize?client_id=cid&redirect_uri=https://good.example/cb&response_type=code&code_challenge=abc&code_challenge_method=S256", nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorizeGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("missing Content-Security-Policy header")
	}
	for _, must := range []string{"default-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'", "'nonce-"} {
		if !strings.Contains(csp, must) {
			t.Fatalf("CSP missing %q: %s", must, csp)
		}
	}
	body := rec.Body.String()
	if strings.Contains(body, "onclick=") {
		t.Fatal("inline onclick handler still present in rendered page")
	}
}

func TestAuthorizeGet_PublicClientWithoutPKCERejected(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	req := httptest.NewRequest("GET", "/authorize?client_id=cid&redirect_uri=https://good.example/cb&response_type=code&state=xyz", nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorizeGet(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("public client without code_challenge should be rejected, got %d", rec.Code)
	}
}

func TestAuthorizeGet_PublicClientWithPlainPKCERejected(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	req := httptest.NewRequest("GET", "/authorize?client_id=cid&redirect_uri=https://good.example/cb&response_type=code&code_challenge=abc&code_challenge_method=plain", nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorizeGet(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("plain PKCE method should be rejected, got %d", rec.Code)
	}
}

func TestAuthorizePost_CSRFMatch_InvalidCreds(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	seedLearner(t, store, "u@example.com", "correct-password")

	form := url.Values{}
	form.Set("csrf_token", "matching-token")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "abc")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "u@example.com")
	form.Set("password", "wrong-password")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "matching-token"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("should not be 403 (csrf passed), got 403")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for invalid creds", rec.Code)
	}
}

// ─── refresh_token grant: client authentication required (issue #30) ────────

// TestRefreshTokenGrant_RejectsMissingClientID locks down the RFC 6749 §6
// requirement: a refresh_token grant must authenticate the client. A POST to
// /token with a valid refresh_token but no client_id (and no Basic auth) must
// be rejected with 401 invalid_client. Previously the handler bypassed
// verifyClientAuth when client_id was empty, allowing any holder of a stolen
// refresh token to mint new access/refresh tokens with no client identity.
func TestRefreshTokenGrant_RejectsMissingClientID(t *testing.T) {
	s, store := newTestServer(t)
	learnerID := seedLearner(t, store, "rt-noclient@example.com", "pw")
	rt, err := store.CreateRefreshToken(context.Background(), learnerID, "")
	if err != nil {
		t.Fatalf("seed refresh token: %v", err)
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt.Token},
	}
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 invalid_client when client_id is omitted, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("body missing invalid_client: %q", rec.Body.String())
	}
	// And the refresh token must NOT have been rotated/consumed.
	if _, err := store.GetRefreshToken(context.Background(), rt.Token); err != nil {
		t.Fatalf("refresh token must remain valid after rejected request: %v", err)
	}
}

// TestRefreshTokenGrant_RejectsCrossClientRedemption locks down issue #30
// part 2: a refresh token issued to client A cannot be redeemed by client B,
// even when B authenticates with valid credentials. Without binding, an
// attacker who steals A's refresh token can self-register as confidential
// client B and redeem the token transparently.
func TestRefreshTokenGrant_RejectsCrossClientRedemption(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	// Two valid clients in the store.
	seedClient(t, store, "client-A", "https://a.example/cb")
	seedClient(t, store, "client-B", "https://b.example/cb")
	learnerID := seedLearner(t, store, "rt-bound@example.com", "pw")
	// Bind the refresh token to client-A.
	rt, err := store.CreateRefreshToken(context.Background(), learnerID, "client-A")
	if err != nil {
		t.Fatalf("seed refresh token: %v", err)
	}

	// Client B presents A's refresh token with valid B credentials.
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt.Token},
		"client_id":     {"client-B"},
	}
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 invalid_grant on cross-client redemption, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("body missing invalid_grant: %q", rec.Body.String())
	}
	// Token must NOT have been rotated.
	if _, err := store.GetRefreshToken(context.Background(), rt.Token); err != nil {
		t.Fatalf("refresh token must remain valid after rejected cross-client redemption: %v", err)
	}
}

// ─── PKCE conditional enforcement at /token (issue #114) ────────────────────

// driveAuthorizePost issues GET /authorize to obtain a CSRF cookie, then POSTs
// the form back so we exercise the real /authorize handler end-to-end. It
// returns the authorization code extracted from the redirect URL.
func driveAuthorizePost(t *testing.T, s *OAuthServer, clientID, redirectURI, codeChallenge, codeChallengeMethod, email, password string) string {
	t.Helper()
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("state", "s-114")
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", codeChallengeMethod)
	}

	getReq := httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil)
	getRec := httptest.NewRecorder()
	s.HandleAuthorizeGet(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("authorize GET status = %d, want 200; body=%q", getRec.Code, getRec.Body.String())
	}
	var csrf string
	for _, c := range getRec.Result().Cookies() {
		if c.Name == "csrf_token" {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("authorize GET did not set csrf_token cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", csrf)
	form.Set("mode", "login")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirectURI)
	form.Set("response_type", "code")
	form.Set("state", "s-114")
	form.Set("scope", "learner")
	form.Set("email", email)
	form.Set("password", password)
	form.Set("approve_client", "yes")
	if codeChallenge != "" {
		form.Set("code_challenge", codeChallenge)
		form.Set("code_challenge_method", codeChallengeMethod)
	}

	postReq := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(&http.Cookie{Name: "csrf_token", Value: csrf})
	postRec := httptest.NewRecorder()
	s.HandleAuthorizePost(postRec, postReq)

	if postRec.Code != http.StatusFound {
		t.Fatalf("authorize POST status = %d, want 302; body=%q", postRec.Code, postRec.Body.String())
	}
	loc := postRec.Header().Get("Location")
	if loc == "" {
		t.Fatalf("authorize POST: missing Location header")
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("authorize POST: bad Location %q: %v", loc, err)
	}
	code := parsed.Query().Get("code")
	if code == "" {
		t.Fatalf("authorize POST: no code in redirect %q", loc)
	}
	return code
}

// seedConfidentialClient registers a confidential client with a known secret
// and returns the cleartext secret.
func seedConfidentialClient(t *testing.T, store *db.Store, clientID, redirectURI, secret string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if err := store.CreateOAuthClientWithSecret(context.Background(), clientID, "Conf 114", fmt.Sprintf(`[%q]`, redirectURI), string(hash)); err != nil {
		t.Fatalf("create confidential client: %v", err)
	}
}

// TestTokenEndpoint_ConfidentialClientWithoutPKCE_Issue114 reproduces issue
// #114: a confidential client legitimately skipped PKCE at /authorize (the
// requirePKCEForPublicClient gate accepts that), so it has no code_verifier
// to present at /token. The pre-fix handler unconditionally required a
// non-empty verifier and compared SHA256(verifier) against the stored empty
// challenge, blocking the redemption. After the fix, a confidential client
// authenticated with client_secret_basic and no verifier must get a token.
func TestTokenEndpoint_ConfidentialClientWithoutPKCE_Issue114(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	const (
		clientID    = "cid-conf-114"
		clientSec   = "shh-114"
		redirectURI = "https://c.example/cb"
		email       = "u-114@example.com"
		password    = "password-correct"
	)
	seedConfidentialClient(t, store, clientID, redirectURI, clientSec)
	seedLearner(t, store, email, password)

	// /authorize without PKCE — confidential clients are allowed to skip it.
	code := driveAuthorizePost(t, s, clientID, redirectURI, "", "", email, password)

	// /token with client_secret_basic and NO code_verifier.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	tokReq := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.SetBasicAuth(clientID, clientSec)
	tokRec := httptest.NewRecorder()
	s.HandleToken(tokRec, tokReq)

	if tokRec.Code != http.StatusOK {
		t.Fatalf("token status = %d, want 200; body=%q", tokRec.Code, tokRec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(tokRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("token body not JSON: %v body=%q", err, tokRec.Body.String())
	}
	if at, _ := resp["access_token"].(string); at == "" {
		t.Fatalf("token response missing access_token: %+v", resp)
	}
	if rt, _ := resp["refresh_token"].(string); rt == "" {
		t.Fatalf("token response missing refresh_token: %+v", resp)
	}
	if tt, _ := resp["token_type"].(string); !strings.EqualFold(tt, "bearer") {
		t.Fatalf("token_type = %q, want bearer", tt)
	}
	// expires_in must be present and positive (per the user-confirmed spec).
	exp, ok := resp["expires_in"].(float64)
	if !ok || exp <= 0 {
		t.Fatalf("token response missing or invalid expires_in: %+v", resp)
	}

	// The auth code must now be consumed: a second redemption with the same
	// code must fail with invalid_grant. This guards against a regression
	// where the conditional PKCE branch lets a code be redeemed twice.
	form2 := url.Values{}
	form2.Set("grant_type", "authorization_code")
	form2.Set("code", code)
	tokReq2 := httptest.NewRequest("POST", "/token", strings.NewReader(form2.Encode()))
	tokReq2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq2.SetBasicAuth(clientID, clientSec)
	tokRec2 := httptest.NewRecorder()
	s.HandleToken(tokRec2, tokReq2)
	if tokRec2.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400 invalid_grant; body=%q", tokRec2.Code, tokRec2.Body.String())
	}
	if !strings.Contains(tokRec2.Body.String(), "invalid_grant") {
		t.Fatalf("replay body missing invalid_grant: %q", tokRec2.Body.String())
	}
}

// TestTokenEndpoint_ConfidentialClientWithoutPKCE_Issue114_ClientSecretPost
// is the same as the basic-auth case but uses client_secret_post.
func TestTokenEndpoint_ConfidentialClientWithoutPKCE_Issue114_ClientSecretPost(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	const (
		clientID    = "cid-conf-114-post"
		clientSec   = "shh-114-post"
		redirectURI = "https://c.example/cb"
		email       = "u-114-post@example.com"
		password    = "password-correct"
	)
	seedConfidentialClient(t, store, clientID, redirectURI, clientSec)
	seedLearner(t, store, email, password)

	code := driveAuthorizePost(t, s, clientID, redirectURI, "", "", email, password)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSec)
	tokReq := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRec := httptest.NewRecorder()
	s.HandleToken(tokRec, tokReq)

	if tokRec.Code != http.StatusOK {
		t.Fatalf("token status = %d, want 200; body=%q", tokRec.Code, tokRec.Body.String())
	}
}

// TestTokenEndpoint_ConfidentialClientWithPKCE_StillEnforced verifies that
// when a confidential client *does* opt in to PKCE at /authorize, the /token
// handler still validates the verifier. A matching verifier must succeed; a
// mismatched one must fail with invalid_grant.
func TestTokenEndpoint_ConfidentialClientWithPKCE_StillEnforced(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	const (
		clientID    = "cid-conf-114-pkce"
		clientSec   = "shh-114-pkce"
		redirectURI = "https://c.example/cb"
		email       = "u-114-pkce@example.com"
		password    = "password-correct"
	)
	seedConfidentialClient(t, store, clientID, redirectURI, clientSec)
	seedLearner(t, store, email, password)

	verifier := "verifier-114-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	t.Run("matching verifier succeeds", func(t *testing.T) {
		code := driveAuthorizePost(t, s, clientID, redirectURI, challenge, "S256", email, password)

		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("code", code)
		form.Set("code_verifier", verifier)
		tokReq := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
		tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tokReq.SetBasicAuth(clientID, clientSec)
		tokRec := httptest.NewRecorder()
		s.HandleToken(tokRec, tokReq)

		if tokRec.Code != http.StatusOK {
			t.Fatalf("token status = %d, want 200; body=%q", tokRec.Code, tokRec.Body.String())
		}
	})

	t.Run("mismatched verifier fails", func(t *testing.T) {
		code := driveAuthorizePost(t, s, clientID, redirectURI, challenge, "S256", email, password)

		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("code", code)
		form.Set("code_verifier", "wrong-verifier-aaaaaaaaaaaaaaaaaaaaaaaaa")
		tokReq := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
		tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tokReq.SetBasicAuth(clientID, clientSec)
		tokRec := httptest.NewRecorder()
		s.HandleToken(tokRec, tokReq)

		if tokRec.Code != http.StatusBadRequest {
			t.Fatalf("token status = %d, want 400 invalid_grant; body=%q", tokRec.Code, tokRec.Body.String())
		}
		if !strings.Contains(tokRec.Body.String(), "invalid_grant") {
			t.Fatalf("body missing invalid_grant: %q", tokRec.Body.String())
		}
	})

	t.Run("missing verifier fails when challenge was set", func(t *testing.T) {
		code := driveAuthorizePost(t, s, clientID, redirectURI, challenge, "S256", email, password)

		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("code", code)
		// No code_verifier.
		tokReq := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
		tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tokReq.SetBasicAuth(clientID, clientSec)
		tokRec := httptest.NewRecorder()
		s.HandleToken(tokRec, tokReq)

		if tokRec.Code != http.StatusBadRequest {
			t.Fatalf("token status = %d, want 400; body=%q", tokRec.Code, tokRec.Body.String())
		}
		// Either invalid_request (missing verifier per the suggested fix) or
		// invalid_grant (PKCE failure per RFC 7636) is acceptable; what
		// matters is that no token is issued.
		body := tokRec.Body.String()
		if !strings.Contains(body, "invalid_request") && !strings.Contains(body, "invalid_grant") {
			t.Fatalf("body must report invalid_request or invalid_grant: %q", body)
		}
	})
}

// TestTokenEndpoint_PublicClientStillRequiresPKCE is a regression guard: a
// public client (no client_secret) cannot reach /token without a code_verifier.
// The /authorize gate already rejects missing PKCE for public clients, but if
// somehow an empty-challenge code was minted for a public client, /token must
// still refuse to issue a token without a verifier. We seed an auth code with
// an empty challenge directly so this guard doesn't depend on /authorize.
func TestTokenEndpoint_PublicClientStillRequiresPKCE(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid-pub", "https://p.example/cb")
	learner := seedLearner(t, store, "u-pub-114@example.com", "pw")

	// Direct seed: empty code_challenge for a public client. /token must
	// still require a verifier (or otherwise refuse) because public clients
	// have no secret to authenticate with.
	if err := store.CreateAuthCode(context.Background(), "pub-code-114", learner, "", "cid-pub", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("seed auth code: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "pub-code-114")
	form.Set("client_id", "cid-pub")
	// No code_verifier, no client_secret — pure public-client misuse.
	tokReq := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRec := httptest.NewRecorder()
	s.HandleToken(tokRec, tokReq)

	if tokRec.Code == http.StatusOK {
		t.Fatalf("public client without verifier must NOT receive a token; got 200 body=%q", tokRec.Body.String())
	}
	// Tighten the regression guard: must be a 400 with invalid_request or
	// invalid_grant. A 5xx would mask a logic bug; a 401 would imply we
	// somehow reached client-auth (public clients have no secret).
	if tokRec.Code != http.StatusBadRequest {
		t.Fatalf("public-client no-PKCE status = %d, want 400; body=%q", tokRec.Code, tokRec.Body.String())
	}
	body := tokRec.Body.String()
	if !strings.Contains(body, "invalid_request") && !strings.Contains(body, "invalid_grant") {
		t.Fatalf("public-client no-PKCE body must report invalid_request or invalid_grant: %q", body)
	}
	// The response must NOT contain anything that looks like a JWT or refresh
	// token field — a sanity check that no token leaked on the rejection path.
	if strings.Contains(body, "access_token") || strings.Contains(body, "refresh_token") {
		t.Fatalf("rejection body unexpectedly contains token fields: %q", body)
	}
}

// TestTokenEndpoint_ConfidentialClientWithBadSecret_NoPKCE_Rejected pairs
// with TestHandleToken_AuthorizationCode_ConfidentialClient_BadSecret but
// drops the (now-optional) code_verifier so the test exercises the exact
// matrix the #114 fix changed: confidential client + WRONG secret + no
// verifier. The bcrypt comparison in verifyClientAuth must fire BEFORE the
// conditional PKCE branch is even reached, so the request must still be
// rejected as invalid_client. If this test ever flips to 200, an attacker
// who knows a confidential client_id could redeem auth codes without a
// secret — a complete bypass.
func TestTokenEndpoint_ConfidentialClientWithBadSecret_NoPKCE_Rejected(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	const (
		clientID    = "cid-conf-114-badsec"
		realSecret  = "real-secret-114"
		wrongSecret = "wrong-secret-114"
		redirectURI = "https://c.example/cb"
		email       = "u-114-badsec@example.com"
		password    = "password-correct"
	)
	seedConfidentialClient(t, store, clientID, redirectURI, realSecret)
	seedLearner(t, store, email, password)

	// Mint an auth code via /authorize WITHOUT PKCE (allowed for confidential).
	code := driveAuthorizePost(t, s, clientID, redirectURI, "", "", email, password)

	// /token with the WRONG client_secret and NO code_verifier.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	tokReq := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.SetBasicAuth(clientID, wrongSecret)
	tokRec := httptest.NewRecorder()
	s.HandleToken(tokRec, tokReq)

	if tokRec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-secret no-PKCE status = %d, want 401 invalid_client; body=%q", tokRec.Code, tokRec.Body.String())
	}
	body := tokRec.Body.String()
	if !strings.Contains(body, "invalid_client") {
		t.Fatalf("body missing invalid_client: %q", body)
	}
	// Defense-in-depth: the rejection body must not echo a JWT/refresh token
	// or anything that resembles the supplied secret.
	if strings.Contains(body, "access_token") || strings.Contains(body, "refresh_token") {
		t.Fatalf("rejection body unexpectedly contains token fields: %q", body)
	}
	if strings.Contains(body, wrongSecret) || strings.Contains(body, realSecret) {
		t.Fatalf("rejection body must not echo client_secret material: %q", body)
	}
}
