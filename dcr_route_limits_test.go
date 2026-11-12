// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"tutor-mcp/auth"
	"tutor-mcp/db"
)

func newDCRRouteTestServer(t *testing.T, mode string, token string) *auth.OAuthServer {
	t.Helper()
	database, err := db.OpenDB(filepath.Join(t.TempDir(), "tutor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	server := auth.NewOAuthServer(db.NewStore(database), "https://tutor.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	var hash [sha256.Size]byte
	if token != "" {
		hash = sha256.Sum256([]byte(token))
	}
	if err := server.ConfigureDynamicClientRegistration(mode, hash); err != nil {
		t.Fatal(err)
	}
	return server
}

func postRegister(mux *http.ServeMux, token, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:4000"
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestDCRAnonymousRequestsAreThrottledBeforeTokenLookup(t *testing.T) {
	server := newDCRRouteTestServer(t, auth.DCRModeToken, strings.Repeat("A", 43))
	admission := auth.NewRateLimiterWithNamespace("test_admission", 0.0001, 2)
	requests := auth.NewRateLimiterWithNamespace("test_requests", 100, 100)
	daily := auth.NewRateLimiterWithNamespace("test_daily", 100, 100)
	t.Cleanup(func() { admission.Stop(); requests.Stop(); daily.Stop() })
	mux := http.NewServeMux()
	mountDynamicClientRegistration(mux, auth.DCRModeToken, server, dcrRouteLimiters{Admission: admission, Requests: requests, Daily: daily})
	for i := 0; i < 2; i++ {
		if code := postRegister(mux, "", `{}`); code != http.StatusUnauthorized {
			t.Fatalf("anonymous request %d status=%d, want 401", i, code)
		}
	}
	if code := postRegister(mux, "", `{}`); code != http.StatusTooManyRequests {
		t.Fatalf("anonymous request over budget status=%d, want 429", code)
	}
}

func TestDCRDailyRegistrationBudgetPerIP(t *testing.T) {
	server := newDCRRouteTestServer(t, auth.DCRModeOpen, "")
	admission := auth.NewRateLimiterWithNamespace("test_admission", 100, 100)
	requests := auth.NewRateLimiterWithNamespace("test_requests", 100, 100)
	daily := auth.NewRateLimiterWithNamespace("test_daily", 2.0/(24*60*60), 2)
	t.Cleanup(func() { admission.Stop(); requests.Stop(); daily.Stop() })
	mux := http.NewServeMux()
	mountDynamicClientRegistration(mux, auth.DCRModeOpen, server, dcrRouteLimiters{Admission: admission, Requests: requests, Daily: daily})
	for i, name := range []string{"one", "two"} {
		body := `{"client_name":"` + name + `","redirect_uris":["https://` + name + `.example/cb"]}`
		if code := postRegister(mux, "", body); code != http.StatusCreated {
			t.Fatalf("registration %d status=%d, want 201", i, code)
		}
	}
	if code := postRegister(mux, "", `{"client_name":"three","redirect_uris":["https://three.example/cb"]}`); code != http.StatusTooManyRequests {
		t.Fatalf("registration over daily budget status=%d, want 429", code)
	}
}

func TestDCRDailyRegistrationBudgetDefault(t *testing.T) {
	t.Setenv("OAUTH_DCR_MAX_REGISTRATIONS_PER_IP_PER_DAY", "")
	if got := defaultDCRRegistrationsPerIPPerDay; got < 100 {
		t.Fatalf("default per-IP daily registrations=%d is not generous", got)
	}
	t.Setenv("OAUTH_DCR_MAX_REGISTRATIONS_PER_IP_PER_DAY", "0")
	if _, err := intEnvInRange("OAUTH_DCR_MAX_REGISTRATIONS_PER_IP_PER_DAY", defaultDCRRegistrationsPerIPPerDay, 1, 1_000_000); err == nil {
		t.Fatal("zero daily registration budget accepted")
	}
}
