// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"tutor-mcp/db"
)

func futureTime() time.Time { return time.Now().Add(time.Hour) }
func pastTime() time.Time   { return time.Now().Add(-time.Hour) }

// ─── RegisterRoutes ─────────────────────────────────────────────────────────

func TestRegisterRoutes_AllEndpointsWired(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	cases := []struct {
		method string
		path   string
	}{
		{"GET", "/.well-known/oauth-authorization-server"},
		{"GET", "/.well-known/oauth-protected-resource"},
		{"GET", "/authorize"},
		{"POST", "/authorize"},
		{"POST", "/token"},
		{"POST", "/register"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s returned 404 — route not wired", tc.method, tc.path)
			}
		})
	}
}

// ─── Metadata endpoints ─────────────────────────────────────────────────────

func TestHandleAuthServerMetadata(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
	rec := httptest.NewRecorder()
	s.HandleAuthServerMetadata(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if meta["issuer"] != "https://test.example" {
		t.Fatalf("issuer = %v", meta["issuer"])
	}
	if meta["authorization_endpoint"] != "https://test.example/authorize" {
		t.Fatalf("authorize endpoint mismatch: %v", meta["authorization_endpoint"])
	}
	if meta["token_endpoint"] != "https://test.example/token" {
		t.Fatalf("token endpoint mismatch: %v", meta["token_endpoint"])
	}
	if meta["registration_endpoint"] != "https://test.example/register" {
		t.Fatalf("registration endpoint mismatch: %v", meta["registration_endpoint"])
	}
	if meta["authorization_response_iss_parameter_supported"] != true {
		t.Fatalf("iss parameter support flag missing or false: %v", meta["authorization_response_iss_parameter_supported"])
	}
	methods, _ := meta["code_challenge_methods_supported"].([]interface{})
	found := false
	for _, m := range methods {
		if m == "S256" {
			found = true
		}
	}
	if !found {
		t.Fatalf("S256 not in code_challenge_methods_supported: %v", methods)
	}
}

func TestHandleProtectedResourceMetadata(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil)
	rec := httptest.NewRecorder()
	s.HandleProtectedResourceMetadata(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if meta["resource"] != "https://test.example/mcp" {
		t.Fatalf("resource = %v", meta["resource"])
	}
	servers, _ := meta["authorization_servers"].([]interface{})
	if len(servers) != 1 || servers[0] != "https://test.example" {
		t.Fatalf("authorization_servers = %v", servers)
	}
}

// ─── HandleToken: dispatcher ─────────────────────────────────────────────────

func TestHandleToken_UnsupportedGrantType(t *testing.T) {
	s, _ := newTestServer(t)
	form := url.Values{}
	form.Set("grant_type", "password")

	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unsupported_grant_type") {
		t.Fatalf("body missing unsupported_grant_type: %q", rec.Body.String())
	}
}

func TestHandleToken_BadFormBody(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/token", strings.NewReader("a=%ZZ"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ─── handleAuthorizationCodeGrant ─────────────────────────────────────────────

func TestHandleToken_AuthorizationCode_Success(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	learnerID := seedLearner(t, store, "u@e.com", "pw123")

	verifier := "abc-verifier-not-empty-string"
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	if err := store.CreateAuthCode(context.Background(), "the-code", learnerID, challenge, "cid", futureTime()); err != nil {
		t.Fatalf("seed code: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "the-code")
	form.Set("code_verifier", verifier)
	form.Set("client_id", "cid")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control header missing")
	}
	if rec.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("Pragma header missing")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["token_type"] != "bearer" {
		t.Fatalf("token_type = %v", resp["token_type"])
	}
	if resp["scope"] != "learner" {
		t.Fatalf("scope = %v", resp["scope"])
	}
	if _, ok := resp["access_token"].(string); !ok {
		t.Fatal("access_token missing or not string")
	}
	if _, ok := resp["refresh_token"].(string); !ok {
		t.Fatal("refresh_token missing or not string")
	}
	if resp["expires_in"].(float64) != 86400 {
		t.Fatalf("expires_in = %v", resp["expires_in"])
	}
}

func TestHandleToken_AuthorizationCode_MissingCode(t *testing.T) {
	setTestSecret(t)
	s, _ := newTestServer(t)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("body missing invalid_request: %q", rec.Body.String())
	}
}

func TestHandleToken_AuthorizationCode_UnknownClient(t *testing.T) {
	setTestSecret(t)
	s, _ := newTestServer(t)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "xyz")
	form.Set("code_verifier", "v")
	form.Set("client_id", "no-such-client")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("body missing invalid_client: %q", rec.Body.String())
	}
}

