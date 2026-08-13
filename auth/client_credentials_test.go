// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

func createOAuthServiceAccount(t *testing.T, store *db.Store) (*models.ServiceAccount, string) {
	t.Helper()
	ctx := context.Background()
	learnerID := seedLearner(t, store, "service-owner@example.com", "strong-password")
	learner, err := store.GetPrincipalForLearner(ctx, learnerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMembershipAuthorization(ctx, learner.TenantScope(),
		models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordMembershipMFAVerification(ctx, learner.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	owner, err := store.GetPrincipalForLearner(ctx, learnerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	account, rawToken, err := store.CreateServiceAccount(ctx, owner, "oauth reporting bot",
		[]string{models.RoleAuditor}, models.OAuthScopeLearnerRead, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return account, rawToken
}

func clientCredentialsRequest(t *testing.T, server *OAuthServer, form url.Values, basicID, basicSecret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicID != "" || basicSecret != "" {
		req.SetBasicAuth(basicID, basicSecret)
	}
	rec := httptest.NewRecorder()
	server.HandleToken(rec, req)
	return rec
}

func TestClientCredentialsGrantIssuesTenantBoundAccessTokenOnly(t *testing.T) {
	setTestSecret(t)
	server, store := newTestServer(t)
	account, rawToken := createOAuthServiceAccount(t, store)
	form := url.Values{
		"grant_type": {"client_credentials"},
		"resource":   {testOAuthResource},
		"scope":      {models.OAuthScopeLearnerRead},
	}
	rec := clientCredentialsRequest(t, server, form, account.ClientID, rawToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["access_token"] == "" || response["scope"] != models.OAuthScopeLearnerRead {
		t.Fatalf("token response = %#v", response)
	}
	if _, present := response["refresh_token"]; present {
		t.Fatalf("client credentials response contains refresh token: %#v", response)
	}
	claims, err := VerifyJWTClaims(response["access_token"].(string), "https://test.example")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := claims.Principal()
	if err != nil {
		t.Fatal(err)
	}
	if claims.AuthorizedParty != account.ClientID || principal.UserID != account.ID ||
		principal.TenantID != account.TenantID || principal.LearnerID != "" ||
		principal.MembershipID != "service_account_"+account.ID {
		t.Fatalf("service token claims = %#v principal=%#v", claims, principal)
	}
	if err := store.ValidatePrincipal(context.Background(), principal); err != nil {
		t.Fatalf("validate service access token principal: %v", err)
	}
}

func TestClientCredentialsGrantRejectsCredentialTargetScopeAndRevocation(t *testing.T) {
	setTestSecret(t)
	server, store := newTestServer(t)
	account, rawToken := createOAuthServiceAccount(t, store)
	base := url.Values{
		"grant_type":    {"client_credentials"},
		"resource":      {testOAuthResource},
		"client_id":     {account.ClientID},
		"client_secret": {rawToken},
	}
	for _, test := range []struct {
		name       string
		mutate     func(url.Values)
		wantStatus int
		wantError  string
	}{
		{"wrong secret", func(v url.Values) { v.Set("client_secret", rawToken+"x") }, http.StatusUnauthorized, "invalid_client"},
		{"wrong client", func(v url.Values) { v.Set("client_id", "another-client") }, http.StatusUnauthorized, "invalid_client"},
		{"wrong resource", func(v url.Values) { v.Set("resource", "https://other.example/mcp") }, http.StatusBadRequest, "invalid_target"},
		{"scope escalation", func(v url.Values) { v.Set("scope", models.OAuthScopeLearnerReadWrite) }, http.StatusBadRequest, "invalid_scope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			form := cloneValues(base)
			test.mutate(form)
			rec := clientCredentialsRequest(t, server, form, "", "")
			if rec.Code != test.wantStatus || !strings.Contains(rec.Body.String(), test.wantError) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	ownerID := seedLearner(t, store, "second-owner@example.com", "strong-password")
	owner, err := store.GetPrincipalForLearner(context.Background(), ownerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	// The account creator is not needed for revocation: a currently authorized
	// tenant owner may revoke any service credential in that tenant.
	if _, err := store.SetMembershipAuthorization(context.Background(), owner.TenantScope(),
		models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordMembershipMFAVerification(context.Background(), owner.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	owner, err = store.GetPrincipalForLearner(context.Background(), ownerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeServiceAccount(context.Background(), owner, account.ID); err != nil {
		t.Fatal(err)
	}
	rec := clientCredentialsRequest(t, server, base, "", "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("revoked status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthorizationServerAdvertisesClientCredentials(t *testing.T) {
	server, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	server.HandleAuthServerMetadata(rec,
		httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	var metadata struct {
		GrantTypes []string `json:"grant_types_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	for _, grant := range metadata.GrantTypes {
		if grant == "client_credentials" {
			return
		}
	}
	t.Fatalf("client_credentials not advertised: %v", metadata.GrantTypes)
}

func cloneValues(source url.Values) url.Values {
	cloned := make(url.Values, len(source))
	for key, values := range source {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}
