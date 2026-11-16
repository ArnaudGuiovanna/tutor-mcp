// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"tutor-mcp/models"
)

// Model a stored claim that reaches signing validation, whose error includes
// the invalid value. Neither grant may copy that value into the log.
type invalidSigningPrincipalStore struct {
	oauthStore
	role string
}

func (s invalidSigningPrincipalStore) GetPrincipal(ctx context.Context, scope models.TenantScope, scopes []string) (models.Principal, error) {
	p, err := s.oauthStore.GetPrincipal(ctx, scope, scopes)
	p.Roles = []string{s.role}
	return p, err
}

func (s invalidSigningPrincipalStore) GetPrincipalForLearner(ctx context.Context, learner string, scopes []string) (models.Principal, error) {
	p, err := s.oauthStore.GetPrincipalForLearner(ctx, learner, scopes)
	p.Roles = []string{s.role}
	return p, err
}

func TestTokenSigningFailuresDoNotLogClaims(t *testing.T) {
	for _, grant := range []string{"authorization_code", "refresh_token"} {
		t.Run(grant, func(t *testing.T) {
			setTestSecret(t)
			s, store := newTestServer(t)
			seedClient(t, store, "signing-client", "https://good.example/cb")
			learner := seedLearner(t, store, "signing-log@example.com", "password")
			const marker = "private-claim-forged-entry"
			s.store = invalidSigningPrincipalStore{store, "invalid\r\n" + marker}
			var logs bytes.Buffer
			s.logger = slog.New(slog.NewTextHandler(&logs, nil))
			form := url.Values{
				"grant_type": {grant}, "client_id": {"signing-client"},
				"resource": {testOAuthResource},
			}
			if grant == "authorization_code" {
				const verifier = "signing-log-code-verifier"
				hash := sha256.Sum256([]byte(verifier))
				challenge := base64.RawURLEncoding.EncodeToString(hash[:])
				if err := store.CreateAuthCodeWithBinding(t.Context(), "signing-code", learner, challenge, "", "signing-client", "", testOAuthResource, futureTime()); err != nil {
					t.Fatal(err)
				}
				form.Set("code", "signing-code")
				form.Set("code_verifier", verifier)
			} else {
				rt, err := store.CreateRefreshToken(t.Context(), learner, "signing-client", testOAuthResource)
				if err != nil {
					t.Fatal(err)
				}
				form.Set("refresh_token", rt.Token)
			}
			req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			s.HandleToken(rec, req)
			if rec.Code != http.StatusInternalServerError || !strings.Contains(logs.String(), "generate jwt failed") {
				t.Fatalf("signing failure not exercised: status=%d logs=%q", rec.Code, logs.String())
			}
			if strings.Contains(logs.String(), marker) || strings.Contains(rec.Body.String(), marker) {
				t.Fatalf("claim disclosed: logs=%q response=%q", logs.String(), rec.Body.String())
			}
		})
	}
}