func TestHandleToken_AuthorizationCode_ConfidentialClient_BadSecret(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("real-secret"), bcrypt.MinCost)
	if err := store.CreateOAuthClientWithSecret(context.Background(), "cid-conf", "Confidential", `["https://c.example/cb"]`, string(hash)); err != nil {
		t.Fatalf("create confidential client: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "x")
	form.Set("code_verifier", "v")
	form.Set("client_id", "cid-conf")
	form.Set("client_secret", "wrong-secret")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("body missing invalid_client: %q", rec.Body.String())
	}
}

func TestHandleToken_AuthorizationCode_ConfidentialClient_BasicAuthOK(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("real-secret"), bcrypt.MinCost)
	if err := store.CreateOAuthClientWithSecret(context.Background(), "cid-conf", "Confidential", `["https://c.example/cb"]`, string(hash)); err != nil {
		t.Fatalf("create confidential client: %v", err)
	}
	learner := seedLearner(t, store, "u-conf@e.com", "pw")

	verifier := "verifier-string"
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	if err := store.CreateAuthCode(context.Background(), "conf-code", learner, challenge, "cid-conf", futureTime()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "conf-code")
	form.Set("code_verifier", verifier)
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cid-conf", "real-secret")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleToken_AuthorizationCode_InvalidGrantUnknownCode(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "nope")
	form.Set("code_verifier", "v")
	form.Set("client_id", "cid")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("body missing invalid_grant: %q", rec.Body.String())
	}
}

func TestHandleToken_AuthorizationCode_ExpiredCode(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	learner := seedLearner(t, store, "u-exp@e.com", "pw")

	verifier := "v"
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	if err := store.CreateAuthCode(context.Background(), "exp-code", learner, challenge, "cid", pastTime()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "exp-code")
	form.Set("code_verifier", verifier)
	form.Set("client_id", "cid")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("body missing invalid_grant: %q", rec.Body.String())
	}
}

func TestHandleToken_AuthorizationCode_PKCEMismatch(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	learner := seedLearner(t, store, "u-pkce@e.com", "pw")

	if err := store.CreateAuthCode(context.Background(), "pkce-code", learner, "wrong-challenge", "cid", futureTime()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "pkce-code")
	form.Set("code_verifier", "real-verifier")
	form.Set("client_id", "cid")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("body missing invalid_grant: %q", rec.Body.String())
	}
}

// ─── handleRefreshTokenGrant ─────────────────────────────────────────────────

func TestHandleToken_RefreshToken_MissingToken(t *testing.T) {
	setTestSecret(t)
	s, _ := newTestServer(t)

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("body missing invalid_request: %q", rec.Body.String())
	}
}

func TestHandleToken_RefreshToken_UnknownToken(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	// A registered public client is required: refresh_token grant now
	// always authenticates the client (issue #30).
	seedClient(t, store, "cid-pub", "https://app.example/cb")

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", "no-such-token")
	form.Set("client_id", "cid-pub")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("body missing invalid_grant: %q", rec.Body.String())
	}
}

