// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"

	"github.com/golang-jwt/jwt/v5"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// helperOKHandler echoes the learner ID injected into context, so tests can
// assert that BearerMiddleware actually populated it.
func helperOKHandler(t *testing.T, wantLearnerID string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := GetLearnerID(r.Context())
		if wantLearnerID != "" && got != wantLearnerID {
			t.Errorf("learner_id in ctx = %q, want %q", got, wantLearnerID)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok:"+got)
	})
}

func TestBearerMiddleware_MissingAuthHeader(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	mw := BearerMiddleware("https://test.example", next)

	req := httptest.NewRequest("GET", "/mcp", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("next handler must not be invoked when auth header missing")
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, `resource_metadata="https://test.example/.well-known/oauth-protected-resource"`) {
		t.Fatalf("WWW-Authenticate missing resource_metadata: %q", wa)
	}
	if !strings.Contains(wa, `scope="learner"`) {
		t.Fatalf("WWW-Authenticate missing legacy bootstrap scope: %q", wa)
	}
	// No invalid_token marker on missing header (only on invalid).
	if strings.Contains(wa, `error="invalid_token"`) {
		t.Fatalf("missing token should NOT produce invalid_token marker: %q", wa)
	}
}

func TestBearerMiddlewareWithScopeHint_GranularBootstrapIsNotGlobalAuthorization(t *testing.T) {
	setTestSecret(t)
	const issuer = "https://test.example"

	t.Run("missing token challenges for read", func(t *testing.T) {
		rec := httptest.NewRecorder()
		BearerMiddlewareWithScopeHint(issuer, models.OAuthScopeLearnerRead, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("next called without bearer token")
		})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if challenge := rec.Header().Get("WWW-Authenticate"); !strings.Contains(challenge, `scope="learner:read"`) {
			t.Fatalf("granular bootstrap challenge = %q", challenge)
		}
	})

	t.Run("write-only token reaches per-tool boundary", func(t *testing.T) {
		token, err := GenerateJWTForResourceAndScope(
			issuer, MCPResource(issuer), "write-only-learner", models.OAuthScopeLearnerWrite,
		)
		if err != nil {
			t.Fatalf("generate token: %v", err)
		}
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if got := GetOAuthScope(r.Context()); got != models.OAuthScopeLearnerWrite {
				t.Fatalf("scope in context = %q, want %q", got, models.OAuthScopeLearnerWrite)
			}
			w.WriteHeader(http.StatusNoContent)
		})
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		BearerMiddlewareWithScopeHint(issuer, models.OAuthScopeLearnerRead, next).ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent || !called {
			t.Fatalf("write-only token status=%d called=%v, want 204/true; body=%q", rec.Code, called, rec.Body.String())
		}
	})
}

func TestBearerMiddleware_NonBearerScheme(t *testing.T) {
	for _, authHeader := range []string{
		"Basic dXNlcjpwYXNz",
		"Bearerx token",
	} {
		t.Run(authHeader, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
			mw := BearerMiddleware("https://test.example", next)

			req := httptest.NewRequest("GET", "/mcp", nil)
			req.Header.Set("Authorization", authHeader)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if called {
				t.Fatal("next must not be called for non-Bearer scheme")
			}
		})
	}
}

func TestBearerMiddleware_AcceptsBearerSchemeCaseInsensitive(t *testing.T) {
	setTestSecret(t)

	const learnerID = "learner-case"
	tok, err := GenerateJWT("https://test.example", learnerID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			mw := BearerMiddleware("https://test.example", helperOKHandler(t, learnerID))

			req := httptest.NewRequest("GET", "/mcp", nil)
			req.Header.Set("Authorization", scheme+" "+tok)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "ok:"+learnerID) {
				t.Fatalf("body = %q, expected learner id", rec.Body.String())
			}
		})
	}
}

func TestBearerMiddleware_InvalidToken(t *testing.T) {
	setTestSecret(t)

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	mw := BearerMiddleware("https://test.example", next)

	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-jwt")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("next must not be called when token invalid")
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(rec.Body.String(), "invalid token") {
		t.Fatalf("expected generic invalid-token response, got %q", rec.Body.String())
	}
	if !strings.Contains(wa, `resource_metadata="https://test.example/.well-known/oauth-protected-resource"`) {
		t.Fatalf("missing resource_metadata in WWW-Authenticate: %q", wa)
	}
}

func TestBearerMiddleware_WrongIssuerToken(t *testing.T) {
	setTestSecret(t)

	tok, err := GenerateJWT("https://other.example", "learner-2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	mw := BearerMiddleware("https://test.example", next)

	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("next must not be called when issuer mismatches")
	}
}

func TestBearerMiddleware_ValidTokenInjectsLearnerID(t *testing.T) {
	setTestSecret(t)

	const learnerID = "learner-xyz"
	tok, err := GenerateJWT("https://test.example", learnerID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	mw := BearerMiddleware("https://test.example", helperOKHandler(t, learnerID))

	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok:"+learnerID) {
		t.Fatalf("body = %q, expected to contain learner id", rec.Body.String())
	}
	// On success, no WWW-Authenticate is set.
	if wa := rec.Header().Get("WWW-Authenticate"); wa != "" {
		t.Fatalf("WWW-Authenticate must not be set on success: %q", wa)
	}
}

