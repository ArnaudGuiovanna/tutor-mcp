// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSExposesOAuthStepUpChallengeToBrowserClients(t *testing.T) {
	handler := corsMiddleware([]string{"https://chat.example"}, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="learner:write"`)
		w.WriteHeader(http.StatusForbidden)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://chat.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	exposed := strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers"))
	for _, name := range []string{"mcp-session-id", "www-authenticate"} {
		if !strings.Contains(exposed, name) {
			t.Fatalf("Access-Control-Expose-Headers=%q missing %q", exposed, name)
		}
	}
}