func TestHandleToken_RefreshToken_Success(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	// Public client (no secret) — required since refresh_token grant now
	// always authenticates the client (issue #30).
	seedClient(t, store, "cid-pub", "https://app.example/cb")
	learner := seedLearner(t, store, "u-rt@e.com", "pw")
	rt, err := store.CreateRefreshToken(context.Background(), learner, "")
	if err != nil {
		t.Fatalf("seed rt: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt.Token)
	form.Set("client_id", "cid-pub")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	newRT, _ := resp["refresh_token"].(string)
	if newRT == "" || newRT == rt.Token {
		t.Fatalf("refresh token must rotate; old=%q new=%q", rt.Token, newRT)
	}
	if _, err := store.GetRefreshToken(context.Background(), rt.Token); err == nil {
		t.Fatal("old refresh token must be deleted after rotation")
	}
}

func TestHandleToken_RefreshToken_ConfidentialClientUnknown(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	learner := seedLearner(t, store, "u-rt2@e.com", "pw")
	rt, err := store.CreateRefreshToken(context.Background(), learner, "")
	if err != nil {
		t.Fatalf("seed rt: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt.Token)
	form.Set("client_id", "ghost-client")
	form.Set("client_secret", "x")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("body missing invalid_client: %q", rec.Body.String())
	}
}

func TestHandleToken_RefreshToken_ConfidentialClientBadSecret(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("real-secret"), bcrypt.MinCost)
	if err := store.CreateOAuthClientWithSecret(context.Background(), "cid-c2", "Confidential2", `["https://c.example/cb"]`, string(hash)); err != nil {
		t.Fatalf("create confidential client: %v", err)
	}
	learner := seedLearner(t, store, "u-rt3@e.com", "pw")
	rt, err := store.CreateRefreshToken(context.Background(), learner, "")
	if err != nil {
		t.Fatalf("seed rt: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt.Token)
	form.Set("client_id", "cid-c2")
	form.Set("client_secret", "wrong-secret")
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// ─── HandleRegister ─────────────────────────────────────────────────────────

func TestHandleRegister_PublicClient(t *testing.T) {
	s, _ := newTestServer(t)

	body := `{"client_name":"My Public Client","redirect_uris":["https://app.example/cb"]}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("Cache-Control: no-store missing")
	}
	if rec.Header().Get("Pragma") != "no-cache" {
		t.Fatal("Pragma: no-cache missing")
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cid, _ := resp["client_id"].(string); cid == "" {
		t.Fatal("client_id missing")
	}
	if resp["client_name"] != "My Public Client" {
		t.Fatalf("client_name = %v", resp["client_name"])
	}
	if resp["token_endpoint_auth_method"] != "none" {
		t.Fatalf("auth_method = %v, want 'none' for public client", resp["token_endpoint_auth_method"])
	}
	if _, ok := resp["client_secret"]; ok {
		t.Fatal("public client must NOT get a client_secret")
	}
	if _, ok := resp["registration_access_token"].(string); !ok {
		t.Fatal("RFC 7592 registration_access_token missing")
	}
}

func TestHandleRegister_ConfidentialClient(t *testing.T) {
	s, _ := newTestServer(t)

	body := `{"client_name":"Confidential","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["token_endpoint_auth_method"] != "client_secret_basic" {
		t.Fatalf("auth_method = %v", resp["token_endpoint_auth_method"])
	}
	secret, _ := resp["client_secret"].(string)
	if secret == "" {
		t.Fatal("confidential client must get a client_secret")
	}
	if v, ok := resp["client_secret_expires_at"]; !ok || v.(float64) != 0 {
		t.Fatalf("client_secret_expires_at = %v, want 0", v)
	}
}

func TestHandleRegister_ConfidentialClientPost(t *testing.T) {
	s, _ := newTestServer(t)

	body := `{"client_name":"C2","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"client_secret_post"}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["client_secret"].(string); !ok {
		t.Fatal("client_secret_post must also produce a client_secret")
	}
}

func TestHandleRegister_BadJSON(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/register", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_client_metadata") {
		t.Fatalf("body missing invalid_client_metadata: %q", rec.Body.String())
	}
}

func TestHandleRegister_RejectsOversizedBody(t *testing.T) {
	s, store := newTestServer(t)
	body := `{"client_name":"` + strings.Repeat("x", int(registerBodyLimitBytes)) + `","redirect_uris":["https://app.example/cb"]}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body too large") {
		t.Fatalf("body missing size error: %q", rec.Body.String())
	}
	count, err := store.CountOAuthClients(context.Background())
	if err != nil {
		t.Fatalf("count clients: %v", err)
	}
	if count != 0 {
		t.Fatalf("registered clients = %d, want 0", count)
	}
}

func TestHandleRegister_ClientCapReached(t *testing.T) {
	s, store := newTestServer(t)
	s.maxRegisteredClients = 1
	if err := store.CreateOAuthClient(context.Background(), "existing", "Existing", `["https://app.example/cb"]`); err != nil {
		t.Fatalf("seed client: %v", err)
	}

	body := `{"client_name":"Blocked","redirect_uris":["https://app.example/cb"]}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "registration_disabled") {
		t.Fatalf("body missing registration_disabled: %q", rec.Body.String())
	}
	count, err := store.CountOAuthClients(context.Background())
	if err != nil {
		t.Fatalf("count clients: %v", err)
	}
	if count != 1 {
		t.Fatalf("registered clients = %d, want 1", count)
	}
}

func TestHandleRegister_NoRedirectURIs(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/register", strings.NewReader(`{"client_name":"X"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_redirect_uri") {
		t.Fatalf("body missing invalid_redirect_uri: %q", rec.Body.String())
	}
}

func TestHandleRegister_PrivateRedirectURIRejected(t *testing.T) {
	s, _ := newTestServer(t)
	body := `{"client_name":"X","redirect_uris":["https://10.0.0.1/cb"]}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_redirect_uri") {
		t.Fatalf("body missing invalid_redirect_uri: %q", rec.Body.String())
	}
}

func TestHandleRegister_RedirectURIsArrayMixedTypes(t *testing.T) {
	s, _ := newTestServer(t)
	body := `{"client_name":"X","redirect_uris":["https://app.example/cb", 42, true]}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}
}

// ─── extractClientCredentials / verifyClientAuth (direct unit tests) ────────

func TestExtractClientCredentials(t *testing.T) {
	t.Run("basic auth wins over form", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/", strings.NewReader("client_id=form-id&client_secret=form-secret"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("basic-id", "basic-secret")
		_ = req.ParseForm()
		id, sec := extractClientCredentials(req)
		if id != "basic-id" || sec != "basic-secret" {
			t.Fatalf("got (%q,%q), want basic creds", id, sec)
		}
	})
	t.Run("form fallback", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/", strings.NewReader("client_id=form-id&client_secret=form-secret"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_ = req.ParseForm()
		id, sec := extractClientCredentials(req)
		if id != "form-id" || sec != "form-secret" {
			t.Fatalf("got (%q,%q), want form creds", id, sec)
		}
	})
	t.Run("none", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_ = req.ParseForm()
		id, sec := extractClientCredentials(req)
		if id != "" || sec != "" {
			t.Fatalf("got (%q,%q), want empty", id, sec)
		}
	})
}

