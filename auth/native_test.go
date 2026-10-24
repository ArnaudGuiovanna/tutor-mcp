// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

type nativeMemoryStore struct {
	storeport.NativeIdentityStore
	mu         sync.Mutex
	identity   models.NativeIdentity
	principal  models.Principal
	challenges map[string]models.NativeChallenge
	sessions   map[string]models.NativeSession
	grantErr   error
}

func (s *nativeMemoryStore) GetNativeIdentity(_ context.Context, tenant, key, action string) (*models.NativeIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expected := s.identity.PublicKey
	if action == "recover" {
		expected = s.identity.RecoveryPublicKey
	}
	if tenant != s.identity.TenantID || key != expected || s.identity.Status != "active" {
		return nil, storeport.ErrInvalidNativeIdentity
	}
	copy := s.identity
	return &copy, nil
}

func (s *nativeMemoryStore) CreateNativeChallenge(_ context.Context, row *models.NativeChallenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.challenges[row.TokenHash] = *row
	return nil
}

func (s *nativeMemoryStore) ConsumeNativeChallenge(_ context.Context, hash string, now time.Time) (*models.NativeChallenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.challenges[hash]
	if !ok || row.ConsumedAt != nil || !row.ExpiresAt.After(now) {
		return nil, storeport.ErrInvalidNativeChallenge
	}
	row.ConsumedAt = &now
	s.challenges[hash] = row
	return &row, nil
}

func (s *nativeMemoryStore) CreateNativeSession(_ context.Context, row *models.NativeSession) (models.Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grantErr != nil {
		return models.Principal{}, s.grantErr
	}
	if row.IdentityID != s.identity.ID || row.KeyVersion != s.identity.Version {
		return models.Principal{}, storeport.ErrInvalidNativeIdentity
	}
	p := s.principal
	p.Scopes = append([]string(nil), row.Scopes...)
	allowed := nativeAllowedScopes(p)
	for _, scope := range row.Scopes {
		if !slices.Contains(allowed, scope) {
			return models.Principal{}, storeport.ErrInvalidOAuthScope
		}
	}
	s.sessions[row.ID] = *row
	return p, nil
}

func (s *nativeMemoryStore) RotateNativeIdentity(_ context.Context, id string, version int64, pub, recovery string, now time.Time) (*models.NativeIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.identity.ID || version != s.identity.Version {
		return nil, storeport.ErrInvalidNativeIdentity
	}
	s.identity.PublicKey, s.identity.RecoveryPublicKey = pub, recovery
	s.identity.Version++
	s.principal.TokenVersion++
	for id, session := range s.sessions {
		session.RevokedAt = &now
		s.sessions[id] = session
	}
	copy := s.identity
	return &copy, nil
}

func (s *nativeMemoryStore) RevokeNativeSession(_ context.Context, identity, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.sessions[id]
	if !ok || row.IdentityID != identity {
		return storeport.ErrInvalidNativeSession
	}
	row.RevokedAt = &now
	s.sessions[id] = row
	return nil
}

func (s *nativeMemoryStore) ValidateNativeSession(_ context.Context, id string, principal models.Principal, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.sessions[id]
	if !ok || row.RevokedAt != nil || !row.ExpiresAt.After(now) || row.IdentityID != s.identity.ID ||
		row.KeyVersion != s.identity.Version || row.MembershipID != principal.MembershipID ||
		row.UserID != principal.UserID || row.TenantID != principal.TenantID || strings.Join(row.Scopes, " ") != strings.Join(principal.Scopes, " ") {
		return storeport.ErrInvalidNativeSession
	}
	return nil
}

func (s *nativeMemoryStore) ValidatePrincipal(_ context.Context, principal models.Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if principal.MembershipID != s.principal.MembershipID || principal.TokenVersion != s.principal.TokenVersion || !slices.Equal(principal.Roles, s.principal.Roles) {
		return storeport.ErrInvalidPrincipal
	}
	return nil
}

