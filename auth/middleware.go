// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
)

type contextKey string

const LearnerIDKey contextKey = "learner_id"

func BearerMiddleware(baseURL string, next http.Handler) http.Handler {
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
		next.ServeHTTP(w, r.WithContext(ctx))
	})

	requireBearer := mcpauth.RequireBearerToken(verifier, &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: baseURL + "/.well-known/oauth-protected-resource",
		Scopes:              []string{"learner"},
	})
	return requireBearer(withLearnerContext)
}

func GetLearnerID(ctx context.Context) string {
	id, _ := ctx.Value(LearnerIDKey).(string)
	return id
}