func TestVerifyClientAuth(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	cases := []struct {
		name    string
		client  *db.OAuthClient
		secret  string
		wantErr bool
	}{
		{"public client passes", &db.OAuthClient{ClientSecretHash: ""}, "", false},
		{"public client passes even with secret", &db.OAuthClient{ClientSecretHash: ""}, "anything", false},
		{"confidential missing secret", &db.OAuthClient{ClientSecretHash: string(hash)}, "", true},
		{"confidential wrong secret", &db.OAuthClient{ClientSecretHash: string(hash)}, "wrong", true},
		{"confidential right secret", &db.OAuthClient{ClientSecretHash: string(hash)}, "s3cret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyClientAuth(tc.client, tc.secret)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ─── writeTokenResponse / writeTokenError direct ─────────────────────────────

func TestWriteTokenResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	writeTokenResponse(rec, "AT", "RT")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("Cache-Control missing")
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["access_token"] != "AT" || resp["refresh_token"] != "RT" {
		t.Fatalf("payload mismatch: %v", resp)
	}
}

func TestWriteTokenError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeTokenError(rec, "invalid_grant", http.StatusBadRequest)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatal("Content-Type missing")
	}
	if !strings.Contains(rec.Body.String(), `"error":"invalid_grant"`) {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestWriteRegistrationError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRegistrationError(rec, "invalid_redirect_uri", "broken")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_redirect_uri") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "broken") {
		t.Fatalf("body missing description: %q", rec.Body.String())
	}
}

