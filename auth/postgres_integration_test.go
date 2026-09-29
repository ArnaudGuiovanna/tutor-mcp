// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"

	"golang.org/x/crypto/bcrypt"
)

// TestPostgresOAuthAuthorizationCodeExchange exercises the HTTP OAuth layer,
// not merely its Store calls, against the PostgreSQL dialect used by the
// production profile. The ordinary auth suite deliberately remains fast on
// SQLite; this opt-in gate runs in the PostgreSQL CI job.
func TestPostgresOAuthAuthorizationCodeExchange(t *testing.T) {
	baseDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	if baseDSN == "" {
		t.Skip("set TUTOR_TEST_PG_DSN to run the PostgreSQL auth gate")
	}
	raw, store := postgresAuthTestStore(t, baseDSN)
	_ = raw
	setTestSecret(t)
	server := NewOAuthServer(store, "https://test.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetEmailSender(&testEmailSender{})

	seedClient(t, store, "pg-auth-client", "https://client.example/callback")
	learnerID := seedLearner(t, store, "pg-auth@example.com", "strong-password")
	verifier := "postgres-pkce-verifier-long-enough"
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if err := store.CreateAuthCodeWithBinding(
		context.Background(), "pg-auth-code", learnerID, challenge, "S256",
		"pg-auth-client", "https://client.example/callback", testOAuthResource,
		time.Now().UTC().Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"resource":      {testOAuthResource},
		"code":          {"pg-auth-code"},
		"code_verifier": {verifier},
		"client_id":     {"pg-auth-client"},
		"redirect_uri":  {"https://client.example/callback"},
	}
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.HandleToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PostgreSQL token exchange status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AccessToken == "" || response.RefreshToken == "" {
		t.Fatalf("missing PostgreSQL-issued credentials: %+v", response)
	}
	claims, err := VerifyJWTClaims(response.AccessToken, "https://test.example")
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != learnerID || claims.Audience[0] != testOAuthResource {
		t.Fatalf("unexpected PostgreSQL token binding: %+v", claims)
	}
}

func postgresAuthTestStore(t *testing.T, baseDSN string) (*sql.DB, *db.Store) {
	t.Helper()
	return postgresAuthTestStoreInSchema(t, baseDSN, "p1_auth_http")
}

func postgresAuthTestStoreInSchema(t *testing.T, baseDSN, schema string) (*sql.DB, *db.Store) {
	t.Helper()
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(baseDSN, "?") {
		separator = "&"
	}
	raw, err := db.OpenPostgres(baseDSN+separator+"search_path="+schema, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MigratePostgres(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = raw.Close()
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		_ = admin.Close()
	})
	return raw, db.NewStoreWithDialect(raw, db.DialectPostgres)
}

// TestPostgresOAuthInstitutionMemberExchangeAndRefresh drives the real
// /authorize, /token and refresh handlers for a member of a provisioned tenant
// on PostgreSQL, where forced row-level security scopes every lookup to the
// tenant bound to the credential.
func TestPostgresOAuthInstitutionMemberExchangeAndRefresh(t *testing.T) {
	baseDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	if baseDSN == "" {
		t.Skip("set TUTOR_TEST_PG_DSN to run the PostgreSQL auth gate")
	}
	raw, store := postgresAuthTestStoreInSchema(t, baseDSN, "p1_auth_institution")
	setTestSecret(t)
	server := NewOAuthServer(store, "https://test.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetEmailSender(&testEmailSender{})

	const (
		tenantID    = "tenant_pg_acme"
		clientID    = "pg-institution-client"
		redirectURI = "https://client.example/callback"
		email       = "alice@pg-acme.test"
		password    = "strong-institution-password"
		verifier    = "postgres-institution-pkce-verifier"
	)
	seedClient(t, store, clientID, redirectURI)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := raw.Exec(`INSERT INTO tenants (id, slug, name, status, region, policy_json, created_at, updated_at)
		VALUES ($1, 'pg-acme', 'PG Acme', 'active', 'default', '{}', $2, $2)`, tenantID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO learners
		(id, email, password_hash, objective, profile_json, created_at, email_verified_at, tenant_id, user_id, membership_id)
		VALUES ('lrn_pg_alice', $1, $2, '', '{}', $3, $3, $4, 'usr_pg_alice', 'mem_pg_alice')`,
		email, string(hash), now, tenantID); err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256([]byte(verifier))
	code := driveAuthorizePost(t, server, clientID, redirectURI, base64.RawURLEncoding.EncodeToString(digest[:]), "S256", email, password)
	status, body := exchangeToken(t, server, url.Values{
		"grant_type": {"authorization_code"}, "resource": {testOAuthResource}, "code": {code},
		"code_verifier": {verifier}, "client_id": {clientID}, "redirect_uri": {redirectURI},
	})
	if status != http.StatusOK {
		t.Fatalf("PostgreSQL institution token exchange = %d %v", status, body)
	}
	claims, err := VerifyJWTClaims(body["access_token"].(string), "https://test.example")
	if err != nil {
		t.Fatal(err)
	}
	if principal, _ := claims.Principal(); principal.TenantID != tenantID || principal.MembershipID != "mem_pg_alice" {
		t.Fatalf("PostgreSQL token bound to the wrong membership: %+v", principal)
	}
	status, refreshed := exchangeToken(t, server, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {body["refresh_token"].(string)},
		"client_id": {clientID}, "resource": {testOAuthResource},
	})
	if status != http.StatusOK {
		t.Fatalf("PostgreSQL institution refresh = %d %v", status, refreshed)
	}
}
