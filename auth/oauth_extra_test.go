// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"tutor-mcp/models"
)

func futureTime() time.Time { return time.Now().Add(time.Hour) }
func pastTime() time.Time   { return time.Now().Add(-time.Hour) }

func assertLearnerScopesSupported(t *testing.T, meta map[string]interface{}, want []string) {
	t.Helper()
	raw, ok := meta["scopes_supported"].([]interface{})
	if !ok {
		t.Fatalf("scopes_supported = %T, want array", meta["scopes_supported"])
	}
	if len(raw) != len(want) {
		t.Fatalf("scopes_supported = %v, want exactly %v", raw, want)
	}
	for i, value := range raw {
		scope, ok := value.(string)
		if !ok {
			t.Fatalf("non-string supported scope: %T", value)
		}
		if scope != want[i] {
			t.Fatalf("scopes_supported = %v, want exactly %v", raw, want)
		}
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
	if meta["client_id_metadata_document_supported"] != true {
		t.Fatalf("CIMD support not advertised: %v", meta["client_id_metadata_document_supported"])
	}
	if meta["authorization_response_iss_parameter_supported"] != true {
		t.Fatalf("iss parameter support flag missing or false: %v", meta["authorization_response_iss_parameter_supported"])
	}
	assertLearnerScopesSupported(t, meta, []string{models.OAuthScopeLearner})
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
	assertLearnerScopesSupported(t, meta, []string{models.OAuthScopeLearner})
}

func TestOAuthMetadataGranularScopeRollout(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetGranularScopesEnabled(true)

	t.Run("authorization server publishes granular capabilities only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.HandleAuthServerMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
		var meta map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		assertLearnerScopesSupported(t, meta, []string{
			models.OAuthScopeLearnerRead,
			models.OAuthScopeLearnerWrite,
		})
	})

	t.Run("protected resource bootstraps with read only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.HandleProtectedResourceMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
		var meta map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		assertLearnerScopesSupported(t, meta, []string{models.OAuthScopeLearnerRead})
	})
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

	if err := store.CreateAuthCodeWithBinding(context.Background(), "the-code", learnerID, challenge, "", "cid", "", testOAuthResource, futureTime()); err != nil {
		t.Fatalf("seed code: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("resource", testOAuthResource)
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
	if resp["expires_in"].(float64) != AccessTokenTTL.Seconds() {
		t.Fatalf("expires_in = %v", resp["expires_in"])
	}
	claims, err := VerifyJWTClaims(resp["access_token"].(string), "https://test.example")
	if err != nil {
		t.Fatalf("verify access token: %v", err)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != testOAuthResource {
		t.Fatalf("access token audience = %v, want [%s]", claims.Audience, testOAuthResource)
	}
}

func TestHandleAuthorizeRequiresExactResource(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid-resource", "https://good.example/cb")

	base := url.Values{
		"client_id":             {"cid-resource"},
		"redirect_uri":          {"https://good.example/cb"},
		"response_type":         {"code"},
		"code_challenge":        {"challenge"},
		"code_challenge_method": {"S256"},
	}
	for _, tc := range []struct {
		name, resource string
		want           int
	}{
		{name: "missing", want: http.StatusBadRequest},
		{name: "wrong endpoint", resource: "https://test.example/other", want: http.StatusBadRequest},
		{name: "wrong origin", resource: "https://other.example/mcp", want: http.StatusBadRequest},
		{name: "canonical", resource: testOAuthResource, want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			for key, values := range base {
				q[key] = append([]string(nil), values...)
			}
			if tc.resource != "" {
				q.Set("resource", tc.resource)
			}
			req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
			rec := httptest.NewRecorder()
			s.HandleAuthorizeGet(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK && (!strings.Contains(rec.Body.String(), `name="resource"`) ||
				!strings.Contains(rec.Body.String(), `value="`+testOAuthResource+`"`)) {
				t.Fatalf("authorization form did not preserve exact resource")
			}
		})
	}
}

func TestAuthorizationCodeGrantRequiresMatchingResourceWithoutConsumingCode(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid-resource-code", "https://good.example/cb")
	learnerID := seedLearner(t, store, "resource-code@example.com", "pw123")
	verifier := "resource-bound-verifier"
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	if err := store.CreateAuthCodeWithBinding(
		context.Background(), "resource-code", learnerID, challenge, "S256",
		"cid-resource-code", "https://good.example/cb", testOAuthResource, futureTime(),
	); err != nil {
		t.Fatalf("seed code: %v", err)
	}

	exchange := func(resource string, include bool) *httptest.ResponseRecorder {
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {"resource-code"},
			"code_verifier": {verifier},
			"client_id":     {"cid-resource-code"},
			"redirect_uri":  {"https://good.example/cb"},
		}
		if include {
			form.Set("resource", resource)
		}
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.HandleToken(rec, req)
		return rec
	}

	if rec := exchange("", false); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("missing resource = %d %q", rec.Code, rec.Body.String())
	}
	if rec := exchange("https://other.example/mcp", true); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("wrong resource = %d %q", rec.Code, rec.Body.String())
	}
	if rec := exchange(testOAuthResource, true); rec.Code != http.StatusOK {
		t.Fatalf("canonical resource after rejected attempts = %d %q", rec.Code, rec.Body.String())
	}
}

