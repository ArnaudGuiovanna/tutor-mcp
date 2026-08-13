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
	const schema = "p1_auth_http"
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
