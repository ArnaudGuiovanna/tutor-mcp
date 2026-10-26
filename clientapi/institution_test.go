// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package clientapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type institutionHTTPStore struct {
	storeport.Store
	calls    int
	revoked  bool
	sessions map[string]string
}

func (s *institutionHTTPStore) ValidatePrincipal(context.Context, models.Principal) error { return nil }
func (s *institutionHTTPStore) ConsumeNativeActionProof(context.Context, models.Principal, models.NativeActionProof) error {
	return nil
}
func (s *institutionHTTPStore) ValidateNativeSession(_ context.Context, id string, p models.Principal, _ time.Time) error {
	valid := id == "settings-native-session-001"
	if s.sessions != nil {
		valid = s.sessions[id] == p.TenantID
	}
	if s.revoked || !valid {
		return errors.New("revoked")
	}
	return nil
}

func TestInstitutionHTTPRejectsProofReplayAcrossTenantAndSession(t *testing.T) {
	t.Setenv("JWT_ED25519_KEYS", "")
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("i", 32))))
	if err := auth.LoadJWTSecret(); err != nil {
		t.Fatal(err)
	}
	const origin = "https://institution.example"
	const sessionA = "settings-native-session-001"
	const sessionB = "settings-native-session-002"
	p := models.Principal{TenantID: "institution-a", MembershipID: "member-a", UserID: "same-account", Roles: []string{models.RoleOwner}, Scopes: []string{models.OAuthScopeFormationContribute}, TokenVersion: 1}
	foreign := p
	foreign.TenantID, foreign.MembershipID = "institution-b", "member-b"
	s := &institutionHTTPStore{sessions: map[string]string{sessionA: p.TenantID, sessionB: p.TenantID, "foreign-native-session": foreign.TenantID}}
	h := auth.BearerMiddlewareWithNativePrincipalValidator(origin, RequiredScope, s, New(s, nil).Handler())
	body := `{"action":"overview","input":{}}`
	proof := institutionHTTPProof(t, origin+InstitutionActionsPath, p.TenantID, sessionA, body)
	for _, tc := range []struct {
		name      string
		principal models.Principal
		session   string
		want      int
	}{
		{"same signing account other institution", foreign, "foreign-native-session", 401},
		{"same signing account other session", p, sessionB, 401},
		{"original tenant and session", p, sessionA, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := auth.GenerateJWTForPrincipalAndScope(origin, auth.MCPResource(origin), "native:"+tc.session, tc.principal, models.OAuthScopeFormationContribute)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, InstitutionActionsPath, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set(auth.NativeActionProofHeader, proof)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d", w.Code, tc.want)
			}
		})
	}
	if s.calls != 1 {
		t.Fatalf("proof reached account service in %d contexts instead of one", s.calls)
	}
}

func institutionHTTPProof(t *testing.T, url, tenant, session, body string) string {
	t.Helper()
	secret := make([]byte, 32)
	secret[31] = 3
	key, pub := btcec.PrivKeyFromBytes(secret)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	payload := sha256.Sum256([]byte(body))
	tags := [][]string{{"u", url}, {"method", "POST"}, {"payload", hex.EncodeToString(payload[:])}, {"nonce", hex.EncodeToString(nonce[:])}, {"tenant", tenant}, {"session", session}}
	created := time.Now().UTC().Unix()
	public := hex.EncodeToString(schnorr.SerializePubKey(pub))
	canonical, err := json.Marshal([]any{0, public, created, 27235, tags, ""})
	if err != nil {
		t.Fatal(err)
	}
	id := sha256.Sum256(canonical)
	sig, err := schnorr.Sign(key, id[:])
	if err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"id": hex.EncodeToString(id[:]), "pubkey": public, "created_at": created, "kind": 27235, "tags": tags, "content": "", "sig": hex.EncodeToString(sig.Serialize())}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}
func (s *institutionHTTPStore) InstitutionAction(_ context.Context, p models.Principal, action string, _ models.InstitutionRequest) (any, error) {
	s.calls++
	return map[string]any{"action": action, "membership": p.MembershipID}, nil
}

func TestInstitutionHTTPRequiresNativeAccountSessionAndStrictInput(t *testing.T) {
	t.Setenv("JWT_ED25519_KEYS", "")
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("i", 32))))
	if err := auth.LoadJWTSecret(); err != nil {
		t.Fatal(err)
	}
	const origin = "https://institution.example"
	p := models.Principal{TenantID: "settings-tenant", MembershipID: "settings-member", UserID: "settings-user", Roles: []string{models.RoleOwner}, Scopes: []string{models.OAuthScopeFormationContribute}, TokenVersion: 1}
	s := &institutionHTTPStore{}
	api := New(s, nil).Handler()
	h := NoStore(auth.BearerMiddlewareWithNativePrincipalValidator(origin, RequiredScope, s, api))
	token, err := auth.GenerateJWTForPrincipalAndScope(origin, auth.MCPResource(origin), "native:settings-native-session-001", p, models.OAuthScopeFormationContribute)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, body, credential string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, InstitutionActionsPath, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
			r.Header.Set(auth.NativeActionProofHeader, institutionHTTPProof(t, origin+InstitutionActionsPath, p.TenantID, "settings-native-session-001", body))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("native account reply cacheable")
		}
		return w
	}
	if w := request(http.MethodPost, `{"action":"overview","input":{}}`, token); w.Code != 200 {
		t.Fatalf("owner without learner status=%d body=%s", w.Code, w.Body.String())
	}
	if s.calls != 1 {
		t.Fatal("native action did not reach service")
	}
	// Possession of the same bearer delegated to MCP is insufficient: only
	// the host can sign an account operation with the local identity key.
	unsigned := httptest.NewRequest(http.MethodPost, InstitutionActionsPath, strings.NewReader(`{"action":"overview","input":{}}`))
	unsigned.Header.Set("Content-Type", "application/json")
	unsigned.Header.Set("Authorization", "Bearer "+token)
	unsignedReply := httptest.NewRecorder()
	h.ServeHTTP(unsignedReply, unsigned)
	if unsignedReply.Code != 401 {
		t.Fatalf("delegated bearer alone acquired account access: %d", unsignedReply.Code)
	}
	for _, body := range []string{`{"action":"overview","input":{"tenant_id":"foreign"}}`, `{"action":"unknown","input":{}}`, `{"action":"overview","input":{}} {}`} {
		if w := request(http.MethodPost, body, token); w.Code != 400 {
			t.Fatalf("invalid native input=%d %s", w.Code, w.Body.String())
		}
	}
	if w := request(http.MethodGet, ``, token); w.Code != 405 {
		t.Fatal(w.Code)
	}
	if w := request(http.MethodPost, `{"action":"overview"}`, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	agentToken, err := auth.GenerateJWTForPrincipalAndScope(origin, auth.MCPResource(origin), "agent-mcp", p, models.OAuthScopeFormationContribute)
	if err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodPost, `{"action":"list_members","input":{}}`, agentToken); w.Code != 401 {
		t.Fatalf("agent token acquired native administration: %d", w.Code)
	}
	ctx, err := auth.WithPrincipal(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, InstitutionActionsPath, strings.NewReader(`{"action":"overview","input":{}}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("ordinary OAuth principal acquired native account access: %d", w.Code)
	}
	s.revoked = true
	if w := request(http.MethodPost, `{"action":"overview","input":{}}`, token); w.Code != 401 {
		t.Fatalf("revoked native session=%d", w.Code)
	}
	if s.calls != 1 {
		t.Fatal("rejected request reached institution service")
	}
}
