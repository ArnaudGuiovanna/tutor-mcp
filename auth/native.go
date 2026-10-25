// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const NativeChallengeTTL = 120 * time.Second

type nativeClientContextKey struct{}

// IsNativeClientSession identifies the already-verified native transport. It
// only controls discovery; every call still checks its scope and live role.
func IsNativeClientSession(ctx context.Context) bool {
	value, _ := ctx.Value(nativeClientContextKey{}).(bool)
	return value
}

const nativeIdentityPrefix = "/api/client/v1/identity/"

type NativeIdentityOptions struct {
	BaseURL         string
	InstitutionID   string
	InstitutionName string
}

type NativeIdentityServer struct {
	store   storeport.NativeIdentityStore
	options NativeIdentityOptions
	origin  *url.URL
	now     func() time.Time
}

type nativeChallengeRequest struct {
	TenantID             string `json:"tenant_id"`
	PublicKey            string `json:"public_key"`
	Action               string `json:"action"`
	Scope                string `json:"scope"`
	NewPublicKey         string `json:"new_public_key,omitempty"`
	NewRecoveryPublicKey string `json:"new_recovery_public_key,omitempty"`
	SessionID            string `json:"session_id,omitempty"`
}

type nativeSignedPayload struct {
	Challenge            string `json:"challenge"`
	TenantID             string `json:"tenant_id"`
	PublicKey            string `json:"public_key"`
	Action               string `json:"action"`
	Scope                string `json:"scope"`
	KeyVersion           int64  `json:"key_version"`
	NewPublicKey         string `json:"new_public_key,omitempty"`
	NewRecoveryPublicKey string `json:"new_recovery_public_key,omitempty"`
	SessionID            string `json:"session_id,omitempty"`
}

func NewNativeIdentityServer(store storeport.NativeIdentityStore, options NativeIdentityOptions) (*NativeIdentityServer, error) {
	base := strings.TrimSuffix(options.BaseURL, "/")
	origin, err := url.Parse(base)
	if err != nil || len(base) > 2048 || base != strings.TrimSpace(base) || origin.Host == "" ||
		origin.User != nil || origin.Path != "" || origin.RawPath != "" || origin.RawQuery != "" ||
		origin.ForceQuery || origin.Fragment != "" || origin.Host != strings.ToLower(origin.Host) ||
		(origin.Scheme == "https" && origin.Port() == "443") || (origin.Scheme == "http" && origin.Port() == "80") {
		return nil, fmt.Errorf("native identity requires a canonical institution URL")
	}
	loopback := strings.EqualFold(origin.Hostname(), "localhost")
	if ip := net.ParseIP(origin.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if origin.Scheme != "https" && !(origin.Scheme == "http" && loopback) {
		return nil, fmt.Errorf("native identity requires HTTPS (HTTP is permitted only on loopback)")
	}
	if store == nil || !nativeID(options.InstitutionID) || len(options.InstitutionName) > 200 ||
		strings.IndexFunc(options.InstitutionName, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("native identity requires a store and a canonical institution ID/name")
	}
	options.BaseURL = base
	if options.InstitutionName == "" {
		options.InstitutionName = options.InstitutionID
	}
	return &NativeIdentityServer{store: store, options: options, origin: origin, now: time.Now}, nil
}

func nativeID(value string) bool {
	return value != "" && len(value) <= 255 && value == strings.TrimSpace(value) &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func nativeNonce(value string) bool {
	if len(value) != 43 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func nativeTokenHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func (s *NativeIdentityServer) endpoint(action string) string {
	return s.options.BaseURL + nativeIdentityPrefix + action
}

func (s *NativeIdentityServer) HandleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		nativeError(w, http.StatusMethodNotAllowed, "invalid_request")
		return
	}
	nativeJSON(w, http.StatusOK, map[string]any{
		"version": 1, "institution_id": s.options.InstitutionID, "name": s.options.InstitutionName,
		"issuer": s.options.BaseURL, "mcp_url": MCPResource(s.options.BaseURL),
		"registration": map[string]string{"mode": "operator"},
		"capabilities": map[string]bool{
			"key_rotation": true, "recovery_key": true, "session_revocation": true,
			"signed_reauthentication": true, "agent_delegation": false,
		},
		"identity": map[string]any{
			"scheme": "nostr-nip98", "event_kind": nativeEventKind, "challenge_ttl_seconds": int(NativeChallengeTTL.Seconds()),
			"challenge_url": s.endpoint("challenge"), "session_url": s.endpoint("session"),
			"rotate_url": s.endpoint("rotate"), "recover_url": s.endpoint("recover"), "logout_url": s.endpoint("logout"),
		},
	})
}

func nativeReadBody(w http.ResponseWriter, r *http.Request, destination any) ([]byte, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		nativeError(w, http.StatusMethodNotAllowed, "invalid_request")
		return nil, false
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) ||
		(r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity") {
		nativeError(w, http.StatusUnsupportedMediaType, "invalid_request")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		nativeError(w, http.StatusRequestEntityTooLarge, "invalid_request")
		return nil, false
	}
	if err := decodeNativeJSON(raw, destination); err != nil {
		nativeError(w, http.StatusBadRequest, "invalid_request")
		return nil, false
	}
	return raw, true
}