func nativeTestKey(value byte) *btcec.PrivateKey {
	raw := make([]byte, 32)
	raw[31] = value
	key, _ := btcec.PrivKeyFromBytes(raw)
	return key
}

func nativeTestPub(key *btcec.PrivateKey) string {
	return hex.EncodeToString(schnorr.SerializePubKey(key.PubKey()))
}

func newNativeTestServer(t *testing.T, owner bool) (*NativeIdentityServer, *nativeMemoryStore, *btcec.PrivateKey, *btcec.PrivateKey) {
	t.Helper()
	t.Setenv("JWT_ED25519_KEYS", "")
	setTestSecret(t)
	key, recovery := nativeTestKey(3), nativeTestKey(4)
	principal := models.Principal{
		UserID: "user_native", TenantID: "tenant_native", MembershipID: "member_native", LearnerID: "learner_native",
		Roles: []string{models.RoleLearner}, Scopes: []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite}, TokenVersion: 1,
	}
	if owner {
		principal.LearnerID = ""
		principal.Roles = []string{models.RoleOwner}
		principal.Scopes = []string{models.OAuthScopeFormationRead}
	}
	store := &nativeMemoryStore{
		identity: models.NativeIdentity{
			ID: "identity_native", UserID: principal.UserID, TenantID: principal.TenantID, MembershipID: principal.MembershipID,
			LearnerID: principal.LearnerID, PublicKey: nativeTestPub(key), RecoveryPublicKey: nativeTestPub(recovery), Status: "active", Version: 1,
		},
		principal: principal, challenges: make(map[string]models.NativeChallenge), sessions: make(map[string]models.NativeSession),
	}
	server, err := NewNativeIdentityServer(store, NativeIdentityOptions{BaseURL: "https://native.example", InstitutionID: principal.TenantID, InstitutionName: "Formation par clés"})
	if err != nil {
		t.Fatal(err)
	}
	return server, store, key, recovery
}

func nativeTestPost(handler http.HandlerFunc, endpoint string, raw []byte, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder
}

type nativeTestChallenge struct {
	Challenge   string `json:"challenge"`
	ExpiresAt   int64  `json:"expires_at"`
	KeyVersion  int64  `json:"key_version"`
	URL         string `json:"url"`
	Method      string `json:"method"`
	RequestBody string `json:"request_body"`
	PayloadHash string `json:"payload_hash"`
	TenantID    string `json:"tenant_id"`
	PublicKey   string `json:"public_key"`
	Action      string `json:"action"`
	Scope       string `json:"scope"`
}

