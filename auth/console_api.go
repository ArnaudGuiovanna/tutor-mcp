// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"tutor-mcp/models"
)

// ConsoleAPIHandler admits only a fresh, MFA-verified browser session. OAuth
// Authorization headers confer no authority here. The cookie stays /console.
func (s *OAuthServer) ConsoleAPIHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.requireConsole(w, r)
		if !ok {
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			cookie, err := r.Cookie(accountCSRFCookieName)
			token := r.Header.Get("X-CSRF-Token")
			if err != nil || token == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 || !s.consumeCSRF(r.Context(), token) {
				http.Error(w, "forbidden: csrf check failed", http.StatusForbidden)
				return
			}
		}
		ctx, err := WithPrincipal(r.Context(), c.principal)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Internal compatibility capabilities for existing handlers. No OAuth
		// token is issued; this context cannot escape the console boundary.
		ctx = WithOAuthScope(ctx, models.OAuthScopeLearner)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HandleConsoleAPICSRF provides a single-use token for administrative JSON
// clients using the same browser session as the rendered console.
func (s *OAuthServer) HandleConsoleAPICSRF(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireConsole(w, r); !ok {
		return
	}
	token, ok := consoleCSRF(w, consoleCookiePath)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": token})
}
