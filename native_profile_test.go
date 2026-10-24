// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"tutor-mcp/auth"
	"tutor-mcp/models"
)

func TestNativeProfileHasNoMailboxOrPasswordEntrypoints(t *testing.T) {
	mux := http.NewServeMux()
	limiter := auth.NewRateLimiterWithNamespace("native_profile_test", 1000, 2000)
	defer limiter.Stop()
	mountNativeIdentity(mux, nil, limiter, limiter)
	for _, path := range []string{"/authorize", "/signup", "/invite", "/console/login", "/learn/login", "/verify-email", "/recover", "/reset-password", "/login-challenge", "/account/invite"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("obsolete account route remains reachable: %s %s (%d)", method, path, recorder.Code)
			}
		}
	}
}

func TestNativeCatalogueGrantDoesNotFollowFromRoleOrLearningGrant(t *testing.T) {
	for _, tc := range []struct {
		method, scope string
		allowed       bool
	}{
		{http.MethodGet, models.OAuthScopeLearner, false},
		{http.MethodPost, models.OAuthScopeLearner, false},
		{http.MethodGet, models.OAuthScopeFormationRead, true},
		{http.MethodPost, models.OAuthScopeFormationRead, false},
		{http.MethodPost, models.OAuthScopeFormationWrite, true},
	} {
		called := false
		handler := nativeAdministrativeScope("/admin/catalog/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}))
		request := httptest.NewRequest(tc.method, "/admin/catalog/formations", nil)
		request = request.WithContext(auth.WithOAuthScope(request.Context(), tc.scope))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if called != tc.allowed {
			t.Fatalf("scope=%q method=%s allowed=%v, wanted %v", tc.scope, tc.method, called, tc.allowed)
		}
	}
}