// ─── mapKeys / generateCode / generateCSRFToken ─────────────────────────────

func TestMapKeys(t *testing.T) {
	got := mapKeys(map[string]interface{}{"a": 1, "b": 2})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, k := range got {
		seen[k] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("got keys %v, want a and b", got)
	}
}

func TestMapKeys_Empty(t *testing.T) {
	got := mapKeys(map[string]interface{}{})
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestGenerateCode_UniqueAndBase64URL(t *testing.T) {
	a, err := generateCode()
	if err != nil {
		t.Fatalf("generateCode: %v", err)
	}
	b, err := generateCode()
	if err != nil {
		t.Fatalf("generateCode: %v", err)
	}
	if a == b {
		t.Fatalf("codes must differ: %q == %q", a, b)
	}
	if a == "" || strings.ContainsAny(a, "+/=") {
		t.Fatalf("code not base64url-no-padding: %q", a)
	}
}

func TestGenerateCSRFToken_UniqueAndBase64URL(t *testing.T) {
	a, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	b, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	if a == b {
		t.Fatalf("CSRF tokens must differ: %q == %q", a, b)
	}
	if a == "" || strings.ContainsAny(a, "+/=") {
		t.Fatalf("token not base64url-no-padding: %q", a)
	}
}

// ─── HandleAuthorizePost: full happy paths + remaining branches ──────────────

func TestAuthorizePost_LoginRequiresClientApproval(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://attacker.example/cb")
	seedLearner(t, store, "victim@example.com", "correct-password")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://attacker.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "victim@example.com")
	form.Set("password", "correct-password")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("unexpected redirect without approval: %s", loc)
	}
	if !strings.Contains(rec.Body.String(), "approve this OAuth client") {
		t.Fatalf("body missing approval message; body=%q", rec.Body.String())
	}
}

func TestAuthorizePost_LoginSuccess_Redirects302WithCodeAndIss(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	seedLearner(t, store, "ok@e.com", "correct-password")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("response_type", "code")
	form.Set("state", "the-state")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "ok@e.com")
	form.Set("password", "correct-password")
	form.Set("approve_client", "yes")
	form.Set("scope", "learner")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%q", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("Location header missing")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if u.Host != "good.example" {
		t.Fatalf("redirect host = %q, want good.example", u.Host)
	}
	q := u.Query()
	if q.Get("code") == "" {
		t.Fatal("redirect missing code param")
	}
	if q.Get("state") != "the-state" {
		t.Fatalf("state = %q, want the-state", q.Get("state"))
	}
	if q.Get("iss") != "https://test.example" {
		t.Fatalf("iss = %q, want https://test.example", q.Get("iss"))
	}
}

func TestAuthorizePost_LoginSuccess_NoState_OmitsStateParam(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	seedLearner(t, store, "okns@e.com", "correct-password")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "okns@e.com")
	form.Set("password", "correct-password")
	form.Set("approve_client", "yes")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	u, _ := url.Parse(rec.Header().Get("Location"))
	if u.Query().Get("state") != "" {
		t.Fatalf("state must be omitted when blank; got %q", u.Query().Get("state"))
	}
	if u.Query().Get("code") == "" {
		t.Fatal("code param missing")
	}
}