func nativeGetChallenge(t *testing.T, server *NativeIdentityServer, key *btcec.PrivateKey, action, scope string, extra map[string]string) nativeTestChallenge {
	t.Helper()
	payload := map[string]string{"tenant_id": server.options.InstitutionID, "public_key": nativeTestPub(key), "action": action, "scope": scope}
	for key, value := range extra {
		payload[key] = value
	}
	raw, _ := json.Marshal(payload)
	recorder := nativeTestPost(server.HandleChallenge, server.endpoint("challenge"), raw, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("challenge status=%d body=%s", recorder.Code, recorder.Body)
	}
	var result nativeTestChallenge
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func nativeSignEvent(t *testing.T, key *btcec.PrivateKey, challenge nativeTestChallenge, mutate func(*nativeNostrEvent)) string {
	t.Helper()
	event := nativeNostrEvent{
		PublicKey: nativeTestPub(key), CreatedAt: time.Now().Unix(), Kind: nativeEventKind, Content: "",
		Tags: [][]string{
			{"u", challenge.URL}, {"method", challenge.Method}, {"payload", challenge.PayloadHash}, {"challenge", challenge.Challenge},
			{"tenant", challenge.TenantID}, {"action", challenge.Action}, {"scope", challenge.Scope}, {"version", strconv.FormatInt(challenge.KeyVersion, 10)},
		},
	}
	if mutate != nil {
		mutate(&event)
	}
	// Serialize independently from nativeEventID: these wire tags use ASCII.
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode([]any{0, event.PublicKey, event.CreatedAt, event.Kind, event.Tags, event.Content}); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(bytes.TrimSuffix(data.Bytes(), []byte("\n")))
	event.ID = hex.EncodeToString(hash[:])
	signature, err := schnorr.Sign(key, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	event.Signature = hex.EncodeToString(signature.Serialize())
	raw, _ := json.Marshal(event)
	return "Nostr " + base64.StdEncoding.EncodeToString(raw)
}

func nativeOpenSession(t *testing.T, server *NativeIdentityServer, key *btcec.PrivateKey, scope string) (string, string) {
	t.Helper()
	challenge := nativeGetChallenge(t, server, key, "session", scope, nil)
	recorder := nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": nativeSignEvent(t, key, challenge, nil)})
	if recorder.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", recorder.Code, recorder.Body)
	}
	var result struct {
		Token     string `json:"access_token"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.Token == "" || result.SessionID == "" {
		t.Fatalf("invalid session response: %v %s", err, recorder.Body)
	}
	return result.Token, result.SessionID
}

func TestNativeSessionUsesMembershipScopesAndDefaultOwnerRead(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(strconv.FormatBool(owner), func(t *testing.T) {
			server, store, key, _ := newNativeTestServer(t, owner)
			token, id := nativeOpenSession(t, server, key, "")
			claims, err := VerifyJWTClaims(token, server.options.BaseURL)
			if err != nil || claims.AuthorizedParty != "native:"+id || claims.TenantID != store.principal.TenantID {
				t.Fatalf("claims=%+v err=%v", claims, err)
			}
			want := models.OAuthScopeLearnerReadWrite
			if owner {
				want = models.OAuthScopeFormationRead
			}
			if claims.Scope != want || claims.LearnerID != store.principal.LearnerID || claims.TokenVersion != store.principal.TokenVersion {
				t.Fatalf("wrong grant: %+v", claims)
			}
			for hash, challenge := range store.challenges {
				if len(hash) != 64 || !challenge.ExpiresAt.Equal(challenge.CreatedAt.Add(NativeChallengeTTL)) || challenge.ConsumedAt == nil {
					t.Fatalf("challenge not bounded/consumed: %+v", challenge)
				}
			}
		})
	}
}

func TestNativeOwnerAuthoringIsExplicitAndLearnerCannotEscalate(t *testing.T) {
	server, _, key, _ := newNativeTestServer(t, true)
	token, _ := nativeOpenSession(t, server, key, "formation:read formation:write")
	claims, _ := VerifyJWTClaims(token, server.options.BaseURL)
	if claims.Scope != "formation:read formation:write" || claims.LearnerID != "" {
		t.Fatal("owner authoring grant is incorrect")
	}
	server, _, key, _ = newNativeTestServer(t, false)
	challenge := nativeGetChallenge(t, server, key, "session", "formation:write", nil)
	recorder := nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": nativeSignEvent(t, key, challenge, nil)})
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "insufficient_scope") {
		t.Fatalf("learner escalated: %d %s", recorder.Code, recorder.Body)
	}
}

func TestNativeChallengeIsSingleUseAcrossConcurrentRequests(t *testing.T) {
	server, store, key, _ := newNativeTestServer(t, false)
	challenge := nativeGetChallenge(t, server, key, "session", "", nil)
	header := nativeSignEvent(t, key, challenge, nil)
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for range 8 {
		wg.Go(func() {
			recorder := nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": header})
			statuses <- recorder.Code
		})
	}
	wg.Wait()
	close(statuses)
	accepted := 0
	for status := range statuses {
		if status == http.StatusOK {
			accepted++
		} else if status != http.StatusUnauthorized {
			t.Fatalf("unexpected response %d", status)
		}
	}
	if accepted != 1 || len(store.sessions) != 1 {
		t.Fatalf("replay admitted %d requests / %d sessions", accepted, len(store.sessions))
	}
}

func TestNativeNostrRejectsEveryRequestBindingMutation(t *testing.T) {
	cases := map[string]func(*nativeNostrEvent){
		"url":       func(e *nativeNostrEvent) { e.Tags[0][1] += "?changed=1" },
		"method":    func(e *nativeNostrEvent) { e.Tags[1][1] = "GET" },
		"payload":   func(e *nativeNostrEvent) { e.Tags[2][1] = strings.Repeat("0", 64) },
		"challenge": func(e *nativeNostrEvent) { e.Tags[3][1] = strings.Repeat("A", 43) },
		"tenant":    func(e *nativeNostrEvent) { e.Tags[4][1] = "other_tenant" },
		"action":    func(e *nativeNostrEvent) { e.Tags[5][1] = "recover" },
		"scope":     func(e *nativeNostrEvent) { e.Tags[6][1] = "formation:write" },
		"version":   func(e *nativeNostrEvent) { e.Tags[7][1] = "2" },
		"duplicate": func(e *nativeNostrEvent) { e.Tags[7] = []string{"scope", e.Tags[6][1]} },
		"extra":     func(e *nativeNostrEvent) { e.Tags = append(e.Tags, []string{"x", "ignored"}) },
		"content":   func(e *nativeNostrEvent) { e.Content = "unexpected" },
		"kind":      func(e *nativeNostrEvent) { e.Kind = 22242 },
		"expired":   func(e *nativeNostrEvent) { e.CreatedAt -= 121 },
		"future":    func(e *nativeNostrEvent) { e.CreatedAt += 31 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			server, store, key, _ := newNativeTestServer(t, false)
			challenge := nativeGetChallenge(t, server, key, "session", "", nil)
			recorder := nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": nativeSignEvent(t, key, challenge, mutate)})
			if recorder.Code != http.StatusUnauthorized || len(store.sessions) != 0 {
				t.Fatalf("mutation accepted: %d %s", recorder.Code, recorder.Body)
			}
		})
	}
}

func TestNativeRejectsChangedBodyOriginAndDuplicateJSONWithoutConsumingNonce(t *testing.T) {
	server, store, key, _ := newNativeTestServer(t, false)
	challenge := nativeGetChallenge(t, server, key, "session", "", nil)
	header := nativeSignEvent(t, key, challenge, nil)
	for _, input := range []struct{ url, body string }{
		{challenge.URL, challenge.RequestBody + " "},
		{"https://other.example" + nativeIdentityPrefix + "session", challenge.RequestBody},
		{challenge.URL + "?x=1", challenge.RequestBody},
		{challenge.URL, strings.TrimSuffix(challenge.RequestBody, "}") + `,"scope":"formation:write"}`},
	} {
		recorder := nativeTestPost(server.HandleSession, input.url, []byte(input.body), map[string]string{"Authorization": header})
		if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusBadRequest {
			t.Fatalf("mutation accepted: %d %s", recorder.Code, recorder.Body)
		}
	}
	if store.challenges[nativeTokenHash(challenge.Challenge)].ConsumedAt != nil {
		t.Fatal("invalid signatures consumed another request's nonce")
	}
	recorder := nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": header})
	if recorder.Code != http.StatusOK {
		t.Fatalf("original request rejected: %d %s", recorder.Code, recorder.Body)
	}
}

func TestNativeInvalidSignaturesAndAmbiguousHeadersCannotConsumeChallenge(t *testing.T) {
	server, store, key, _ := newNativeTestServer(t, false)
	challenge := nativeGetChallenge(t, server, key, "session", "", nil)
	valid := nativeSignEvent(t, key, challenge, nil)
	for _, field := range []string{"id", "sig", "pubkey"} {
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(valid, "Nostr "))
		var event map[string]any
		_ = json.Unmarshal(raw, &event)
		event[field] = strings.Repeat("0", 64)
		if field == "sig" {
			event[field] = strings.Repeat("0", 128)
		}
		raw, _ = json.Marshal(event)
		w := nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": "Nostr " + base64.StdEncoding.EncodeToString(raw)})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid %s accepted: %d", field, w.Code)
		}
	}
	for _, field := range []string{"content", "tags"} {
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(valid, "Nostr "))
		var event map[string]any
		_ = json.Unmarshal(raw, &event)
		event[field] = nil
		raw, _ = json.Marshal(event)
		w := nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": "Nostr " + base64.StdEncoding.EncodeToString(raw)})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("null %s accepted", field)
		}
	}
	request := httptest.NewRequest(http.MethodPost, challenge.URL, strings.NewReader(challenge.RequestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Add("Authorization", valid)
	request.Header.Add("Authorization", valid)
	w := httptest.NewRecorder()
	server.HandleSession(w, request)
	if w.Code != http.StatusUnauthorized || store.challenges[nativeTokenHash(challenge.Challenge)].ConsumedAt != nil {
		t.Fatal("ambiguous headers accepted or consumed challenge")
	}
}

func TestNativeStaleChallengeCannotCreateASessionAfterRotation(t *testing.T) {
	server, store, key, recovery := newNativeTestServer(t, false)
	stale := nativeGetChallenge(t, server, key, "session", "", nil)
	staleProof := nativeSignEvent(t, key, stale, nil)
	newKey, newRecovery := nativeTestKey(20), nativeTestKey(21)
	rotation := nativeGetChallenge(t, server, key, "rotate", "", map[string]string{
		"new_public_key": nativeTestPub(newKey), "new_recovery_public_key": nativeTestPub(newRecovery),
	})
	w := nativeTestPost(server.HandleRotate, rotation.URL, []byte(rotation.RequestBody), map[string]string{
		"Authorization":              nativeSignEvent(t, key, rotation, nil),
		"X-Tutor-New-Key-Proof":      nativeSignEvent(t, newKey, rotation, nil),
		"X-Tutor-New-Recovery-Proof": nativeSignEvent(t, newRecovery, rotation, nil),
		"X-Tutor-Recovery-Approval":  nativeSignEvent(t, recovery, rotation, nil),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("rotation failed: %d %s", w.Code, w.Body)
	}
	w = nativeTestPost(server.HandleSession, stale.URL, []byte(stale.RequestBody), map[string]string{"Authorization": staleProof})
	if w.Code != http.StatusUnauthorized || len(store.sessions) != 0 {
		t.Fatalf("stale key version admitted: %d %s", w.Code, w.Body)
	}
	_, _ = nativeOpenSession(t, server, newKey, "")
}

func TestNativeBearerChecksRevocationAndRejectsLegacyTokens(t *testing.T) {
	server, store, key, _ := newNativeTestServer(t, false)
	token, id := nativeOpenSession(t, server, key, "")
	handler := BearerMiddlewareWithNativePrincipalValidator(server.options.BaseURL, models.OAuthScopeLearnerRead, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := GetPrincipal(r.Context()); !ok {
			t.Error("principal absent")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func(token string) int {
		r := httptest.NewRequest("GET", server.options.BaseURL+"/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(token); got != http.StatusNoContent {
		t.Fatalf("valid native token rejected: %d", got)
	}
	unknown, err := GenerateJWTForPrincipalAndScope(server.options.BaseURL, MCPResource(server.options.BaseURL), "native:unknown-session", store.principal, models.OAuthScopeLearnerReadWrite)
	if err != nil || request(unknown) != http.StatusUnauthorized {
		t.Fatalf("nonexistent DB session admitted: %v", err)
	}
	legacy, err := GenerateJWTForPrincipalAndScope(server.options.BaseURL, MCPResource(server.options.BaseURL), "oauth-browser-client", store.principal, models.OAuthScopeLearnerReadWrite)
	if err != nil || request(legacy) != http.StatusUnauthorized {
		t.Fatalf("legacy token admitted: %v", err)
	}
	challenge := nativeGetChallenge(t, server, key, "logout", "", map[string]string{"session_id": id})
	w := nativeTestPost(server.HandleLogout, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": nativeSignEvent(t, key, challenge, nil)})
	if w.Code != http.StatusOK || request(token) != http.StatusUnauthorized {
		t.Fatalf("logout failed: %d %s", w.Code, w.Body)
	}
	nextToken, _ := nativeOpenSession(t, server, key, "")
	if request(nextToken) != http.StatusNoContent {
		t.Fatal("fresh signed reauthentication failed")
	}
}

func TestNativeRotationAndOfflineRecoveryRequireBothNewKeyProofs(t *testing.T) {
	server, store, key, recovery := newNativeTestServer(t, true)
	oldToken, oldSession := nativeOpenSession(t, server, key, "")
	for i, action := range []string{"rotate", "recover"} {
		signer := key
		if action == "recover" {
			signer = recovery
		}
		newKey, newRecovery := nativeTestKey(byte(10+i*2)), nativeTestKey(byte(11+i*2))
		challenge := nativeGetChallenge(t, server, signer, action, "", map[string]string{
			"new_public_key": nativeTestPub(newKey), "new_recovery_public_key": nativeTestPub(newRecovery),
		})
		headers := map[string]string{"Authorization": nativeSignEvent(t, signer, challenge, nil), "X-Tutor-New-Key-Proof": nativeSignEvent(t, newKey, challenge, nil)}
		handler := server.HandleRotate
		if action == "recover" {
			handler = server.HandleRecover
		}
		w := nativeTestPost(handler, challenge.URL, []byte(challenge.RequestBody), headers)
		if w.Code != http.StatusUnauthorized || store.identity.Version != int64(i+1) {
			t.Fatalf("rotation without recovery possession: %d %s", w.Code, w.Body)
		}
		headers["X-Tutor-New-Recovery-Proof"] = nativeSignEvent(t, newRecovery, challenge, nil)
		if action == "rotate" {
			// Possession of the active key and both attacker-chosen new keys is
			// insufficient to overwrite the existing offline recovery key.
			w = nativeTestPost(handler, challenge.URL, []byte(challenge.RequestBody), headers)
			if w.Code != http.StatusUnauthorized || store.identity.Version != int64(i+1) {
				t.Fatal("online key alone replaced the offline recovery credential")
			}
			headers["X-Tutor-Recovery-Approval"] = nativeSignEvent(t, newRecovery, challenge, nil)
			w = nativeTestPost(handler, challenge.URL, []byte(challenge.RequestBody), headers)
			if w.Code != http.StatusUnauthorized {
				t.Fatal("new recovery key impersonated the current offline recovery key")
			}
			headers["X-Tutor-Recovery-Approval"] = nativeSignEvent(t, recovery, challenge, nil)
		}
		w = nativeTestPost(handler, challenge.URL, []byte(challenge.RequestBody), headers)
		if w.Code != http.StatusOK || store.identity.PublicKey != nativeTestPub(newKey) || store.identity.Version != int64(i+2) {
			t.Fatalf("rotation failed: %d %s", w.Code, w.Body)
		}
		claims, _ := VerifyJWTClaims(oldToken, server.options.BaseURL)
		p, _ := claims.Principal()
		if store.ValidateNativeSession(t.Context(), oldSession, p, time.Now()) == nil || store.ValidatePrincipal(t.Context(), p) == nil {
			t.Fatal("rotation preserved an old bearer grant")
		}
		key, recovery = newKey, newRecovery
	}
	if _, err := store.GetNativeIdentity(t.Context(), store.identity.TenantID, nativeTestPub(nativeTestKey(3)), "session"); err == nil {
		t.Fatal("former identity key still accepted")
	}
}

func TestNativeExpiredChallengeAndStoreFailuresFailClosed(t *testing.T) {
	server, store, key, _ := newNativeTestServer(t, false)
	challenge := nativeGetChallenge(t, server, key, "session", "", nil)
	server.now = func() time.Time { return time.Now().Add(NativeChallengeTTL + time.Second) }
	w := nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": nativeSignEvent(t, key, challenge, nil)})
	if w.Code != http.StatusUnauthorized {
		t.Fatal("expired challenge admitted")
	}
	server.now = time.Now
	store.grantErr = errors.New("database-password-sensitive")
	challenge = nativeGetChallenge(t, server, key, "session", "", nil)
	w = nativeTestPost(server.HandleSession, challenge.URL, []byte(challenge.RequestBody), map[string]string{"Authorization": nativeSignEvent(t, key, challenge, nil)})
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "password-sensitive") {
		t.Fatalf("store error leaked or accepted: %d %s", w.Code, w.Body)
	}
}

func TestNativeDiscoveryAndBoundedInput(t *testing.T) {
	server, _, key, _ := newNativeTestServer(t, false)
	w := httptest.NewRecorder()
	server.HandleDiscovery(w, httptest.NewRequest("GET", server.options.BaseURL+"/.well-known/tutor-institution", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mode":"operator"`) || strings.Contains(w.Body.String(), "nsec") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("discovery invalid: %d %s", w.Code, w.Body)
	}
	for _, raw := range []string{
		`{"tenant_id":"other","public_key":"` + nativeTestPub(key) + `","action":"session","scope":""}`,
		`{"tenant_id":"tenant_native","public_key":"` + strings.Repeat("0", 64) + `","action":"session","scope":""}`,
		`{"tenant_id":"tenant_native","public_key":"` + nativeTestPub(key) + `","action":"signup","scope":"formation:write"}`,
		`{"tenant_id":"tenant_native","public_key":"` + nativeTestPub(key) + `","action":"session","scope":"learner"}`,
		strings.Repeat("x", 16*1024+1),
	} {
		w := nativeTestPost(server.HandleChallenge, server.endpoint("challenge"), []byte(raw), nil)
		if w.Code < 400 {
			t.Fatalf("invalid input accepted: %d", w.Code)
		}
	}
}

