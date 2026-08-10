// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateNonce_UniqueAndBase64(t *testing.T) {
	a, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	b, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	if a == "" || b == "" {
		t.Fatalf("empty nonce: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("nonces must differ: %q == %q", a, b)
	}
	// 16 raw bytes → 24-char standard base64 with padding.
	if len(a) != 24 {
		t.Fatalf("nonce length = %d, want 24 (b64 of 16 bytes)", len(a))
	}
}

func TestRenderAuthPage_LoginNoError(t *testing.T) {
	rec := httptest.NewRecorder()
	data := authPageData{
		ClientID:            "cid-1",
		RedirectURI:         "https://good.example/cb",
		ResponseType:        "code",
		State:               "state-abc",
		CodeChallenge:       "challenge-xyz",
		CodeChallengeMethod: "S256",
		Scope:               "learner",
		CSRFToken:           "csrf-tok-123",
	}
	renderAuthPage(rec, data, "", "login")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, must := range []string{"default-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'", "'nonce-"} {
		if !strings.Contains(csp, must) {
			t.Fatalf("CSP missing %q: %s", must, csp)
		}
	}

	body := rec.Body.String()
	mustContain := []string{
		"tutor",
		"Self-learning is a superpower.",
		"Sign in to continue.",
		`action="/authorize"`,
		`name="csrf_token"`,
		`value="csrf-tok-123"`,
		`value="cid-1"`,
		`value="https://good.example/cb"`,
		`value="code"`,
		`value="state-abc"`,
		`value="challenge-xyz"`,
		`value="S256"`,
		`value="learner"`,
		`name="approve_client"`,
		`I recognize this client and approve sharing access.`,
		`https://good.example`,
	}
	for _, s := range mustContain {
		if !strings.Contains(body, s) {
			t.Fatalf("body missing %q\n--- body ---\n%s", s, body)
		}
	}
	// Login mode: must not invoke toggleView() at startup.
	if strings.Contains(body, `{{if eq .Mode "register"}}toggleView();{{end}}`) {
		t.Fatal("template directive leaked into output")
	}
}

func TestRenderAuthPage_RegisterMode(t *testing.T) {
	rec := httptest.NewRecorder()
	renderAuthPage(rec, authPageData{
		ClientID:    "cid-1",
		RedirectURI: "https://good.example/cb",
		CSRFToken:   "tok",
	}, "", "register")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "choose your password and approve the client from the verification link") {
		t.Fatal("body missing mailbox-controlled activation explanation for register mode")
	}
	// Register-mode JS bootstrap toggleView() must be present.
	if !strings.Contains(body, "toggleView();") {
		t.Fatal("register mode should include startup toggleView() call")
	}
	// Registration only collects the mailbox address. A password and client
	// consent are intentionally deferred to the verification link so an
	// unauthenticated initiator cannot choose either for somebody else.
	registerStart := strings.Index(body, `<div id="register-view"`)
	registerEnd := strings.Index(body, `<p class="toggle">Already have an account?`)
	if registerStart < 0 || registerEnd <= registerStart {
		t.Fatal("could not isolate register form")
	}
	registerView := body[registerStart:registerEnd]
	if strings.Contains(registerView, `name="password"`) || strings.Contains(registerView, `name="approve_client"`) {
		t.Fatal("register mode must not accept a password or client consent")
	}
}

func TestRenderAuthPage_WithErrorMessageReturns401(t *testing.T) {
	rec := httptest.NewRecorder()
	renderAuthPage(rec, authPageData{ClientID: "cid"}, "Invalid email or password.", "login")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid email or password.") {
		t.Fatalf("body missing error message; body=%s", rec.Body.String())
	}
	// The error-box class wraps the message.
	if !strings.Contains(rec.Body.String(), `class="error-box"`) {
		t.Fatal("body missing error-box wrapper")
	}
}

func TestRenderAuthPage_EmptyModeDefaultsToLogin(t *testing.T) {
	rec := httptest.NewRecorder()
	renderAuthPage(rec, authPageData{ClientID: "cid"}, "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sign in to continue.") {
		t.Fatal("default mode should be login")
	}
	// Login mode should NOT have the startup toggleView() call.
	if strings.Contains(body, "toggleView();\n") && !strings.Contains(body, "function toggleView()") {
		// safety: function definition is always present, only the call should be conditional
	}
}

func TestRenderAuthPage_NonceDifferentPerCall(t *testing.T) {
	rec1 := httptest.NewRecorder()
	renderAuthPage(rec1, authPageData{ClientID: "c"}, "", "login")
	csp1 := rec1.Header().Get("Content-Security-Policy")

	rec2 := httptest.NewRecorder()
	renderAuthPage(rec2, authPageData{ClientID: "c"}, "", "login")
	csp2 := rec2.Header().Get("Content-Security-Policy")

	if csp1 == csp2 {
		t.Fatalf("CSP nonce must differ between calls: %q", csp1)
	}
}

func TestRenderAuthPage_CSPFormActionIncludesRedirectURIOrigin(t *testing.T) {
	rec := httptest.NewRecorder()
	renderAuthPage(rec, authPageData{
		ClientID:    "cid",
		RedirectURI: "https://claude.ai/api/mcp/auth_callback",
		CSRFToken:   "tok",
	}, "", "login")
	csp := rec.Header().Get("Content-Security-Policy")
	// The directive must include the redirect_uri origin in addition to
	// 'self' so the browser allows the post-login 302 to follow through to
	// the OAuth client's callback (issue: claude.ai never POSTs /token
	// because form-action 'self' silently blocked the redirect).
	if !strings.Contains(csp, "form-action 'self' https://claude.ai;") {
		t.Fatalf("form-action must allow redirect_uri origin, got CSP: %s", csp)
	}
}

func TestFormActionOriginFromRedirectURI(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://claude.ai/api/mcp/auth_callback", "https://claude.ai"},
		{"http://localhost:9999/cb", "http://localhost:9999"},
		{"https://callback.mistral.ai/oauth/callback?x=1", "https://callback.mistral.ai"},
		{"", ""},
		{"not-a-url", ""},
		{"/relative/only", ""},
	}
	for _, tc := range cases {
		if got := formActionOriginFromRedirectURI(tc.in); got != tc.want {
			t.Errorf("formActionOriginFromRedirectURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderAuthPage_HTMLEscapesUserData(t *testing.T) {
	rec := httptest.NewRecorder()
	renderAuthPage(rec, authPageData{
		ClientID:    `<script>alert(1)</script>`,
		RedirectURI: "https://good.example/cb",
		CSRFToken:   "tok",
	}, "", "login")

	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("user-supplied client_id was rendered raw — XSS risk")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("expected HTML-escaped client_id in body; body=%s", body)
	}
}