func TestAuthorizePost_RegisterSuccess_CreatesAndRedirects(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "register")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("response_type", "code")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "newuser@e.com")
	form.Set("password", "password-1234")
	form.Set("password_confirm", "password-1234")
	form.Set("approve_client", "yes")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%q", rec.Code, rec.Body.String())
	}
	if learner, err := store.GetLearnerByEmail(context.Background(), "newuser@e.com"); err != nil || learner == nil {
		t.Fatalf("learner not created: err=%v learner=%v", err, learner)
	}
}

func TestAuthorizePost_BadForm_400(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/authorize", strings.NewReader("a=%ZZ"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAuthorizePost_MissingEmail_RendersForm(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (renderAuthPage with errMsg)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Email and password are required.") {
		t.Fatalf("expected error message, got %q", rec.Body.String())
	}
}

func TestAuthorizePost_RegisterPasswordMismatch(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "register")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "x@e.com")
	form.Set("password", "passw0rd")
	form.Set("password_confirm", "different")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Passwords do not match.") {
		t.Fatalf("expected mismatch message; got %q", rec.Body.String())
	}
}

func TestAuthorizePost_RegisterPasswordTooShort(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "register")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "x@e.com")
	form.Set("password", "abc")
	form.Set("password_confirm", "abc")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Password must be at least 12 characters.") {
		t.Fatalf("expected too-short message; got %q", rec.Body.String())
	}
}

func TestAuthorizePost_RegisterDuplicateEmail(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	seedLearner(t, store, "dup@e.com", "anything")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "register")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "dup@e.com")
	form.Set("password", "new-password-12")
	form.Set("password_confirm", "new-password-12")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "An account with this email already exists.") {
		t.Fatalf("expected duplicate-email message; got %q", rec.Body.String())
	}
}

func TestAuthorizePost_LoginUnknownEmail(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "ghost@e.com")
	form.Set("password", "doesntmatter")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid email or password.") {
		t.Fatalf("expected invalid-creds message; got %q", rec.Body.String())
	}
}

// ─── validateRedirectURI extra branches ──────────────────────────────────────

