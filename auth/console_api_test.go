// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestInstitutionStaffOAuthAndConsoleAPIBoundaries(t *testing.T) {
	s, store, server := institutionTestServer(t, true)
	b := newTestBrowser(t, server)
	_, page := b.get("/signup")
	b.post("/signup", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "tenant_name": {"Staff school"}, "tenant_slug": {"staff-school"}, "email": {"staff@school.test"}, "accept_terms": {"yes"}})
	_, page = b.get(s.emailSender.(*testEmailSender).signupLinks[0])
	_, page = b.post("/signup/complete", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "token": {field(t, tokenFieldPattern, page)}, "password": {"staff-password-2026"}, "password_confirm": {"staff-password-2026"}})
	secret, _ := enrollTOTP(t, b, page)
	if resp, _ := b.get("/console/test-api"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("console read = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/console/test-api", nil)
	if resp, _ := b.do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write without CSRF = %d", resp.StatusCode)
	}
	_, raw := b.get("/console/api-csrf")
	var csrf struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal([]byte(raw), &csrf); err != nil || csrf.Token == "" {
		t.Fatalf("CSRF token: %s %v", raw, err)
	}
	for i, want := range []int{http.StatusNoContent, http.StatusForbidden} {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/console/test-api", nil)
		req.Header.Set("X-CSRF-Token", csrf.Token)
		if resp, _ := b.do(req); resp.StatusCode != want {
			t.Fatalf("CSRF attempt %d = %d", i, resp.StatusCode)
		}
	}

	const clientID, callback, verifier = "staff-http", "https://client.example/callback", "staff-pkce-verifier-0000000000000000000000000000"
	seedClient(t, store, clientID, callback)
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {clientID}, "redirect_uri": {callback}, "response_type": {"code"}, "resource": {testOAuthResource}, "scope": {"formation:read formation:write"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	_, page = b.get("/authorize?" + query.Encode())
	query.Set("csrf_token", field(t, csrfPattern, page))
	query.Set("mode", "login")
	query.Set("email", "staff@school.test")
	query.Set("password", "staff-password-2026")
	query.Set("approve_client", "yes")
	query.Set("totp_code", testTOTP(t, secret, time.Now().Add(30*time.Second)))
	resp, page := b.post("/authorize", query)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("staff authorize = %d %.500s", resp.StatusCode, page)
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp, raw = b.post("/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "redirect_uri": {callback}, "resource": {testOAuthResource}, "code": {location.Query().Get("code")}, "code_verifier": {verifier}})
	var token struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Scope   string `json:"scope"`
	}
	if resp.StatusCode != 200 || json.Unmarshal([]byte(raw), &token) != nil {
		t.Fatalf("exchange = %d %s", resp.StatusCode, raw)
	}
	claims, err := VerifyJWTClaims(token.Access, "https://test.example")
	if err != nil || claims.LearnerID != "" || claims.Scope != "formation:read formation:write" {
		t.Fatalf("staff claims = %+v %v", claims, err)
	}
	stranger := newTestBrowser(t, server)
	stranger.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/console/test-api", nil)
	req.Header.Set("Authorization", "Bearer "+token.Access)
	if resp, _ := stranger.do(req); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("OAuth bearer opened console API: %d", resp.StatusCode)
	}
	resp, raw = b.post("/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "resource": {testOAuthResource}, "refresh_token": {token.Refresh}, "scope": {"formation:read"}})
	if resp.StatusCode != 200 || json.Unmarshal([]byte(raw), &token) != nil || token.Scope != "formation:read" {
		t.Fatalf("refresh = %d %s", resp.StatusCode, raw)
	}
	p, err := claims.Principal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMembershipAuthorization(t.Context(), p.TenantScope(), models.MembershipStatusActive, []string{models.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	// Even a still-privileged membership must not refresh a previous version.
	if _, err := store.RecordMembershipMFAVerification(t.Context(), p.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	resp, raw = b.post("/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "resource": {testOAuthResource}, "refresh_token": {token.Refresh}})
	if resp.StatusCode != 400 || !strings.Contains(raw, "invalid_grant") {
		t.Fatalf("stale membership refreshed: %d %s", resp.StatusCode, raw)
	}
}