func TestNativeInstitutionURLIsAnOriginAcceptedByTheClient(t *testing.T) {
	server, _, _, _ := newNativeTestServer(t, false)
	for _, base := range []string{"https://tutor.example", "https://tutor.example/", "http://127.0.0.1:3099"} {
		options := server.options
		options.BaseURL = base
		if _, err := NewNativeIdentityServer(server.store, options); err != nil {
			t.Fatalf("canonical origin rejected: %s: %v", base, err)
		}
	}
	for _, base := range []string{"https://tutor.example/prefix", "https://tutor.example//", "https://tutor.example/%2f", "https://tutor.example?", "https://tutor.example/?query=1", "https://tutor.example/#fragment", "https://user:secret@tutor.example", "https://TUTOR.example", "https://tutor.example:443", " https://tutor.example", "http://tutor.example"} {
		options := server.options
		options.BaseURL = base
		if _, err := NewNativeIdentityServer(server.store, options); err == nil {
			t.Fatalf("noncanonical origin accepted: %s", base)
		}
	}
}

func TestNativeCanonicalNostrJSONPreservesUnicodeSeparatorsAndEscapes(t *testing.T) {
	input := "<é>&\u2028\u2029\"\\\n\r\t\b\f\x01"
	want := "\"<é>&\u2028\u2029\\\"\\\\\\n\\r\\t\\b\\f\\u0001\""
	if got := string(appendNativeJSONString(nil, input)); got != want {
		t.Fatalf("NIP01 serialization differs: got=%q want=%q", got, want)
	}
}

func TestNativeNIP98AcceptsIndependentRustClientFixture(t *testing.T) {
	// Generated and verified with the client's nostr 0.44.8 implementation,
	// using public BIP340 test scalar 3. Never use that scalar for an identity.
	raw, err := os.ReadFile("testdata/native-rust-nip98.json")
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"u": "https://interop.example/api/client/v1/identity/session", "method": "POST",
		"payload": strings.Repeat("0", 64), "challenge": strings.Repeat("A", 43),
		"tenant": "tenant_interop", "action": "session", "scope": models.OAuthScopeLearnerReadWrite, "version": "1",
	}
	if err := verifyNativeNostr("Nostr "+base64.StdEncoding.EncodeToString(raw), true,
		"f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9", expected, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("Go rejected the Rust NIP98/BIP340 event: %v", err)
	}
}