func TestBearerMiddleware_PopulatesMCPTokenInfo(t *testing.T) {
	setTestSecret(t)

	const learnerID = "learner-sdk-context"
	token, err := GenerateJWTForResourceAndScope(
		"https://test.example", MCPResource("https://test.example"), learnerID,
		models.OAuthScopeLearnerRead,
	)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := mcpauth.TokenInfoFromContext(r.Context())
		if info == nil {
			t.Fatal("official MCP TokenInfo missing from request context")
		}
		if info.UserID != learnerID {
			t.Errorf("TokenInfo.UserID = %q, want %q", info.UserID, learnerID)
		}
		if len(info.Scopes) != 1 || info.Scopes[0] != models.OAuthScopeLearnerRead {
			t.Errorf("TokenInfo.Scopes = %v, want [%s]", info.Scopes, models.OAuthScopeLearnerRead)
		}
		if info.Expiration.Before(time.Now()) || info.Expiration.After(time.Now().Add(AccessTokenTTL+time.Second)) {
			t.Errorf("TokenInfo.Expiration = %v, want the JWT expiry within %v", info.Expiration, AccessTokenTTL)
		}
		if got := GetLearnerID(r.Context()); got != learnerID {
			t.Errorf("GetLearnerID = %q, want %q", got, learnerID)
		}
		if got := GetOAuthScope(r.Context()); got != models.OAuthScopeLearnerRead {
			t.Errorf("GetOAuthScope = %q, want %q", got, models.OAuthScopeLearnerRead)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	BearerMiddleware("https://test.example", next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%q", rec.Code, rec.Body.String())
	}
}

func TestBearerMiddleware_RejectsTokenWithoutLearnerScope(t *testing.T) {
	setTestSecret(t)
	const issuer = "https://test.example"

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "learner-wrong-scope",
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{MCPResource(issuer)},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "profile:read",
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	BearerMiddleware(issuer, next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("next must not be called without the learner scope")
	}
	if !strings.Contains(rec.Body.String(), "invalid token") {
		t.Fatalf("unexpected response body %q", rec.Body.String())
	}
}

func TestBearerMiddleware_RejectsCrossUserMCPSessionReuse(t *testing.T) {
	setTestSecret(t)
	const issuer = "https://test.example"

	server := mcp.NewServer(&mcp.Implementation{Name: "auth-session-test", Version: "1.0"}, nil)
	streamHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		DisableLocalhostProtection: true,
		JSONResponse:               true,
	})
	httpServer := httptest.NewServer(BearerMiddleware(issuer, streamHandler))
	t.Cleanup(httpServer.Close)

	tokenA, err := GenerateJWT(issuer, "learner-A")
	if err != nil {
		t.Fatalf("generate token A: %v", err)
	}
	tokenB, err := GenerateJWT(issuer, "learner-B")
	if err != nil {
		t.Fatalf("generate token B: %v", err)
	}

	type response struct {
		status int
		header http.Header
		body   string
	}
	doRequest := func(method, token, sessionID, payload string) response {
		t.Helper()
		var body io.Reader
		if payload != "" {
			body = strings.NewReader(payload)
		}
		req, err := http.NewRequest(method, httpServer.URL+"/mcp", body)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json, text/event-stream")
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
			req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		return response{status: resp.StatusCode, header: resp.Header.Clone(), body: string(data)}
	}

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	initResp := doRequest(http.MethodPost, tokenA, "", initialize)
	if initResp.status != http.StatusOK {
		t.Fatalf("initialize as A: status=%d body=%q", initResp.status, initResp.body)
	}
	sessionID := initResp.header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize response did not include Mcp-Session-Id")
	}

	ping := `{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`
	hijackResp := doRequest(http.MethodPost, tokenB, sessionID, ping)
	if hijackResp.status != http.StatusForbidden {
		t.Fatalf("session reuse as B: status=%d, want 403; body=%q", hijackResp.status, hijackResp.body)
	}
	if !strings.Contains(hijackResp.body, "session user mismatch") {
		t.Fatalf("session reuse as B returned unexpected body %q", hijackResp.body)
	}

	ownerResp := doRequest(http.MethodPost, tokenA, sessionID, ping)
	if ownerResp.status != http.StatusOK {
		t.Fatalf("session reuse as original A: status=%d, want 200; body=%q", ownerResp.status, ownerResp.body)
	}

	deleteResp := doRequest(http.MethodDelete, tokenA, sessionID, "")
	if deleteResp.status != http.StatusNoContent {
		t.Fatalf("delete session: status=%d, want 204; body=%q", deleteResp.status, deleteResp.body)
	}
}

func TestBearerMiddleware_EmptyBearerToken(t *testing.T) {
	setTestSecret(t)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	mw := BearerMiddleware("https://test.example", next)

	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer ") // technically prefix matches, but token is ""
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("next must not be called for empty bearer token")
	}
}

func TestGetLearnerID_NoValueReturnsEmpty(t *testing.T) {
	if got := GetLearnerID(context.Background()); got != "" {
		t.Fatalf("GetLearnerID with empty ctx = %q, want empty", got)
	}
}

func TestGetLearnerID_WrongTypeReturnsEmpty(t *testing.T) {
	// Storing a non-string under LearnerIDKey should not panic and should return "".
	ctx := context.WithValue(context.Background(), LearnerIDKey, 12345)
	if got := GetLearnerID(ctx); got != "" {
		t.Fatalf("GetLearnerID with non-string value = %q, want empty", got)
	}
}

func TestGetLearnerID_StringValueReturned(t *testing.T) {
	ctx := context.WithValue(context.Background(), LearnerIDKey, "abc-123")
	if got := GetLearnerID(ctx); got != "abc-123" {
		t.Fatalf("GetLearnerID = %q, want abc-123", got)
	}
}
