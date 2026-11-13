// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserCookiesUseHostPrefixOnHTTPS(t *testing.T) {
	s := NewOAuthServer(nil, "https://tutor.example", slog.Default())
	rec := httptest.NewRecorder()
	s.setAuthorizeCSRFCookie(rec, "authorize-token", 3600)
	s.setAccountCSRFCookie(rec, "account-token", "/verify-email", 1800)
	s.setTrustedLoginDeviceCookie(rec, "device-token")
	cookies := rec.Result().Cookies()
	if len(cookies) != 3 {
		t.Fatalf("cookies=%d, want 3", len(cookies))
	}
	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Name, "__Host-") || !cookie.Secure || cookie.Path != "/" ||
			cookie.Domain != "" || !cookie.HttpOnly {
			t.Fatalf("cookie violates the __Host- contract: %+v", cookie)
		}
	}
	if cookies[0].SameSite != http.SameSiteStrictMode || cookies[1].SameSite != http.SameSiteStrictMode {
		t.Fatalf("CSRF cookies must be SameSite=Strict: %+v %+v", cookies[0], cookies[1])
	}
	if cookies[1].Name != "__Host-tutor_csrf_verify_email" {
		t.Fatalf("account CSRF cookie name=%q", cookies[1].Name)
	}
	if accountCSRFCookieName("/verify-email") == accountCSRFCookieName("/recover") {
		t.Fatal("account flows share one CSRF cookie")
	}

	// A cookie under the bare name (as a non-HTTPS or sibling origin could
	// set it) is not read.
	req := httptest.NewRequest(http.MethodPost, "/authorize", nil)
	req.AddCookie(&http.Cookie{Name: "tutor_csrf_authorize", Value: "planted"})
	if _, ok := s.cookieValue(req, authorizeCSRFCookieName); ok {
		t.Fatal("unprefixed cookie accepted on an HTTPS origin")
	}
}

func TestBrowserCookiesOnLoopbackHTTPDropPrefixAndSecure(t *testing.T) {
	for _, base := range []string{"http://localhost:3000", "http://127.0.0.1:3000", "http://[::1]:3000"} {
		s := NewOAuthServer(nil, base, slog.Default())
		rec := httptest.NewRecorder()
		s.setAuthorizeCSRFCookie(rec, "token", 3600)
		cookie := rec.Result().Cookies()[0]
		if cookie.Name != "tutor_csrf_authorize" || cookie.Secure || cookie.Path != "/" || !cookie.HttpOnly {
			t.Fatalf("%s: loopback cookie=%+v", base, cookie)
		}
		req := httptest.NewRequest(http.MethodPost, "/authorize", nil)
		req.AddCookie(cookie)
		if value, ok := s.cookieValue(req, authorizeCSRFCookieName); !ok || value != "token" {
			t.Fatalf("%s: loopback cookie not read back", base)
		}
	}
	for _, base := range []string{"http://tutor.example", "http://10.0.0.5:3000", "https://localhost"} {
		if loopbackHTTPOrigin(base) {
			t.Fatalf("%s must keep the __Host- prefix", base)
		}
	}
}