func TestHandleToken_AuthorizationCode_MissingCode(t *testing.T) {
	setTestSecret(t)
	s, _ := newTestServer(t)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("resource", testOAuthResource)
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
	form.Set("resource", testOAuthResource)
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
	form.Set("resource", testOAuthResource)
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
	if err := store.CreateAuthCodeWithBinding(context.Background(), "conf-code", learner, challenge, "", "cid-conf", "", testOAuthResource, futureTime()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("resource", testOAuthResource)
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
	form.Set("resource", testOAuthResource)
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
	if err := store.CreateAuthCodeWithBinding(context.Background(), "exp-code", learner, challenge, "", "cid", "", testOAuthResource, pastTime()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("resource", testOAuthResource)
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

	if err := store.CreateAuthCodeWithBinding(context.Background(), "pkce-code", learner, "wrong-challenge", "", "cid", "", testOAuthResource, futureTime()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("resource", testOAuthResource)
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
	if _, err := store.GetAuthCode(context.Background(), "pkce-code", "cid"); err != nil {
		t.Fatalf("PKCE mismatch consumed the authorization code: %v", err)
	}
}

func TestHandleToken_AuthorizationCode_RedirectMismatchDoesNotConsume(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	const redirectURI = "https://good.example/cb"
	seedClient(t, store, "cid", redirectURI)
	learner := seedLearner(t, store, "u-redirect-bind@e.com", "pw")

	verifier := strings.Repeat("v", 43)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	if err := store.CreateAuthCodeWithBinding(
		context.Background(), "redirect-bound-code", learner, challenge, "S256",
		"cid", redirectURI, testOAuthResource, futureTime(),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	request := func(redirect string) *httptest.ResponseRecorder {
		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("resource", testOAuthResource)
		form.Set("code", "redirect-bound-code")
		form.Set("code_verifier", verifier)
		form.Set("client_id", "cid")
		form.Set("redirect_uri", redirect)
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.HandleToken(rec, req)
		return rec
	}

	if rec := request("https://attacker.example/cb"); rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatch status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if _, err := store.GetAuthCode(context.Background(), "redirect-bound-code", "cid"); err != nil {
		t.Fatalf("redirect mismatch consumed the authorization code: %v", err)
	}
	if rec := request(redirectURI); rec.Code != http.StatusOK {
		t.Fatalf("matching redirect status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleToken_ConfidentialClientRejectsPKCEDowngrade(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("real-secret"), bcrypt.MinCost)
	if err := store.CreateOAuthClientWithSecret(context.Background(), "cid-conf-down", "Confidential", `["https://c.example/cb"]`, string(hash)); err != nil {
		t.Fatalf("create confidential client: %v", err)
	}
	learner := seedLearner(t, store, "u-conf-down@e.com", "pw")
	if err := store.CreateAuthCodeWithBinding(context.Background(), "conf-down-code", learner, "", "", "cid-conf-down", "", testOAuthResource, futureTime()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("resource", testOAuthResource)
	form.Set("code", "conf-down-code")
	form.Set("code_verifier", strings.Repeat("v", 43))
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cid-conf-down", "real-secret")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("status/body = %d %q, want 400 invalid_grant", rec.Code, rec.Body.String())
	}
	if _, err := store.GetAuthCode(context.Background(), "conf-down-code", "cid-conf-down"); err != nil {
		t.Fatalf("downgrade attempt consumed the authorization code: %v", err)
	}
}

// ─── handleRefreshTokenGrant ─────────────────────────────────────────────────

func TestHandleToken_RefreshToken_MissingToken(t *testing.T) {
	setTestSecret(t)
	s, _ := newTestServer(t)

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("resource", testOAuthResource)
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
	form.Set("resource", testOAuthResource)
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
	rt, err := store.CreateRefreshToken(context.Background(), learner, "cid-pub", testOAuthResource)
	if err != nil {
		t.Fatalf("seed rt: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("resource", testOAuthResource)
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
	if resp["scope"] != models.OAuthScopeLearner {
		t.Fatalf("legacy refresh scope = %v, want %q", resp["scope"], models.OAuthScopeLearner)
	}
	legacyAccess, _ := resp["access_token"].(string)
	legacyClaims, err := VerifyJWTClaims(legacyAccess, "https://test.example")
	if err != nil || legacyClaims.Scope != models.OAuthScopeLearner {
		t.Fatalf("legacy access claims = %+v, err=%v", legacyClaims, err)
	}
	stored, err := store.GetRefreshToken(context.Background(), newRT)
	if err != nil || stored.Scope != models.OAuthScopeLearner {
		t.Fatalf("legacy successor = %+v, err=%v", stored, err)
	}
	if _, err := store.GetRefreshToken(context.Background(), rt.Token); err == nil {
		t.Fatal("old refresh token must be deleted after rotation")
	}
}

func TestHandleToken_RefreshScopeNarrowsButNeverWidens(t *testing.T) {
	setTestSecret(t)
	s, oauthStore := newTestServer(t)
	s.SetGranularScopesEnabled(true)
	const clientID = "cid-refresh-scope"
	seedClient(t, oauthStore, clientID, "https://app.example/cb")
	learner := seedLearner(t, oauthStore, "refresh-scope@example.com", "pw")
	root, err := oauthStore.CreateRefreshTokenWithScope(
		context.Background(), learner, clientID, testOAuthResource,
		models.OAuthScopeLearnerReadWrite,
	)
	if err != nil {
		t.Fatalf("seed scoped refresh token: %v", err)
	}

	refresh := func(token, scope string) (*httptest.ResponseRecorder, map[string]interface{}) {
		t.Helper()
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"resource":      {testOAuthResource},
			"refresh_token": {token},
			"client_id":     {clientID},
		}
		if scope != "" {
			form.Set("scope", scope)
		}
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.HandleToken(rec, req)
		var response map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &response)
		return rec, response
	}

	narrowedRec, narrowed := refresh(root.Token, models.OAuthScopeLearnerRead)
	if narrowedRec.Code != http.StatusOK {
		t.Fatalf("narrow refresh status = %d; body=%q", narrowedRec.Code, narrowedRec.Body.String())
	}
	if narrowed["scope"] != models.OAuthScopeLearnerRead {
		t.Fatalf("narrow response scope = %v", narrowed["scope"])
	}
	narrowedAccess, _ := narrowed["access_token"].(string)
	claims, err := VerifyJWTClaims(narrowedAccess, "https://test.example")
	if err != nil || claims.Scope != models.OAuthScopeLearnerRead {
		t.Fatalf("narrow JWT claims = %+v, err=%v", claims, err)
	}
	narrowedToken, _ := narrowed["refresh_token"].(string)
	storedNarrowed, err := oauthStore.GetRefreshToken(context.Background(), narrowedToken)
	if err != nil || storedNarrowed.Scope != models.OAuthScopeLearnerRead {
		t.Fatalf("narrow refresh token = %+v, err=%v", storedNarrowed, err)
	}

	widenRec, widen := refresh(narrowedToken, models.OAuthScopeLearnerWrite)
	if widenRec.Code != http.StatusBadRequest || widen["error"] != "invalid_scope" {
		t.Fatalf("widen response = %d %q", widenRec.Code, widenRec.Body.String())
	}
	if stillActive, err := oauthStore.GetRefreshToken(context.Background(), narrowedToken); err != nil || stillActive.Scope != models.OAuthScopeLearnerRead {
		t.Fatalf("rejected widening consumed or changed token: token=%+v err=%v", stillActive, err)
	}

	preservedRec, preserved := refresh(narrowedToken, "")
	if preservedRec.Code != http.StatusOK || preserved["scope"] != models.OAuthScopeLearnerRead {
		t.Fatalf("omitted scope must preserve grant: %d %q", preservedRec.Code, preservedRec.Body.String())
	}
}

func TestHandleToken_PhaseARejectsGranularRefreshNarrowing(t *testing.T) {
	setTestSecret(t)
	s, oauthStore := newTestServer(t) // rollout flag intentionally remains OFF
	const clientID = "cid-refresh-phase-a"
	seedClient(t, oauthStore, clientID, "https://app.example/cb")
	learner := seedLearner(t, oauthStore, "refresh-phase-a@example.com", "pw")
	root, err := oauthStore.CreateRefreshTokenWithScope(
		context.Background(), learner, clientID, testOAuthResource, models.OAuthScopeLearner,
	)
	if err != nil {
		t.Fatalf("seed legacy refresh token: %v", err)
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"resource":      {testOAuthResource},
		"refresh_token": {root.Token},
		"client_id":     {clientID},
		"scope":         {models.OAuthScopeLearnerRead},
	}
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_scope") {
		t.Fatalf("status=%d body=%q, want 400 invalid_scope", rec.Code, rec.Body.String())
	}
	if stored, err := oauthStore.GetRefreshToken(context.Background(), root.Token); err != nil || stored.Scope != models.OAuthScopeLearner {
		t.Fatalf("rejected narrowing consumed or changed legacy refresh token: token=%+v err=%v", stored, err)
	}
}

func TestRefreshGrantRequiresMatchingResourceWithoutRotating(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid-resource-refresh", "https://app.example/cb")
	learner := seedLearner(t, store, "resource-refresh@example.com", "pw")
	rt, err := store.CreateRefreshToken(context.Background(), learner, "cid-resource-refresh", testOAuthResource)
	if err != nil {
		t.Fatalf("seed refresh token: %v", err)
	}

	refresh := func(resource string, include bool) *httptest.ResponseRecorder {
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {rt.Token},
			"client_id":     {"cid-resource-refresh"},
		}
		if include {
			form.Set("resource", resource)
		}
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.HandleToken(rec, req)
		return rec
	}

	if rec := refresh("", false); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("missing resource = %d %q", rec.Code, rec.Body.String())
	}
	if rec := refresh("https://other.example/mcp", true); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("wrong resource = %d %q", rec.Code, rec.Body.String())
	}
	if rec := refresh(testOAuthResource, true); rec.Code != http.StatusOK {
		t.Fatalf("canonical resource after rejected attempts = %d %q", rec.Code, rec.Body.String())
	}
}

func TestHandleToken_RefreshTokenReuseRevokesSuccessor(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid-pub", "https://app.example/cb")
	learner := seedLearner(t, store, "u-rt-reuse@e.com", "pw")
	root, err := store.CreateRefreshToken(context.Background(), learner, "cid-pub", testOAuthResource)
	if err != nil {
		t.Fatalf("seed rt: %v", err)
	}

	refresh := func(token string) *httptest.ResponseRecorder {
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("resource", testOAuthResource)
		form.Set("refresh_token", token)
		form.Set("client_id", "cid-pub")
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.HandleToken(rec, req)
		return rec
	}

	first := refresh(root.Token)
	if first.Code != http.StatusOK {
		t.Fatalf("first refresh = %d %q", first.Code, first.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode first refresh: %v", err)
	}
	successor, _ := payload["refresh_token"].(string)
	if successor == "" {
		t.Fatal("missing successor refresh token")
	}

	if replay := refresh(root.Token); replay.Code != http.StatusBadRequest || !strings.Contains(replay.Body.String(), "invalid_grant") {
		t.Fatalf("replay = %d %q, want 400 invalid_grant", replay.Code, replay.Body.String())
	}
	if descendant := refresh(successor); descendant.Code != http.StatusBadRequest || !strings.Contains(descendant.Body.String(), "invalid_grant") {
		t.Fatalf("revoked descendant = %d %q, want 400 invalid_grant", descendant.Code, descendant.Body.String())
	}
}

func TestHandleToken_RefreshTokenConcurrentReuseSingleSuccess(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	seedClient(t, store, "cid-pub", "https://app.example/cb")
	learner := seedLearner(t, store, "u-rt-race@e.com", "pw")
	rt, err := store.CreateRefreshToken(context.Background(), learner, "cid-pub", testOAuthResource)
	if err != nil {
		t.Fatalf("seed rt: %v", err)
	}

	const contenders = 8
	start := make(chan struct{})
	type result struct {
		status int
		body   string
	}
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			form := url.Values{}
			form.Set("grant_type", "refresh_token")
			form.Set("resource", testOAuthResource)
			form.Set("refresh_token", rt.Token)
			form.Set("client_id", "cid-pub")
			req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			s.HandleToken(rec, req)
			results <- result{status: rec.Code, body: rec.Body.String()}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	rejections := 0
	for result := range results {
		switch result.status {
		case http.StatusOK:
			successes++
		case http.StatusBadRequest:
			rejections++
			if !strings.Contains(result.body, "invalid_grant") {
				t.Fatalf("loser body = %q, want invalid_grant", result.body)
			}
		default:
			t.Fatalf("unexpected concurrent refresh status %d: %q", result.status, result.body)
		}
	}
	if successes != 1 || rejections != contenders-1 {
		t.Fatalf("concurrent refresh results: successes=%d rejections=%d", successes, rejections)
	}
}

func TestHandleToken_RefreshToken_ConfidentialClientUnknown(t *testing.T) {
	setTestSecret(t)
	s, store := newTestServer(t)
	learner := seedLearner(t, store, "u-rt2@e.com", "pw")
	rt, err := store.CreateRefreshToken(context.Background(), learner, "ghost-client", testOAuthResource)
	if err != nil {
		t.Fatalf("seed rt: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("resource", testOAuthResource)
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
	rt, err := store.CreateRefreshToken(context.Background(), learner, "cid-c2", testOAuthResource)
	if err != nil {
		t.Fatalf("seed rt: %v", err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("resource", testOAuthResource)
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
	if resp["application_type"] != "web" {
		t.Fatalf("application_type = %v, want web default", resp["application_type"])
	}
	if expires, ok := resp["client_id_expires_at"].(float64); !ok || expires <= float64(time.Now().Unix()) {
		t.Fatalf("client_id_expires_at = %v, want future timestamp", resp["client_id_expires_at"])
	}
	if _, ok := resp["client_secret"]; ok {
		t.Fatal("public client must NOT get a client_secret")
	}
	if _, ok := resp["registration_access_token"]; ok {
		t.Fatal("response advertises an unimplemented RFC 7592 credential")
	}
	if _, ok := resp["registration_client_uri"]; ok {
		t.Fatal("response advertises an unimplemented RFC 7592 management endpoint")
	}
	if _, ok := resp["scope"]; ok {
		t.Fatal("dynamic client registration response must not advertise a false client scope restriction")
	}
}

func TestHandleRegister_DoesNotLogRedirectURISecrets(t *testing.T) {
	s, _ := newTestServer(t)
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))

	const marker = "registration-query-secret"
	body := `{"client_name":"Public","redirect_uris":["https://app.example/cb?bootstrap=` + marker + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), marker) || strings.Contains(logs.String(), "https://app.example/cb") {
		t.Fatalf("registration log disclosed redirect URI: %s", logs.String())
	}
}

func TestAuthLogIdentifierRemovesRecordDelimitersAndRawSecrets(t *testing.T) {
	const raw = "https://client.example/id?secret=one\r\nforged=value"
	first := authLogIdentifier(raw)
	second := authLogIdentifier(raw)
	other := authLogIdentifier(raw + "-other")
	if first != second {
		t.Fatalf("identifier fingerprint is not deterministic: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("distinct identifiers share fingerprint %q", first)
	}
	for _, forbidden := range []string{"secret=one", "\r", "\n", "forged=value"} {
		if strings.Contains(first, forbidden) {
			t.Fatalf("fingerprint %q contains unsafe value %q", first, forbidden)
		}
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("fingerprint=%q", first)
	}
}

func TestAuthorizeParameterErrorsDoNotLogUntrustedScope(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			s, _ := newTestServer(t)
			var logs bytes.Buffer
			s.logger = slog.New(slog.NewTextHandler(&logs, nil))
			const maliciousScope = "unsupported-scope\r\nforged=value"

			var req *http.Request
			if method == http.MethodGet {
				q := url.Values{
					"client_id":             {"cid"},
					"redirect_uri":          {"https://good.example/cb"},
					"resource":              {MCPResource(s.baseURL)},
					"response_type":         {"code"},
					"scope":                 {maliciousScope},
					"code_challenge":        {"challenge"},
					"code_challenge_method": {"S256"},
				}
				req = httptest.NewRequest(method, "/authorize?"+q.Encode(), nil)
			} else {
				csrf := "csrf-parameter-log-test"
				form := url.Values{
					"csrf_token":            {csrf},
					"client_id":             {"cid"},
					"redirect_uri":          {"https://good.example/cb"},
					"resource":              {MCPResource(s.baseURL)},
					"response_type":         {"code"},
					"scope":                 {maliciousScope},
					"code_challenge":        {"challenge"},
					"code_challenge_method": {"S256"},
				}
				req = httptest.NewRequest(method, "/authorize", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.AddCookie(&http.Cookie{Name: "csrf_token", Value: csrf})
			}

			rec := httptest.NewRecorder()
			if method == http.MethodGet {
				s.HandleAuthorizeGet(rec, req)
			} else {
				s.HandleAuthorizePost(rec, req)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if strings.Contains(logs.String(), maliciousScope) || strings.Contains(logs.String(), "forged=value") {
				t.Fatalf("untrusted scope reached log: %q", logs.String())
			}
		})
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

func TestHandleRegister_RejectsUnknownTokenEndpointAuthMethod(t *testing.T) {
	s, _ := newTestServer(t)
	body := `{"client_name":"Bad","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"private_key_jwt"}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.HandleRegister(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_client_metadata") {
		t.Fatalf("body = %q, want invalid_client_metadata", rec.Body.String())
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
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
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
	budget := mustBcryptBudget(1)
	cases := []struct {
		name    string
		client  *models.OAuthClient
		secret  string
		wantErr bool
	}{
		{"public client passes", &models.OAuthClient{ClientSecretHash: ""}, "", false},
		{"public client passes even with secret", &models.OAuthClient{ClientSecretHash: ""}, "anything", false},
		{"confidential missing secret", &models.OAuthClient{ClientSecretHash: string(hash)}, "", true},
		{"confidential wrong secret", &models.OAuthClient{ClientSecretHash: string(hash)}, "wrong", true},
		{"confidential right secret", &models.OAuthClient{ClientSecretHash: string(hash)}, "s3cret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyClientAuthWithBudget(context.Background(), budget, tc.client, tc.secret)
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
	writeTokenResponse(rec, "AT", "RT", models.OAuthScopeLearner)
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

// ─── generateCode / generateCSRFToken ───────────────────────────────────────

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
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
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
	form.Set("resource", testOAuthResource)
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
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
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

func TestAuthorizePost_RegisterRequiresEmailVerificationBeforeRedirect(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "register")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
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

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%q", rec.Code, rec.Body.String())
	}
	learner, err := store.GetLearnerByEmail(context.Background(), "newuser@e.com")
	if err != nil || learner == nil {
		t.Fatalf("learner not created: err=%v learner=%v", err, learner)
	}
	if learner.EmailVerifiedAt != nil {
		t.Fatal("registration activated identity before mailbox proof")
	}
	if bcrypt.CompareHashAndPassword([]byte(learner.PasswordHash), []byte("password-1234")) == nil {
		t.Fatal("registration initiator chose a usable pending credential")
	}
	if approved, err := store.IsClientApproved(context.Background(), learner.ID, "cid", "https://good.example/cb"); err != nil || approved {
		t.Fatalf("registration persisted consent before mailbox proof: approved=%v err=%v", approved, err)
	}
	sender := s.emailSender.(*testEmailSender)
	if len(sender.verificationLinks) != 1 || sender.verificationTo[0] != "newuser@e.com" {
		t.Fatalf("verification deliveries = %+v / %+v", sender.verificationTo, sender.verificationLinks)
	}
	verificationURL, err := url.Parse(sender.verificationLinks[0])
	if err != nil {
		t.Fatalf("parse verification URL: %v", err)
	}
	rawToken := verificationURL.Query().Get("token")
	getRec := httptest.NewRecorder()
	s.HandleVerifyEmailGet(getRec, httptest.NewRequest(http.MethodGet, verificationURL.String(), nil))
	if getRec.Code != http.StatusOK || len(getRec.Result().Cookies()) == 0 {
		t.Fatalf("verification confirmation status=%d", getRec.Code)
	}
	if body := getRec.Body.String(); !strings.Contains(body, "Test Client") ||
		!strings.Contains(body, "https://good.example") ||
		!strings.Contains(body, "Requested permissions:") ||
		!strings.Contains(body, "Read and modify your learner profile") ||
		!strings.Contains(body, `name="password"`) ||
		!strings.Contains(body, `name="approve_client"`) {
		t.Fatalf("verification page does not disclose the credential/consent boundary: %q", body)
	}
	csrf := getRec.Result().Cookies()[0].Value
	verifyForm := url.Values{
		"token": {rawToken}, "csrf_token": {csrf},
		"password": {"owner-password-123"}, "password_confirm": {"owner-password-123"},
		"approve_client": {"yes"},
	}
	verifyReq := httptest.NewRequest(http.MethodPost, "/verify-email", strings.NewReader(verifyForm.Encode()))
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	verifyReq.AddCookie(&http.Cookie{Name: accountCSRFCookieName, Value: csrf})
	verifyRec := httptest.NewRecorder()
	s.HandleVerifyEmailPost(verifyRec, verifyReq)
	if verifyRec.Code != http.StatusFound {
		t.Fatalf("verification status=%d body=%q", verifyRec.Code, verifyRec.Body.String())
	}
	callback, err := url.Parse(verifyRec.Header().Get("Location"))
	if err != nil || callback.Query().Get("code") == "" {
		t.Fatalf("verification callback=%q err=%v", verifyRec.Header().Get("Location"), err)
	}
	learner, err = store.GetLearnerByEmail(context.Background(), "newuser@e.com")
	if err != nil || learner.EmailVerifiedAt == nil ||
		bcrypt.CompareHashAndPassword([]byte(learner.PasswordHash), []byte("owner-password-123")) != nil ||
		bcrypt.CompareHashAndPassword([]byte(learner.PasswordHash), []byte("password-1234")) == nil {
		t.Fatalf("mailbox owner did not replace the pending credential: learner=%+v err=%v", learner, err)
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
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
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
	if !strings.Contains(rec.Body.String(), "Email is required.") {
		t.Fatalf("expected error message, got %q", rec.Body.String())
	}
}

func TestAuthorizePost_RegisterDoesNotAcceptInitiatorCredential(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "register")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "x@e.com")
	form.Set("password", "attacker-password")
	form.Set("password_confirm", "attacker-password")
	form.Set("approve_client", "yes")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	learner, err := store.GetLearnerByEmail(context.Background(), "x@e.com")
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(learner.PasswordHash), []byte("attacker-password")) == nil {
		t.Fatal("unauthenticated registration password became usable")
	}
	approved, err := store.IsClientApproved(context.Background(), learner.ID, "cid", "https://good.example/cb")
	if err != nil || approved {
		t.Fatalf("unauthenticated registration consent persisted: approved=%v err=%v", approved, err)
	}
}

func TestAuthorizePost_RegisterNeedsOnlyEmail(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")

	form := url.Values{}
	form.Set("csrf_token", "tkn")
	form.Set("mode", "register")
	form.Set("client_id", "cid")
	form.Set("redirect_uri", "https://good.example/cb")
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", "x@e.com")

	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "tkn"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%q", rec.Code, rec.Body.String())
	}
	if _, err := store.GetLearnerByEmail(context.Background(), "x@e.com"); err != nil {
		t.Fatalf("email-only pending registration failed: %v", err)
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
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
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

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Check your email") {
		t.Fatalf("expected non-enumerating registration message; got %q", rec.Body.String())
	}
	if len(s.emailSender.(*testEmailSender).verificationLinks) != 0 {
		t.Fatal("duplicate registration must not spam the existing account")
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
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
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
		{"suffix hostname", "cid", "https://good.example.attacker.example/cb", true},
		{"userinfo", "cid", "https://good.example@attacker.example/cb", true},
		{"scheme relative", "cid", "//good.example/cb", true},
		{"backslash", "cid", `https://good.example\\attacker.example/cb`, true},
		{"explicit default port", "cid", "https://good.example:443/cb", true},
		{"trailing dot", "cid", "https://good.example./cb", true},
		{"extra query", "cid", "https://good.example/cb?next=https://attacker.example", true},
		{"fragment", "cid", "https://good.example/cb#https://attacker.example", true},
		{"encoded slash", "cid", "https://good.example%2fattacker.example/cb", true},
		{"encoded backslash", "cid", "https://good.example%5cattacker.example/cb", true},
		{"encoded CRLF", "cid", "https://good.example/cb%0d%0aLocation:%20https://attacker.example", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.validateRedirectURI(context.Background(), tc.clientID, tc.redirectURI)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error; got %q", got)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr && got != "https://good.example/cb" {
				t.Fatalf("registered redirect=%q", got)
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
	csrf, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generate csrf: %v", err)
	}
	form := url.Values{}
	form.Set("csrf_token", csrf)
	form.Set("mode", "login")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirectURI)
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
	form.Set("code_challenge", "ch")
	form.Set("code_challenge_method", "S256")
	form.Set("email", email)
	form.Set("password", password)
	if approveClient {
		form.Set("approve_client", "yes")
	}
	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: csrf})
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
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
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

// Failure accounting is case-normalized. A correct password at the threshold
// does not grant a code to an unfamiliar device: mailbox confirmation creates
// a bounded trusted-device cookie, after which the owner can sign in normally.
func TestAuthorizePost_LoginFailureThresholdRequiresRecoverableDeviceChallenge(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	seedLearner(t, store, "bob@x.com", "correct-password")

	mkForm := func(email, password string) url.Values {
		form := url.Values{}
		form.Set("csrf_token", "tkn")
		form.Set("mode", "login")
		form.Set("client_id", "cid")
		form.Set("redirect_uri", "https://good.example/cb")
		form.Set("response_type", "code")
		form.Set("resource", testOAuthResource)
		form.Set("code_challenge", "ch")
		form.Set("code_challenge_method", "S256")
		form.Set("email", email)
		form.Set("password", password)
		form.Set("approve_client", "yes")
		return form
	}
	postLogin := func(form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		csrf, err := generateCSRFToken()
		if err != nil {
			t.Fatalf("generate csrf: %v", err)
		}
		form.Set("csrf_token", csrf)
		req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "csrf_token", Value: csrf})
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		s.HandleAuthorizePost(rec, req)
		return rec
	}

	// Burn the 5-failure budget on the mixed-case form.
	for i := 0; i < 5; i++ {
		rec := postLogin(mkForm("Bob@x.com", "wrong-password"))
		wantStatus := http.StatusUnauthorized
		if i == 4 {
			wantStatus = http.StatusTooManyRequests
		}
		if rec.Code != wantStatus {
			t.Fatalf("attempt %d: status = %d, want %d; body=%q", i+1, rec.Code, wantStatus, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Invalid email or password") {
			t.Fatalf("attempt %d: expected invalid-credentials message; body=%q", i+1, rec.Body.String())
		}
		if i == 4 && rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("attempt %d: Retry-After=%q, want 1", i+1, rec.Header().Get("Retry-After"))
		}
	}

	// Correct credentials now trigger a mailbox challenge rather than a global
	// account lockout or an immediate authorization grant.
	rec := postLogin(mkForm("BOB@x.com", "correct-password"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("adaptive challenge status=%d, want 202; body=%q", rec.Code, rec.Body.String())
	}
	sender := s.emailSender.(*testEmailSender)
	if len(sender.challengeLinks) != 1 || len(sender.challengeTo) != 1 || sender.challengeTo[0] != "bob@x.com" {
		t.Fatalf("security challenge deliveries=%v recipients=%v", sender.challengeLinks, sender.challengeTo)
	}
	challengeURL, err := url.Parse(sender.challengeLinks[0])
	if err != nil {
		t.Fatal(err)
	}
	rawToken := challengeURL.Query().Get("token")
	getRec := httptest.NewRecorder()
	s.HandleLoginChallengeGet(getRec, httptest.NewRequest(http.MethodGet, challengeURL.String(), nil))
	if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), "Confirm this sign-in") {
		t.Fatalf("challenge confirmation status=%d body=%q", getRec.Code, getRec.Body.String())
	}
	var accountCookie *http.Cookie
	for _, cookie := range getRec.Result().Cookies() {
		if cookie.Name == accountCSRFCookieName {
			accountCookie = cookie
			break
		}
	}
	if accountCookie == nil {
		t.Fatal("challenge confirmation omitted CSRF cookie")
	}
	confirm := url.Values{"token": {rawToken}, "csrf_token": {accountCookie.Value}}
	confirmReq := httptest.NewRequest(http.MethodPost, "/login-challenge", strings.NewReader(confirm.Encode()))
	confirmReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmReq.AddCookie(accountCookie)
	confirmRec := httptest.NewRecorder()
	s.HandleLoginChallengePost(confirmRec, confirmReq)
	if confirmRec.Code != http.StatusFound || !strings.Contains(confirmRec.Header().Get("Location"), "/authorize?") {
		t.Fatalf("challenge confirmation status=%d location=%q", confirmRec.Code, confirmRec.Header().Get("Location"))
	}
	var trustedCookie *http.Cookie
	for _, cookie := range confirmRec.Result().Cookies() {
		if cookie.Name == trustedLoginDeviceCookieName {
			trustedCookie = cookie
			break
		}
	}
	if trustedCookie == nil || !trustedCookie.HttpOnly || !trustedCookie.Secure || trustedCookie.MaxAge <= 0 {
		t.Fatalf("trusted-device cookie=%+v", trustedCookie)
	}

	// The confirmed device succeeds immediately and clears the failure signal.
	rec = postLogin(mkForm("BOB@x.com", "correct-password"), trustedCookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("confirmed owner sign-in status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !s.loginFailures.Allow("bob@x.com") {
		t.Fatal("successful challenged sign-in did not reset the account failure signal")
	}

	// A copied email link cannot be consumed twice.
	replayCSRF := "challenge-replay-csrf"
	replayForm := url.Values{"token": {rawToken}, "csrf_token": {replayCSRF}}
	replayReq := httptest.NewRequest(http.MethodPost, "/login-challenge", strings.NewReader(replayForm.Encode()))
	replayReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replayReq.AddCookie(&http.Cookie{Name: accountCSRFCookieName, Value: replayCSRF})
	replayRec := httptest.NewRecorder()
	s.HandleLoginChallengePost(replayRec, replayReq)
	if replayRec.Code != http.StatusBadRequest {
		t.Fatalf("challenge replay status=%d, want 400", replayRec.Code)
	}
}

// TestAuthorizePost_RegisterRejectsCaseDuplicate verifies R002 register-side:
// once "alice@x.com" exists, "Alice@x.com" must not create a second account.
// The public response is intentionally identical to a pending registration.
// Without normalisation the duplicate-check at
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
	form.Set("response_type", "code")
	form.Set("resource", testOAuthResource)
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

	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "Check your email") {
		t.Fatalf("case-variant response status=%d, body=%q", rec.Code, rec.Body.String())
	}
	var count int
	if err := store.RawDB().QueryRow(`SELECT COUNT(*) FROM learners WHERE email = 'alice@x.com'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("canonical learner count=%d err=%v", count, err)
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
