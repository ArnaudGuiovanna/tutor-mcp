// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tutor-mcp/auth"
	"tutor-mcp/models"
)

func scopedHTTPRequest(t *testing.T, body, grant string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	ctx := context.WithValue(req.Context(), auth.LearnerIDKey, "http-scope-learner")
	ctx = auth.WithOAuthScope(ctx, grant)
	return req.WithContext(ctx)
}

func TestOAuthScopeHTTPMiddleware_InsufficientScopeReturnsStepUpChallenge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tool   string
		grant  string
		modern bool
	}{
		{name: "read-only token calling mutation", tool: "record_interaction", grant: models.OAuthScopeLearnerRead},
		{name: "modern write-only token calling read with side effects", tool: "get_next_activity", grant: models.OAuthScopeLearnerWrite, modern: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
			handler := OAuthScopeHTTPMiddleware("https://test.example", 4096, true, next)
			meta := ""
			if tc.modern {
				meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},`
			}
			body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{` + meta + `"name":"` + tc.tool + `","arguments":{}}}`
			rec := httptest.NewRecorder()
			req := scopedHTTPRequest(t, body, tc.grant)
			if tc.modern {
				req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
				req.Header.Set("Mcp-Method", "tools/call")
				req.Header.Set("Mcp-Name", tc.tool)
			}
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403; body=%q", rec.Code, rec.Body.String())
			}
			if called {
				t.Fatal("unauthorized call reached the MCP transport")
			}
			challenge := rec.Header().Get("WWW-Authenticate")
			for _, want := range []string{
				`error="insufficient_scope"`,
				`scope="learner:read learner:write"`,
				`resource_metadata="https://test.example/.well-known/oauth-protected-resource"`,
			} {
				if !strings.Contains(challenge, want) {
					t.Fatalf("challenge %q missing %q", challenge, want)
				}
			}

			var response oauthScopeHTTPResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode MCP fallback: %v; body=%q", err, rec.Body.String())
			}
			if response.JSONRPC != "2.0" || string(response.ID) != "7" || response.Result == nil || !response.Result.IsError {
				t.Fatalf("unexpected MCP fallback: %+v", response)
			}
			if metaChallenge := oauthChallengeFromMeta(t, response.Result.Meta); metaChallenge != challenge {
				t.Fatalf("metadata challenge %q differs from HTTP challenge %q", metaChallenge, challenge)
			}
		})
	}
}

func TestOAuthScopeHTTPMiddleware_LegacyRolloutChallengesForBundle(t *testing.T) {
	called := false
	handler := OAuthScopeHTTPMiddleware("https://test.example", 4096, false, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	body := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"record_interaction","arguments":{}}}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, scopedHTTPRequest(t, body, models.OAuthScopeLearnerRead))
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v, want 403/false", rec.Code, called)
	}
	challenge := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(challenge, `scope="learner"`) || strings.Contains(challenge, `scope="learner:read`) {
		t.Fatalf("phase-A challenge must request the supported legacy bundle: %q", challenge)
	}
	if !strings.Contains(rec.Body.String(), "insufficient OAuth scope: learner") {
		t.Fatalf("phase-A MCP error is inconsistent with its challenge: %q", rec.Body.String())
	}
}

func TestOAuthScopeHTTPMiddleware_AllowsSufficientAndLegacyGrants(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tool  string
		grant string
	}{
		{name: "pure read", tool: "get_pending_alerts", grant: models.OAuthScopeLearnerRead},
		{name: "pure write", tool: "record_interaction", grant: models.OAuthScopeLearnerWrite},
		{name: "mixed granular", tool: "get_memory_state", grant: models.OAuthScopeLearnerReadWrite},
		{name: "bounded legacy bundle", tool: "get_metacognitive_mirror", grant: models.OAuthScopeLearner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotBody = string(body)
				w.WriteHeader(http.StatusNoContent)
			})
			handler := OAuthScopeHTTPMiddleware("https://test.example", 4096, true, next)
			body := `{"jsonrpc":"2.0","id":"ok","method":"tools/call","params":{"name":"` + tc.tool + `","arguments":{}}}`
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, scopedHTTPRequest(t, body, tc.grant))
			if rec.Code != http.StatusNoContent || gotBody != body {
				t.Fatalf("status=%d preserved body=%q, want 204/%q", rec.Code, gotBody, body)
			}
		})
	}
}

func TestOAuthScopeHTTPMiddleware_BoundsBodyAndDefersProtocolErrors(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "off")
	t.Run("oversized", func(t *testing.T) {
		called := false
		handler := OAuthScopeHTTPMiddleware("https://test.example", 32, true, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, scopedHTTPRequest(t, strings.Repeat("x", 33), models.OAuthScopeLearnerRead))
		if rec.Code != http.StatusRequestEntityTooLarge || called {
			t.Fatalf("status=%d called=%v, want 413/false", rec.Code, called)
		}
	})

	for _, body := range []string{
		`not-json`,
		`[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"record_interaction"}}]`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"1.0","id":1,"method":"tools/call","params":{"name":"record_interaction"}}`,
		`{"jsonrpc":"2.0","id":true,"method":"tools/call","params":{"name":"record_interaction"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool_that_does_not_exist"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_goal_relevance"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			called := false
			handler := OAuthScopeHTTPMiddleware("https://test.example", 4096, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				got, _ := io.ReadAll(r.Body)
				if string(got) != body {
					t.Fatalf("body changed: %q", got)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, scopedHTTPRequest(t, body, models.OAuthScopeLearnerRead))
			if rec.Code != http.StatusNoContent || !called {
				t.Fatalf("status=%d called=%v, want SDK delegation", rec.Code, called)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{name: "wrong content type", header: "Content-Type", value: "text/plain"},
		{name: "incomplete accept", header: "Accept", value: "application/json"},
		{name: "unsupported legacy protocol", header: "Mcp-Protocol-Version", value: "2025-01-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := OAuthScopeHTTPMiddleware("https://test.example", 4096, true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"record_interaction"}}`
			req := scopedHTTPRequest(t, body, models.OAuthScopeLearnerRead)
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent || !called {
				t.Fatalf("status=%d called=%v, want SDK delegation", rec.Code, called)
			}
		})
	}

	t.Run("modern routing headers must agree with body", func(t *testing.T) {
		called := false
		handler := OAuthScopeHTTPMiddleware("https://test.example", 4096, true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}))
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"name":"record_interaction"}}`
		req := scopedHTTPRequest(t, body, models.OAuthScopeLearnerRead)
		req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/list")
		req.Header.Set("Mcp-Name", "record_interaction")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent || !called {
			t.Fatalf("status=%d called=%v, want delegation on inconsistent routing headers", rec.Code, called)
		}
	})
}
