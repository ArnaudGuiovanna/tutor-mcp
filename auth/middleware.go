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

// PrincipalValidator checks current tenant, user, membership status, roles and
// token version. Production wires the durable identity store here so a
// suspension or role change revokes an already-issued bearer immediately.
type PrincipalValidator interface {
	ValidatePrincipal(context.Context, models.Principal) error
}

const LearnerIDKey contextKey = "learner_id"

const principalContextKey contextKey = "principal"
const principalTokenInfoKey = "tutor.principal"

func BearerMiddleware(baseURL string, next http.Handler) http.Handler {
	return bearerMiddleware(baseURL, models.OAuthScopeLearner, nil, next)
}

// BearerMiddlewareWithScopeHint authenticates every MCP HTTP request while
// keeping scope authorization at the per-tool boundary. The hint is added only
// to 401 challenges; passing it through RequireBearerTokenOptions.Scopes would
// incorrectly impose one global AND requirement and reject valid write-only or
// step-up tokens before their target tool is known.
func BearerMiddlewareWithScopeHint(baseURL, initialScope string, next http.Handler) http.Handler {
	return bearerMiddleware(baseURL, initialScope, nil, next)
}

func BearerMiddlewareWithPrincipalValidator(baseURL, initialScope string, validator PrincipalValidator, next http.Handler) http.Handler {
	if validator == nil {
		panic("auth: nil principal validator")
	}
	return bearerMiddleware(baseURL, initialScope, validator, next)
}

func bearerMiddleware(baseURL, initialScope string, validator PrincipalValidator, next http.Handler) http.Handler {
	verifier := func(ctx context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		claims, err := VerifyJWTClaims(token, baseURL)
		if err != nil {
			slog.Debug("jwt verify failed", "err", err)
			return nil, mcpauth.ErrInvalidToken
		}
		principal, err := claims.Principal()
		if err != nil {
			slog.Debug("jwt principal invalid", "err", err)
			return nil, mcpauth.ErrInvalidToken
		}
		if validator != nil {
			if err := validator.ValidatePrincipal(ctx, principal); err != nil {
				slog.Debug("jwt principal no longer active", "err", err)
				return nil, mcpauth.ErrInvalidToken
			}
		}
		return &mcpauth.TokenInfo{
			UserID:     principal.SessionBindingID(),
			Scopes:     strings.Fields(claims.Scope),
			Expiration: claims.ExpiresAt.Time,
			Extra:      map[string]any{principalTokenInfoKey: principal},
		}, nil
	}

	// The MCP SDK stores TokenInfo under its own private context key. Stateful
	// transports use TokenInfo.UserID to bind all subsequent requests to the
	// principal that initialized the session and reject cross-user reuse.
	withLearnerContext := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenInfo := mcpauth.TokenInfoFromContext(r.Context())
		if tokenInfo == nil || tokenInfo.UserID == "" || tokenInfo.Extra == nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		principal, ok := tokenInfo.Extra[principalTokenInfoKey].(models.Principal)
		if !ok || principal.SessionBindingID() != tokenInfo.UserID {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		ctx, err := WithPrincipal(r.Context(), principal)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
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
	if principal, ok := GetPrincipal(ctx); ok {
		return principal.LearnerID
	}
	id, _ := ctx.Value(LearnerIDKey).(string)
	return id
}

// WithPrincipal admits only a fully validated, single-tenant identity into a
// business context.
func WithPrincipal(ctx context.Context, principal models.Principal) (context.Context, error) {
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, principalContextKey, principal), nil
}

func GetPrincipal(ctx context.Context) (models.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey).(models.Principal)
	if !ok || principal.Validate() != nil {
		return models.Principal{}, false
	}
	return principal, true
}