func TestValidateRedirectURI_Branches(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	// Seed a client with malformed registered URIs (should hit unmarshal error).
	if err := store.CreateOAuthClient(context.Background(), "cid-bad", "Bad", "not-json-at-all"); err != nil {
		t.Fatalf("seed bad client: %v", err)
	}

	cases := []struct {
		name        string
		clientID    string
		redirectURI string
		wantErr     bool
	}{
		{"empty client", "", "https://good.example/cb", true},
		{"empty redirect", "cid", "", true},
		{"unknown client", "no-client", "https://good.example/cb", true},
		{"malformed registered uris", "cid-bad", "https://good.example/cb", true},
		{"ok exact match", "cid", "https://good.example/cb", false},
		{"mismatch path", "cid", "https://good.example/other", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.validateRedirectURI(context.Background(), tc.clientID, tc.redirectURI)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ─── HandleAuthorizePost: PKCE missing for public client (POST path) ────────

func TestAuthorizePost_PublicClientWithoutPKCERejected(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	// No code_challenge: public client must be rejected on POST too.
	form.Set("email", "u@e.com")
	form.Set("password", "password123")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (PKCE required)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("body missing invalid_request: %q", rec.Body.String())
	}
}

// ─── HandleRegister: too many redirect_uris (hits validate failure branch) ─

func TestHandleRegister_TooManyRedirectURIs(t *testing.T) {
	s, _ := newTestServer(t)
	body := `{"client_name":"X","redirect_uris":["https://a/1","https://a/2","https://a/3","https://a/4","https://a/5","https://a/6"]}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_redirect_uri") {
		t.Fatalf("body missing invalid_redirect_uri: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too many") {
		t.Fatalf("body missing description: %q", rec.Body.String())
	}
}

// ─── requirePKCEForPublicClient extra branches ───────────────────────────────

func TestRequirePKCEForPublicClient_Branches(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "pub", "https://good.example/cb")
	hash, _ := bcrypt.GenerateFromPassword([]byte("s"), bcrypt.MinCost)
	if err := store.CreateOAuthClientWithSecret(context.Background(), "conf", "Conf", `["https://c.example/cb"]`, string(hash)); err != nil {
		t.Fatalf("seed conf: %v", err)
	}

	cases := []struct {
		name      string
		clientID  string
		challenge string
		method    string
		wantErr   bool
	}{
		{"empty client_id", "", "ch", "S256", true},
		{"unknown client", "ghost", "ch", "S256", true},
		{"confidential without pkce ok", "conf", "", "", false},
		{"public with S256 ok", "pub", "ch", "S256", false},
		{"public missing challenge", "pub", "", "S256", true},
		{"public plain method", "pub", "ch", "plain", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.requirePKCEForPublicClient(context.Background(), tc.clientID, tc.challenge, tc.method)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ─── Persisted client approval (R001) ───────────────────────────────────────

// loginRequest builds an /authorize POST form for a learner login. When
// approveClient is false, the approve_client field is omitted entirely (which
// is what a returning client would send if it relied on a remembered consent).
func loginRequest(t *testing.T, clientID, redirectURI, email, password string, approveClient bool) *http.Request {
	t.Helper()
	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", email)
	form.Set("password", password)
	if approveClient {
		form.Set("approve_client", "yes")
	}
	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	return req
}

// TestAuthorizePost_LoginSkipsApprovalAfterFirstConsent verifies R001: once a
// learner has consented to an OAuth client for a specific redirect_uri, the
// next /authorize POST for the same triple must NOT re-prompt for approval.
// Changing the redirect_uri (legitimately, within the client's registered
// list) must re-prompt because the approval is scoped to (learner, client,
// redirect_uri).
func TestAuthorizePost_LoginSkipsApprovalAfterFirstConsent(t *testing.T) {
	s, store := newTestServer(t)
	// Client registered with two redirect_uris so we can verify that the
	// approval row keys on redirect_uri and not on client_id alone.
	if err := store.CreateOAuthClient(context.Background(),
		"cid",
		"Test Client",
		`["https://good.example/cb","https://good.example/cb2"]`,
	); err != nil {
		t.Fatalf("seed client: %v", err)
	}
	seedLearner(t, store, "ok@e.com", "correct-password")

	// 1. First /authorize WITH approve_client=yes — succeeds and persists
	//    the approval row for (learner, cid, https://good.example/cb).
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, loginRequest(t, "cid", "https://good.example/cb", "ok@e.com", "correct-password", true))
	if rec.Code != http.StatusFound {
		t.Fatalf("first login: status = %d, want 302; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://good.example/cb?") {
		t.Fatalf("first login: unexpected redirect %q", loc)
	}

	// 2. Second /authorize WITHOUT approve_client — must succeed (approval
	//    remembered, screen skipped).
	rec = httptest.NewRecorder()
	s.HandleAuthorizePost(rec, loginRequest(t, "cid", "https://good.example/cb", "ok@e.com", "correct-password", false))
	if rec.Code != http.StatusFound {
		t.Fatalf("second login (same redirect_uri, no approve_client): status = %d, want 302; body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "approve this OAuth client") {
		t.Fatalf("second login: approval HTML returned but the client was already approved; body=%q", rec.Body.String())
	}

	// 3. Third /authorize, different (but still registered) redirect_uri,
	//    WITHOUT approve_client — must re-prompt because the approval is
	//    scoped to redirect_uri.
	rec = httptest.NewRecorder()
	s.HandleAuthorizePost(rec, loginRequest(t, "cid", "https://good.example/cb2", "ok@e.com", "correct-password", false))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("third login (different redirect_uri, no approve_client): status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "approve this OAuth client") {
		t.Fatalf("third login: expected approval re-prompt for new redirect_uri; body=%q", rec.Body.String())
	}
}

// ─── Case-insensitive email (R002) ─────────────────────────────────────────

// TestAuthorizePost_LoginEmailIsCaseInsensitive verifies R002: a learner who
// registered as "bob@x.com" can sign back in with "Bob@x.com". Without
// normalisation the lookup runs under SQLite's default BINARY collation and
// silently fails — which both annoys the learner AND means the per-account
// failure tracker keeps separate buckets per case, defeating the lockout.
func TestAuthorizePost_LoginEmailIsCaseInsensitive(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	// Existing row is stored lowercase — the migration normalises rows
	// written before the helper was in place to this shape.
	seedLearner(t, store, "bob@x.com", "correct-password")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "login")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "Bob@x.com") // uppercase first letter
	form.Set("password", "correct-password")
	form.Set("approve_client", "yes")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://good.example/cb?") {
		t.Fatalf("unexpected redirect %q", loc)
	}
}

// TestAuthorizePost_LoginFailureBucketSharedAcrossEmailCases verifies that
// the per-account lockout cannot be bypassed by toggling email case. R002:
// without normalisation an attacker rotates Bob/BOB/bOB permutations and
// each gets its own 5-failure budget under `loginFailures`.
func TestAuthorizePost_LoginFailureBucketSharedAcrossEmailCases(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	seedLearner(t, store, "bob@x.com", "correct-password")

	mkForm := func(email, password string) url.Values {
		form := url.Values{}
		form.Set("csrf_token", "tkn")
		form.Set("mode", "login")
		form.Set("client_id", "cid")
		form.Set("redirect_uri", "https://good.example/cb")
		form.Set("code_challenge", "ch")
		form.Set("code_challenge_method", "S256")
		form.Set("email", email)
		form.Set("password", password)
		form.Set("approve_client", "yes")
		return form
	}
	postLogin := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
		rec := httptest.NewRecorder()
		s.HandleAuthorizePost(rec, req)
		return rec
	}

	// Burn the 5-failure budget on the mixed-case form.
	for i := 0; i < 5; i++ {
		rec := postLogin(mkForm("Bob@x.com", "wrong-password"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401; body=%q", i+1, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Invalid email or password") {
			t.Fatalf("attempt %d: expected invalid-credentials message; body=%q", i+1, rec.Body.String())
		}
	}

	// A sixth attempt under a DIFFERENT case with the CORRECT password must
	// be rejected by the failure tracker — proving the bucket is shared.
	rec := postLogin(mkForm("BOB@x.com", "correct-password"))
	if !strings.Contains(rec.Body.String(), "Too many failed attempts") {
		t.Fatalf("expected per-account lockout to cover case-variant; status=%d, body=%q", rec.Code, rec.Body.String())
	}
}

// TestAuthorizePost_RegisterRejectsCaseDuplicate verifies R002 register-side:
// once "alice@x.com" exists, "Alice@x.com" must also be refused, not silently
// accepted as a second account. Without normalisation the duplicate-check at
// HandleAuthorizePost runs under BINARY collation and lets the second row in.
func TestAuthorizePost_RegisterRejectsCaseDuplicate(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	seedLearner(t, store, "alice@x.com", "first-password")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "register")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "Alice@x.com") // case-variant of an existing learner
	form.Set("password", "second-password")
	form.Set("password_confirm", "second-password")
	form.Set("approve_client", "yes")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if !strings.Contains(rec.Body.String(), "account with this email already exists") {
		t.Fatalf("register accepted a case-variant duplicate; status=%d, body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandleRegister_CapsClientNameAt120Bytes verifies R001 hardening: an
// attacker registering a client with a multi-KB phishing name (e.g. an entire
// fake consent paragraph in client_name) is truncated to a manageable length
// before the value is echoed back or surfaced in the consent screen.
func TestHandleRegister_CapsClientNameAt120Bytes(t *testing.T) {
	s, _ := newTestServer(t)
	huge := strings.Repeat("A", 2000)
	body := fmt.Sprintf(`{"client_name":%q,"redirect_uris":["https://good.example/cb"]}`, huge)
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	name, _ := resp["client_name"].(string)
	if len(name) == 0 {
		t.Fatal("client_name missing from response")
	}
	if len(name) > 120 {
		t.Fatalf("client_name not capped: len=%d (want ≤ 120 bytes); got=%q…", len(name), name[:40])
	}
}
