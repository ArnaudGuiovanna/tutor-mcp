// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"tutor-mcp/models"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
)

type contextKey string

const LearnerIDKey contextKey = "learner_id"

func BearerMiddleware(baseURL string, next http.Handler) http.Handler {
	return BearerMiddlewareWithScopeHint(baseURL, models.OAuthScopeLearner, next)
}

// BearerMiddlewareWithScopeHint authenticates every MCP HTTP request while
// keeping scope authorization at the per-tool boundary. The hint is added only
// to 401 challenges; passing it through RequireBearerTokenOptions.Scopes would
// incorrectly impose one global AND requirement and reject valid write-only or
// step-up tokens before their target tool is known.
func BearerMiddlewareWithScopeHint(baseURL, initialScope string, next http.Handler) http.Handler {
	verifier := func(_ context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		claims, err := VerifyJWTClaims(token, baseURL)
		if err != nil {
			slog.Debug("jwt verify failed", "err", err)
			return nil, mcpauth.ErrInvalidToken
		}
		return &mcpauth.TokenInfo{
			UserID:     claims.Subject,
			Scopes:     strings.Fields(claims.Scope),
			Expiration: claims.ExpiresAt.Time,
		}, nil
	}

	// The MCP SDK stores TokenInfo under its own private context key. Stateful
	// transports use TokenInfo.UserID to bind all subsequent requests to the
	// principal that initialized the session and reject cross-user reuse.
	withLearnerContext := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenInfo := mcpauth.TokenInfoFromContext(r.Context())
		if tokenInfo == nil || tokenInfo.UserID == "" {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), LearnerIDKey, tokenInfo.UserID)
		ctx = WithOAuthScope(ctx, strings.Join(tokenInfo.Scopes, " "))
		next.ServeHTTP(w, r.WithContext(ctx))
	})

	requireBearer := mcpauth.RequireBearerToken(verifier, &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: baseURL + "/.well-known/oauth-protected-resource",
		// Granular scopes are alternatives enforced at each MCP tool boundary;
		// the generic bearer layer cannot express read OR write OR the bounded
		// legacy bundle as one RequireBearerToken scope set.
	})
	return withInitialScopeHint(initialScope, requireBearer(withLearnerContext))
}

type scopeHintResponseWriter struct {
	http.ResponseWriter
	scope       string
	wroteHeader bool
}

func (w *scopeHintResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *scopeHintResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if status == http.StatusUnauthorized && w.scope != "" {
		appendScopeToBearerChallenge(w.Header(), w.scope)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *scopeHintResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func withInitialScopeHint(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&scopeHintResponseWriter{ResponseWriter: w, scope: scope}, r)
	})
}

func appendScopeToBearerChallenge(header http.Header, scope string) {
	values := header.Values("WWW-Authenticate")
	for i, value := range values {
		trimmed := strings.TrimSpace(value)
		if !strings.HasPrefix(strings.ToLower(trimmed), "bearer") {
			continue
		}
		if strings.Contains(strings.ToLower(trimmed), "scope=") {
			return
		}
		values[i] = strings.TrimRight(value, " ") + ", scope=" + strconv.Quote(scope)
		header.Del("WWW-Authenticate")
		for _, updated := range values {
			header.Add("WWW-Authenticate", updated)
		}
		return
	}
	header.Add("WWW-Authenticate", "Bearer scope="+strconv.Quote(scope))
}

func GetLearnerID(ctx context.Context) string {
	id, _ := ctx.Value(LearnerIDKey).(string)
	return id
}