func nativePayloadFields(action, publicKey, newPublicKey, newRecovery, sessionID string) bool {
	if !nativePublicKey(publicKey) {
		return false
	}
	switch action {
	case "session":
		return newPublicKey == "" && newRecovery == "" && sessionID == ""
	case "logout":
		return newPublicKey == "" && newRecovery == "" && nativeID(sessionID)
	case "rotate", "recover":
		return sessionID == "" && nativePublicKey(newPublicKey) && nativePublicKey(newRecovery) &&
			newPublicKey != newRecovery && newPublicKey != publicKey && newRecovery != publicKey
	default:
		return false
	}
}

func nativeIdentityMatches(identity *models.NativeIdentity, tenant, signer, action string) bool {
	if identity == nil || !nativeID(identity.ID) || identity.TenantID != tenant || identity.Status != "active" || identity.Version < 1 ||
		!nativePublicKey(identity.PublicKey) || !nativePublicKey(identity.RecoveryPublicKey) || identity.PublicKey == identity.RecoveryPublicKey {
		return false
	}
	if action == "recover" {
		return identity.RecoveryPublicKey == signer
	}
	return identity.PublicKey == signer
}

func (s *NativeIdentityServer) HandleChallenge(w http.ResponseWriter, r *http.Request) {
	var request nativeChallengeRequest
	if _, ok := nativeReadBody(w, r, &request); !ok {
		return
	}
	if request.TenantID != s.options.InstitutionID || !nativePayloadFields(request.Action, request.PublicKey, request.NewPublicKey, request.NewRecoveryPublicKey, request.SessionID) {
		nativeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	identity, err := s.store.GetNativeIdentity(r.Context(), request.TenantID, request.PublicKey, request.Action)
	if err != nil {
		nativeStoreError(w, err)
		return
	}
	if !nativeIdentityMatches(identity, request.TenantID, request.PublicKey, request.Action) {
		nativeError(w, http.StatusUnauthorized, "invalid_identity")
		return
	}
	scope := request.Scope
	if scope == "" {
		scope = models.OAuthScopeFormationRead
		if identity.LearnerID != "" {
			scope = models.OAuthScopeLearnerReadWrite
		}
	}
	scope, err = models.CanonicalOAuthScope(scope)
	if err != nil || scope == models.OAuthScopeLearner {
		nativeError(w, http.StatusBadRequest, "invalid_scope")
		return
	}
	challenge, err := newOpaqueToken()
	if err != nil {
		nativeError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	payload := nativeSignedPayload{
		Challenge: challenge, TenantID: request.TenantID, PublicKey: request.PublicKey,
		Action: request.Action, Scope: scope, KeyVersion: identity.Version,
		NewPublicKey: request.NewPublicKey, NewRecoveryPublicKey: request.NewRecoveryPublicKey, SessionID: request.SessionID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		nativeError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	now := s.now().UTC()
	hash := sha256.Sum256(body)
	row := &models.NativeChallenge{
		TokenHash: nativeTokenHash(challenge), IdentityID: identity.ID, TenantID: request.TenantID,
		PublicKey: request.PublicKey, Action: request.Action, Scopes: strings.Fields(scope), KeyVersion: identity.Version,
		URL: s.endpoint(request.Action), Method: http.MethodPost, PayloadHash: hex.EncodeToString(hash[:]),
		CreatedAt: now, ExpiresAt: now.Add(NativeChallengeTTL),
	}
	if err := s.store.CreateNativeChallenge(r.Context(), row); err != nil {
		nativeStoreError(w, err)
		return
	}
	nativeJSON(w, http.StatusOK, map[string]any{
		"challenge": challenge, "expires_at": row.ExpiresAt.Unix(), "key_version": row.KeyVersion,
		"url": row.URL, "method": row.Method, "request_body": string(body), "payload_hash": row.PayloadHash,
		"tenant_id": row.TenantID, "public_key": row.PublicKey, "action": row.Action, "scope": scope,
	})
}

func (s *NativeIdentityServer) HandleSession(w http.ResponseWriter, r *http.Request) {
	payload, row, ok := s.authorize(w, r, "session")
	if !ok {
		return
	}
	identity, err := s.store.GetNativeIdentity(r.Context(), payload.TenantID, payload.PublicKey, "session")
	if err != nil || !nativeIdentityMatches(identity, payload.TenantID, payload.PublicKey, "session") || identity.ID != row.IdentityID || identity.Version != row.KeyVersion {
		if err == nil {
			err = storeport.ErrInvalidNativeIdentity
		}
		nativeStoreError(w, err)
		return
	}
	sessionID, err := newOpaqueToken()
	if err != nil {
		nativeError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	now := s.now().UTC()
	session := &models.NativeSession{
		ID: sessionID, IdentityID: identity.ID, TenantID: identity.TenantID, UserID: identity.UserID,
		MembershipID: identity.MembershipID, LearnerID: identity.LearnerID, KeyVersion: identity.Version,
		Scopes: strings.Fields(payload.Scope), CreatedAt: now, ExpiresAt: now.Add(AccessTokenTTL),
	}
	principal, err := s.store.CreateNativeSession(r.Context(), session)
	if err != nil {
		nativeStoreError(w, err)
		return
	}
	if principal.Validate() != nil || principal.UserID != session.UserID || principal.TenantID != session.TenantID ||
		principal.MembershipID != session.MembershipID || principal.LearnerID != session.LearnerID || strings.Join(principal.Scopes, " ") != payload.Scope {
		_ = s.store.RevokeNativeSession(r.Context(), identity.ID, session.ID, now)
		nativeError(w, http.StatusUnauthorized, "invalid_identity")
		return
	}
	allowed := nativeAllowedScopes(principal)
	for _, scope := range session.Scopes {
		if !slices.Contains(allowed, scope) {
			_ = s.store.RevokeNativeSession(r.Context(), identity.ID, session.ID, now)
			nativeError(w, http.StatusForbidden, "insufficient_scope")
			return
		}
	}
	token, err := GenerateJWTForPrincipalAndScope(s.options.BaseURL, MCPResource(s.options.BaseURL), "native:"+session.ID, principal, payload.Scope)
	if err != nil {
		_ = s.store.RevokeNativeSession(r.Context(), identity.ID, session.ID, now)
		nativeError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	nativeJSON(w, http.StatusOK, map[string]any{
		"access_token": token, "token_type": "Bearer", "expires_in": int(AccessTokenTTL.Seconds()),
		"scope": payload.Scope, "allowed_scopes": nativeAllowedScopes(principal), "session_id": session.ID,
		"identity": nativeIdentityReply(identity),
		"principal": map[string]any{
			"user_id": principal.UserID, "tenant_id": principal.TenantID, "membership_id": principal.MembershipID,
			"learner_id": principal.LearnerID, "roles": principal.Roles,
		},
	})
}

func (s *NativeIdentityServer) HandleRotate(w http.ResponseWriter, r *http.Request) {
	s.rotate(w, r, "rotate")
}

func (s *NativeIdentityServer) HandleRecover(w http.ResponseWriter, r *http.Request) {
	s.rotate(w, r, "recover")
}

func (s *NativeIdentityServer) rotate(w http.ResponseWriter, r *http.Request, action string) {
	payload, row, ok := s.authorize(w, r, action)
	if !ok {
		return
	}
	identity, err := s.store.RotateNativeIdentity(r.Context(), row.IdentityID, row.KeyVersion, payload.NewPublicKey, payload.NewRecoveryPublicKey, s.now().UTC())
	if err != nil {
		nativeStoreError(w, err)
		return
	}
	if identity == nil || identity.PublicKey != payload.NewPublicKey || identity.RecoveryPublicKey != payload.NewRecoveryPublicKey || identity.Version != row.KeyVersion+1 {
		nativeError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	nativeJSON(w, http.StatusOK, map[string]any{
		"rotated": true, "identity": nativeIdentityReply(identity), "reauthentication_required": true,
	})
}

func (s *NativeIdentityServer) HandleLogout(w http.ResponseWriter, r *http.Request) {
	payload, row, ok := s.authorize(w, r, "logout")
	if !ok {
		return
	}
	if err := s.store.RevokeNativeSession(r.Context(), row.IdentityID, payload.SessionID, s.now().UTC()); err != nil {
		nativeStoreError(w, err)
		return
	}
	nativeJSON(w, http.StatusOK, map[string]any{"revoked": true, "session_id": payload.SessionID})
}

func (s *NativeIdentityServer) authorize(w http.ResponseWriter, r *http.Request, action string) (nativeSignedPayload, *models.NativeChallenge, bool) {
	var payload nativeSignedPayload
	raw, ok := nativeReadBody(w, r, &payload)
	if !ok {
		return payload, nil, false
	}
	canonicalScope, err := models.CanonicalOAuthScope(payload.Scope)
	if payload.Action != action || payload.TenantID != s.options.InstitutionID || !nativeNonce(payload.Challenge) || payload.KeyVersion < 1 ||
		!nativePayloadFields(action, payload.PublicKey, payload.NewPublicKey, payload.NewRecoveryPublicKey, payload.SessionID) ||
		err != nil || canonicalScope != payload.Scope || canonicalScope == models.OAuthScopeLearner ||
		!strings.EqualFold(r.Host, s.origin.Host) || r.URL.RawQuery != "" ||
		s.origin.Scheme+"://"+s.origin.Host+r.URL.EscapedPath() != s.endpoint(action) {
		nativeError(w, http.StatusUnauthorized, "invalid_signature")
		return payload, nil, false
	}
	hash := sha256.Sum256(raw)
	payloadHash := hex.EncodeToString(hash[:])
	tags := map[string]string{
		"u": s.endpoint(action), "method": http.MethodPost, "payload": payloadHash, "challenge": payload.Challenge,
		"tenant": payload.TenantID, "action": action, "scope": payload.Scope, "version": strconv.FormatInt(payload.KeyVersion, 10),
	}
	now := s.now().UTC()
	if len(r.Header.Values("Authorization")) != 1 || verifyNativeNostr(r.Header.Get("Authorization"), true, payload.PublicKey, tags, now) != nil {
		nativeError(w, http.StatusUnauthorized, "invalid_signature")
		return payload, nil, false
	}
	if action == "rotate" || action == "recover" {
		for header, key := range map[string]string{
			"X-Tutor-New-Key-Proof": payload.NewPublicKey, "X-Tutor-New-Recovery-Proof": payload.NewRecoveryPublicKey,
		} {
			if len(r.Header.Values(header)) != 1 || verifyNativeNostr(r.Header.Get(header), false, key, tags, now) != nil {
				nativeError(w, http.StatusUnauthorized, "invalid_signature")
				return payload, nil, false
			}
		}
	}
	if action == "rotate" {
		// An online-key thief must not be able to replace the offline recovery
		// credential. Rotation therefore also needs the CURRENT recovery key's
		// approval; recovery already proves that key in Authorization.
		identity, err := s.store.GetNativeIdentity(r.Context(), payload.TenantID, payload.PublicKey, "rotate")
		if err != nil {
			nativeStoreError(w, err)
			return payload, nil, false
		}
		const header = "X-Tutor-Recovery-Approval"
		if !nativeIdentityMatches(identity, payload.TenantID, payload.PublicKey, "rotate") || identity.Version != payload.KeyVersion || len(r.Header.Values(header)) != 1 ||
			verifyNativeNostr(r.Header.Get(header), false, identity.RecoveryPublicKey, tags, now) != nil {
			nativeError(w, http.StatusUnauthorized, "invalid_signature")
			return payload, nil, false
		}
	}
	row, err := s.store.ConsumeNativeChallenge(r.Context(), nativeTokenHash(payload.Challenge), now)
	if err != nil {
		nativeStoreError(w, err)
		return payload, nil, false
	}
	if row == nil || row.TokenHash != nativeTokenHash(payload.Challenge) || row.ConsumedAt == nil || row.Action != action || row.TenantID != payload.TenantID || row.PublicKey != payload.PublicKey ||
		row.KeyVersion != payload.KeyVersion || row.URL != tags["u"] || row.Method != http.MethodPost ||
		row.PayloadHash != payloadHash || strings.Join(row.Scopes, " ") != payload.Scope ||
		!row.ExpiresAt.After(now) || !row.ExpiresAt.After(row.CreatedAt) || row.CreatedAt.After(now.Add(30*time.Second)) || row.ExpiresAt.Sub(row.CreatedAt) > NativeChallengeTTL {
		nativeError(w, http.StatusUnauthorized, "invalid_challenge")
		return payload, nil, false
	}
	return payload, row, true
}

func nativeIdentityReply(identity *models.NativeIdentity) map[string]any {
	return map[string]any{"public_key": identity.PublicKey, "recovery_public_key": identity.RecoveryPublicKey, "key_version": identity.Version}
}

func nativeAllowedScopes(principal models.Principal) []string {
	allowed := []string{}
	if principal.LearnerID != "" && principal.Authorize(models.PermissionLearningSelf, models.AuthorizationResource{TenantID: principal.TenantID, OwnerUserID: principal.UserID}) {
		allowed = append(allowed, models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite)
	}
	if principal.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: principal.TenantID}) {
		allowed = append(allowed, models.OAuthScopeFormationRead, models.OAuthScopeFormationWrite)
	}
	if principal.CanReadInstitutionProgress() {
		allowed = append(allowed, models.OAuthScopeProgressRead)
	}
	return allowed
}

func nativeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storeport.ErrNativeKeyConflict):
		nativeError(w, http.StatusConflict, "key_conflict")
	case errors.Is(err, storeport.ErrInvalidOAuthScope):
		nativeError(w, http.StatusForbidden, "insufficient_scope")
	case errors.Is(err, storeport.ErrInvalidNativeChallenge):
		nativeError(w, http.StatusUnauthorized, "invalid_challenge")
	case errors.Is(err, storeport.ErrInvalidNativeSession):
		nativeError(w, http.StatusUnauthorized, "invalid_session")
	case errors.Is(err, storeport.ErrInvalidNativeIdentity), errors.Is(err, storeport.ErrInvalidPrincipal), errors.Is(err, sql.ErrNoRows):
		nativeError(w, http.StatusUnauthorized, "invalid_identity")
	default:
		nativeError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
	}
}

func nativeError(w http.ResponseWriter, status int, code string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Nostr realm="tutor-institution"`)
	}
	nativeJSON(w, status, map[string]string{"error": code})
}

func nativeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type NativePrincipalValidator interface {
	PrincipalValidator
	ValidateNativeSession(context.Context, string, models.Principal, time.Time) error
}

// BearerMiddlewareWithNativePrincipalValidator admits only durable native
// sessions. Legacy browser, OAuth and machine JWTs cannot authenticate an
// institution configured for keys, even if their signatures remain valid.
func BearerMiddlewareWithNativePrincipalValidator(baseURL, initialScope string, validator NativePrincipalValidator, next http.Handler) http.Handler {
	if validator == nil {
		panic("auth: nil native principal validator")
	}
	protected := BearerMiddlewareWithPrincipalValidator(baseURL, initialScope, validator, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deny := func() {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+baseURL+`/.well-known/oauth-protected-resource"`)
			appendScopeToBearerChallenge(w.Header(), initialScope)
			nativeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		}
		scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || len(r.Header.Values("Authorization")) != 1 {
			deny()
			return
		}
		claims, err := VerifyJWTClaims(raw, baseURL)
		if err != nil || !strings.HasPrefix(claims.AuthorizedParty, "native:") || !nativeID(strings.TrimPrefix(claims.AuthorizedParty, "native:")) {
			deny()
			return
		}
		principal, err := claims.Principal()
		if err != nil || validator.ValidateNativeSession(r.Context(), strings.TrimPrefix(claims.AuthorizedParty, "native:"), principal, time.Now().UTC()) != nil {
			deny()
			return
		}
		protected.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nativeClientContextKey{}, true)))
	})
}
