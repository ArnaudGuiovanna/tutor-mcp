// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	storeport "tutor-mcp/store"
)

type trackingRequestBody struct {
	reader io.Reader
	read   atomic.Bool
}

func (b *trackingRequestBody) Read(p []byte) (int, error) {
	b.read.Store(true)
	return b.reader.Read(p)
}
func (*trackingRequestBody) Close() error { return nil }

func configureTokenDCR(t *testing.T, server *OAuthServer, rawToken string) {
	t.Helper()
	if err := server.ConfigureDynamicClientRegistration(DCRModeToken, sha256.Sum256([]byte(rawToken))); err != nil {
		t.Fatal(err)
	}
}

func TestDCRDisabledIsNotDiscoveredAndCannotCreate(t *testing.T) {
	server, store := newTestServer(t)
	if err := server.ConfigureDynamicClientRegistration(DCRModeDisabled, [sha256.Size]byte{}); err != nil {
		t.Fatal(err)
	}
	metadata := httptest.NewRecorder()
	server.HandleAuthServerMetadata(metadata, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	var document map[string]any
	if err := json.Unmarshal(metadata.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if _, advertised := document["registration_endpoint"]; advertised {
		t.Fatal("disabled DCR was advertised in authorization-server metadata")
	}

	body := &trackingRequestBody{reader: strings.NewReader(`{"redirect_uris":["https://client.example/cb"]}`)}
	req := httptest.NewRequest(http.MethodPost, "/register", body)
	rec := httptest.NewRecorder()
	server.HandleRegister(rec, req)
	if rec.Code != http.StatusNotFound || body.read.Load() {
		t.Fatalf("disabled response status=%d body_read=%v", rec.Code, body.read.Load())
	}
	if count, err := store.CountOAuthClients(context.Background()); err != nil || count != 0 {
		t.Fatalf("disabled DCR mutated clients: count=%d err=%v", count, err)
	}
}

func TestDCRTokenAdmissionIsUniformAndPrecedesAllMutation(t *testing.T) {
	server, store := newTestServer(t)
	const rawToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	configureTokenDCR(t, server, rawToken)
	if err := store.CreateOAuthClientWithSecretCappedTTL(
		context.Background(), "expired", "Expired", `["https://client.example/cb"]`, "", 10,
		time.Now().UTC().Add(-time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		headers []string
	}{
		{name: "missing"},
		{name: "wrong", headers: []string{"Bearer wrong"}},
		{name: "wrong scheme", headers: []string{"Basic " + rawToken}},
		{name: "duplicate", headers: []string{"Bearer " + rawToken, "Bearer " + rawToken}},
		{name: "comma joined", headers: []string{"Bearer " + rawToken + ", Bearer " + rawToken}},
	}
	var baselineBody, baselineChallenge string
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackingRequestBody{reader: strings.NewReader(
				`{"redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
			)}
			req := httptest.NewRequest(http.MethodPost, "/register", body)
			for _, header := range tc.headers {
				req.Header.Add("Authorization", header)
			}
			rec := httptest.NewRecorder()
			server.HandleRegister(rec, req)
			if rec.Code != http.StatusUnauthorized || body.read.Load() {
				t.Fatalf("status=%d body_read=%v body=%q", rec.Code, body.read.Load(), rec.Body.String())
			}
			challenge := rec.Header().Get("WWW-Authenticate")
			if !strings.Contains(challenge, `error="invalid_token"`) || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("challenge=%q cache=%q", challenge, rec.Header().Get("Cache-Control"))
			}
			if i == 0 {
				baselineBody, baselineChallenge = rec.Body.String(), challenge
			} else if rec.Body.String() != baselineBody || challenge != baselineChallenge {
				t.Fatalf("enumerating admission response: body=%q challenge=%q", rec.Body.String(), challenge)
			}
		})
	}
	if count, err := store.CountOAuthClients(context.Background()); err != nil || count != 1 {
		t.Fatalf("invalid IAT performed cleanup/creation: count=%d err=%v", count, err)
	}
}

func TestDCRTokenAdmissionWrapsRateLimitAndValidTokenRegisters(t *testing.T) {
	server, store := newTestServer(t)
	const rawToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	configureTokenDCR(t, server, rawToken)

	var passed atomic.Int32
	backend := &recordingRateLimitBackend{}
	limiter := NewRateLimiterWithNamespace("dcr_policy_test", 100, 10)
	limiter.SetBackend(backend)
	defer limiter.Stop()
	wrapped := server.DynamicClientRegistrationAdmission(RateLimitMiddleware(limiter,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			passed.Add(1)
			server.HandleRegister(w, r)
		})))
	request := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(
			`{"client_name":"Authorized","redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
		))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		return rec
	}

	invalid := request("wrong")
	if invalid.Code != http.StatusUnauthorized || passed.Load() != 0 || len(backend.keys) != 0 {
		t.Fatalf("invalid IAT reached inner/rate-limit handler: status=%d passed=%d backend_writes=%d", invalid.Code, passed.Load(), len(backend.keys))
	}
	valid := request(rawToken)
	if valid.Code != http.StatusCreated || passed.Load() != 1 || len(backend.keys) != 1 {
		t.Fatalf("valid IAT status=%d passed=%d backend_writes=%d body=%q", valid.Code, passed.Load(), len(backend.keys), valid.Body.String())
	}
	if count, err := store.CountOAuthClients(context.Background()); err != nil || count != 1 {
		t.Fatalf("authorized registration count=%d err=%v", count, err)
	}
}

func TestHandleRegisterLeavesExpiredClientCleanupToScheduler(t *testing.T) {
	server, store := newTestServer(t)
	if err := store.CreateOAuthClientWithSecretCappedTTL(
		context.Background(), "expired", "Expired", `["https://old.example/cb"]`, "", 10,
		time.Now().UTC().Add(-time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(
		`{"client_name":"Fresh","redirect_uris":["https://client.example/cb"]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.HandleRegister(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("registration status=%d body=%q", rec.Code, rec.Body.String())
	}
	if count, err := store.CountOAuthClients(context.Background()); err != nil || count != 2 {
		t.Fatalf("request-path cleanup ran: count=%d err=%v", count, err)
	}
}

func TestConfigureDCRPolicyRejectsInconsistentInputs(t *testing.T) {
	server, _ := newTestServer(t)
	hash := sha256.Sum256([]byte("token"))
	for _, tc := range []struct {
		mode string
		hash [sha256.Size]byte
	}{
		{mode: "unknown"},
		{mode: DCRModeToken},
		{mode: DCRModeOpen, hash: hash},
		{mode: DCRModeDisabled, hash: hash},
	} {
		if err := server.ConfigureDynamicClientRegistration(tc.mode, tc.hash); err == nil {
			t.Fatalf("mode=%q hash_present=%v unexpectedly accepted", tc.mode, tc.hash != ([sha256.Size]byte{}))
		}
	}
}

func dcrRegistrationRequest(server *OAuthServer, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.HandleRegister(rec, req)
	return rec
}

func TestDCREquivalentPublicRegistrationIsConcurrentAndQuotaIdempotent(t *testing.T) {
	server, store := newTestServer(t)
	const rawToken = "ccccccccccccccccccccccccccccccccccccccccccc"
	configureTokenDCR(t, server, rawToken)
	if _, err := store.RawDB().Exec(`UPDATE oauth_dcr_initial_access_tokens SET max_registrations = 1`); err != nil {
		t.Fatal(err)
	}
	const body = `{"client_name":"Equivalent","redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"none"}`

	const contenders = 12
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- dcrRegistrationRequest(server, rawToken, body)
		}()
	}
	close(start)
	wg.Wait()
	close(responses)

	clientIDs := map[string]bool{}
	created, replayed := 0, 0
	for response := range responses {
		switch response.Code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			replayed++
		default:
			t.Fatalf("concurrent registration status=%d body=%q", response.Code, response.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		clientIDs[payload["client_id"].(string)] = true
	}
	if created != 1 || replayed != contenders-1 || len(clientIDs) != 1 {
		t.Fatalf("created=%d replayed=%d client_ids=%v", created, replayed, clientIDs)
	}
	if count, err := store.CountOAuthClients(context.Background()); err != nil || count != 1 {
		t.Fatalf("deduplicated clients=%d err=%v", count, err)
	}
	var used int
	if err := store.RawDB().QueryRow(`SELECT used_registrations FROM oauth_dcr_initial_access_tokens`).Scan(&used); err != nil || used != 1 {
		t.Fatalf("token usage=%d err=%v", used, err)
	}

	distinct := dcrRegistrationRequest(server, rawToken,
		`{"client_name":"Distinct","redirect_uris":["https://other.example/cb"]}`)
	if distinct.Code != http.StatusBadRequest || !strings.Contains(distinct.Body.String(), "quota reached") {
		t.Fatalf("quota response status=%d body=%q", distinct.Code, distinct.Body.String())
	}
}

func TestDCRRotationOverlapsAcrossNodesAndRevocationIsImmediate(t *testing.T) {
	oldNode, store := newTestServer(t)
	const oldToken = "ddddddddddddddddddddddddddddddddddddddddddd"
	const newToken = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	configureTokenDCR(t, oldNode, oldToken)

	newNode := NewOAuthServer(store, "https://test.example", oldNode.logger)
	configureTokenDCR(t, newNode, newToken)
	oldOnNew := dcrRegistrationRequest(newNode, oldToken,
		`{"client_name":"Old overlap","redirect_uris":["https://old.example/cb"]}`)
	newOnOld := dcrRegistrationRequest(oldNode, newToken,
		`{"client_name":"New overlap","redirect_uris":["https://new.example/cb"]}`)
	if oldOnNew.Code != http.StatusCreated || newOnOld.Code != http.StatusCreated {
		t.Fatalf("overlap statuses old/new=%d/%d bodies=%q / %q", oldOnNew.Code, newOnOld.Code, oldOnNew.Body.String(), newOnOld.Body.String())
	}

	oldHash := sha256.Sum256([]byte(oldToken))
	oldID := "bootstrap-" + fmt.Sprintf("%x", oldHash[:])[:16]
	applied, err := store.RevokeDCRInitialAccessToken(context.Background(), oldID, "test-operator", "rotation complete", time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("revoke old token applied=%v err=%v", applied, err)
	}
	revoked := dcrRegistrationRequest(oldNode, oldToken,
		`{"client_name":"Revoked","redirect_uris":["https://revoked.example/cb"]}`)
	stillActive := dcrRegistrationRequest(oldNode, newToken,
		`{"client_name":"Still active","redirect_uris":["https://active.example/cb"]}`)
	if revoked.Code != http.StatusUnauthorized || stillActive.Code != http.StatusCreated {
		t.Fatalf("post-revoke statuses old/new=%d/%d", revoked.Code, stillActive.Code)
	}
	if err := oldNode.ConfigureDynamicClientRegistration(DCRModeToken, oldHash); !errors.Is(err, storeport.ErrDCRInvalidInitialAccessToken) {
		t.Fatalf("restart resurrected revoked token: %v", err)
	}
	events, err := store.ListDCRAudit(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var created, revokedAudit bool
	for _, event := range events {
		created = created || event.Action == "token_created"
		revokedAudit = revokedAudit || event.Action == "token_revoked" && event.TokenID == oldID
	}
	if !created || !revokedAudit {
		t.Fatalf("rotation audit incomplete: %+v", events)
	}
}

func TestDCRConfidentialReplayReturnsSameEncryptedSecret(t *testing.T) {
	server, store := newTestServer(t)
	encodedKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32))
	keyring, err := db.NewIntegrationSecretKeyring("dcr:"+encodedKey, "dcr")
	if err != nil {
		t.Fatal(err)
	}
	store.SetIntegrationSecretKeyring(keyring)
	const rawToken = "fffffffffffffffffffffffffffffffffffffffffff"
	configureTokenDCR(t, server, rawToken)
	const body = `{"client_name":"Confidential","redirect_uris":["https://confidential.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`
	first := dcrRegistrationRequest(server, rawToken, body)
	second := dcrRegistrationRequest(server, rawToken, body)
	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("confidential statuses=%d/%d bodies=%q / %q", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var firstPayload, secondPayload map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &firstPayload)
	_ = json.Unmarshal(second.Body.Bytes(), &secondPayload)
	if firstPayload["client_id"] != secondPayload["client_id"] || firstPayload["client_secret"] != secondPayload["client_secret"] {
		t.Fatalf("confidential replay drifted: first=%v second=%v", firstPayload, secondPayload)
	}
	secret := firstPayload["client_secret"].(string)
	var ciphertext string
	if err := store.RawDB().QueryRow(`SELECT registration_secret_ciphertext FROM oauth_clients`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == "" || strings.Contains(ciphertext, secret) {
		t.Fatal("confidential replay secret was not stored as an opaque envelope")
	}
}
