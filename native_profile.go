// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"strings"

	"tutor-mcp/auth"
	"tutor-mcp/models"
)

// mountNativeIdentity exposes only proof-of-key ceremonies. These handlers
// enforce their own signed challenge, action and one-use nonce; they do not
// accept a password, mailbox proof or unsigned role assignment.
func mountNativeIdentity(mux *http.ServeMux, native *auth.NativeIdentityServer, discovery, ceremony *auth.RateLimiter) {
	mux.Handle("GET /.well-known/tutor-institution", auth.RateLimitMiddleware(discovery, http.HandlerFunc(native.HandleDiscovery)))
	for path, handler := range map[string]http.HandlerFunc{
		"challenge": native.HandleChallenge,
		"session":   native.HandleSession,
		"rotate":    native.HandleRotate,
		"recover":   native.HandleRecover,
		"logout":    native.HandleLogout,
	} {
		mux.Handle("POST /api/client/v1/identity/"+path, auth.RateLimitMiddleware(ceremony, handler))
	}
}

// A role and a delegated capability are separate boundaries. In particular,
// authorizing Codex to tutor must never also authorize catalogue mutations.
func nativeAdministrativeScope(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if prefix != "/admin/catalog/" {
			// Console review mutations use a separate ceremony and are not part
			// of the native formation-authoring grant. Keep them closed rather
			// than replacing their cookie/MFA checks with a broad bearer grant.
			http.Error(w, "native review delegation is not available", http.StatusForbidden)
			return
		}
		scope := models.OAuthScopeFormationRead
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.HasSuffix(r.URL.Path, "/report") {
			scope = models.OAuthScopeProgressRead
		} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
			scope = models.OAuthScopeFormationWrite
		}
		if !auth.HasOAuthScope(r.Context(), scope) {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+scope+`"`)
			http.Error(w, "formation delegation is required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
