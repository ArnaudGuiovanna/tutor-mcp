// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// NormalizeEmail folds an email address to a canonical form (lowercase +
// trimmed). R002: SQLite's default BINARY collation treats Alice@x.com and
// alice@x.com as different keys, which lets an attacker rotate case to bypass
// the per-account failure-tracker lockout and lets registration create
// duplicate accounts whose only difference is letter case. Every handler that
// reads `email` from a form/body must call NormalizeEmail before any lookup,
// failure-bucket write, or learner insert.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// sanitizeAuthLogField removes record delimiters before a value reaches any
// logger. Structured handlers should escape them too, but this keeps embedded
// OAuthServer users safe even when they supply a plain-text handler.
func sanitizeAuthLogField(value string) string {
	value = strings.ReplaceAll(value, "\n", "")
	return strings.ReplaceAll(value, "\r", "")
}

func authLogIdentifier(value string) string {
	clean := sanitizeAuthLogField(value)
	if clean == "" {
		return "[empty]"
	}
	digest := sha256.Sum256([]byte(clean))
	return fmt.Sprintf("sha256:%x", digest[:8])
}

func authLogErrorType(err error) string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", err)
}

// bcryptCost is the work factor used for password and client_secret hashes.
// Bumped from DefaultCost (10) to 12 in 2026-05 (issue #36): at cost 12 a
// single hash takes ~250 ms on commodity hardware, raising the cost of an
// offline brute-force on a leaked SQLite file by 4×. Hashes carry their cost
// in the encoded string, so existing accounts keep working without migration.
const bcryptCost = 12

// passwordMinLen is the minimum length enforced when the mailbox holder
// activates or resets an account. Bcrypt silently truncates to 72 bytes, hence
// passwordMaxLen.
const (
	passwordMinLen = 12
	passwordMaxLen = 72
)

// A real cost-12 bcrypt digest used when an email is unknown. Performing the
// same password work for existing and nonexistent accounts prevents a remote
// timing oracle without ever accepting the dummy credential.
var dummyPasswordHash = []byte("$2a$12$Cfv5jtGrBAIaZzVQI1XKyeTp9.kxPz23zkFOYmZjLKp3L1qGdvb0a")

// The same syntactically valid digest with a minimum work factor is used only
// as timing padding. Its checksum need not match: bcrypt still performs the
// requested work before returning a credential mismatch.
var dummyMinCostPasswordHash = []byte("$2a$04$Cfv5jtGrBAIaZzVQI1XKyeTp9.kxPz23zkFOYmZjLKp3L1qGdvb0a")

const (
	authorizeBodyLimitBytes             int64 = 16 << 10
	tokenBodyLimitBytes                 int64 = 16 << 10
	registerBodyLimitBytes              int64 = 16 << 10
	defaultMaxRegisteredOAuthClients          = 10_000
	defaultDCRInitialTokenRegistrations       = 1_000
	emailMaxLen                               = 254
	// clientNameMaxLen caps the byte-length of an attacker-controlled
	// client_name on /register. The value is echoed on the consent screen
	// and into the registration response; a multi-KB phishing string would
	// otherwise be displayed verbatim. 120 bytes is enough for any real
	// product name while making the consent UI usable.
	clientNameMaxLen = 120
	dynamicClientTTL = 30 * 24 * time.Hour
)

func validateEmail(email string) error {
	if email == "" {
		return fmt.Errorf("email is required")
	}
	if len(email) > emailMaxLen {
		return fmt.Errorf("email must be at most %d bytes", emailMaxLen)
	}
	if !utf8.ValidString(email) || strings.ContainsAny(email, "<>\r\n\t ") {
		return fmt.Errorf("email is invalid")
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || at > 64 || len(email)-at-1 > 253 {
		return fmt.Errorf("email is invalid")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return fmt.Errorf("email is invalid")
	}
	return nil
}

func parseLimitedForm(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if r.ContentLength > limit {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseForm(); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeBcryptBusyJSON(w http.ResponseWriter) {
	w.Header().Set("Retry-After", bcryptBusyRetryAfter)
	writeTokenError(w, "temporarily_unavailable", http.StatusServiceUnavailable)
}

func renderBcryptBusyAuthPage(w http.ResponseWriter, data authPageData, mode string) {
	w.Header().Set("Retry-After", bcryptBusyRetryAfter)
	renderAuthPageStatus(w, http.StatusServiceUnavailable, data,
		"Sign-in is temporarily busy. Please try again shortly.", mode)
}

// oauthStore is the persistence surface the OAuth server needs.
type oauthStore interface {
	storeport.LearnerStore
	storeport.AuthStore
	storeport.IdentityStore
	WithTenantTx(context.Context, models.TenantScope, func(context.Context, storeport.Store) error) error
}

// serviceAccountAuthenticator is deliberately separate from oauthStore so
// alternative OAuth store implementations do not gain an unrelated method.
// A server can advertise and accept client_credentials only when its store
// provides the durable service-account credential boundary.
type serviceAccountAuthenticator interface {
	AuthenticateServiceAccount(context.Context, models.ServiceAccountCredential) (models.Principal, error)
}

type oauthCSRFConsumer interface {
	ConsumeOAuthCSRF(context.Context, models.OAuthCSRFCredential) (bool, error)
}

// OAuthServer implements the OAuth 2.1 authorization server.
type OAuthServer struct {
	store                oauthStore
	baseURL              string
	logger               *slog.Logger
	granularScopes       bool
	emailSender          EmailSender
	loginFailures        *LoginFailureTracker
	bcrypt               *bcryptBudget
	dcrMode              string
	maxRegisteredClients int
	cimdHTTPClient       *http.Client
	cimdMu               sync.Mutex
	cimdCache            map[string]cachedCIMDClient
	csrfMu               sync.Mutex
	usedCSRF             map[string]time.Time
	lastCSRFPrune        time.Time
}

// SetGranularScopesEnabled switches discovery and authorization issuance from
// the bounded legacy `learner` bundle to learner:read / learner:write. It must
// be set during startup, before the server begins handling requests. Runtime
// token verification remains scope-aware in both modes so phase-A nodes can be
// deployed fleet-wide before granular issuance is enabled.
func (s *OAuthServer) SetGranularScopesEnabled(enabled bool) {
	s.granularScopes = enabled
}

// NewOAuthServer creates a new OAuthServer. The login-failure tracker locks
// out an email after 5 password mismatches in 10 minutes (issue #36).
func NewOAuthServer(store oauthStore, baseURL string, logger *slog.Logger) *OAuthServer {
	return &OAuthServer{
		store:                store,
		baseURL:              baseURL,
		logger:               logger,
		loginFailures:        NewLoginFailureTracker(5, 10*time.Minute),
		bcrypt:               currentBcryptBudget(),
		dcrMode:              DCRModeOpen,
		maxRegisteredClients: defaultMaxRegisteredOAuthClients,
		cimdHTTPClient:       newCIMDHTTPClient(),
		cimdCache:            make(map[string]cachedCIMDClient),
		usedCSRF:             make(map[string]time.Time),
	}
}

func setAuthorizeCSRFCookie(w http.ResponseWriter, value string, maxAge int) {
	cookie := &http.Cookie{
		Name:     "csrf_token",
		Value:    value,
		Path:     "/authorize",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
	if maxAge < 0 {
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	http.SetCookie(w, cookie)
}

// consumeCSRF makes a valid double-submit token single-use fleet-wide when the
// durable store implements oauthCSRFConsumer. The bounded local fallback is
// retained only for small embedders and unit-test stores.
func (s *OAuthServer) consumeCSRF(ctx context.Context, token string) bool {
	now := time.Now().UTC()
	if consumer, ok := s.store.(oauthCSRFConsumer); ok {
		consumed, err := consumer.ConsumeOAuthCSRF(ctx, models.OAuthCSRFCredential{
			Token: token, ExpiresAt: now.Add(time.Hour),
		})
		if err != nil {
			s.logger.Error("durable CSRF consumption failed", "error_type", fmt.Sprintf("%T", err))
			return false
		}
		return consumed
	}
	s.csrfMu.Lock()
	defer s.csrfMu.Unlock()
	if s.lastCSRFPrune.IsZero() || now.Sub(s.lastCSRFPrune) >= time.Minute {
		for used, expiresAt := range s.usedCSRF {
			if !expiresAt.After(now) {
				delete(s.usedCSRF, used)
			}
		}
		s.lastCSRFPrune = now
	}
	if _, replayed := s.usedCSRF[token]; replayed {
		return false
	}
	s.usedCSRF[token] = now.Add(time.Hour)
	return true
}

// SetLoginFailureBackend installs a shared, fleet-wide store for per-account
// login-failure tracking. nil restores the in-memory default. Opt-in: existing
// callers that never call this keep the unchanged in-process behaviour.
func (s *OAuthServer) SetLoginFailureBackend(backend LoginFailureBackend) {
	s.loginFailures.SetBackend(backend)
}

func (s *OAuthServer) HandleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	scopesSupported := []string{models.OAuthScopeLearner}
	if s.granularScopes {
		scopesSupported = []string{
			models.OAuthScopeLearnerRead,
			models.OAuthScopeLearnerWrite,
		}
	}
	meta := map[string]interface{}{
		"issuer":                                s.baseURL,
		"authorization_endpoint":                s.baseURL + "/authorize",
		"token_endpoint":                        s.baseURL + "/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", "client_credentials"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      scopesSupported,
		"client_id_metadata_document_supported": true,
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		// RFC 9207: we include iss in authorization responses.
		"authorization_response_iss_parameter_supported": true,
		"jwks_uri": s.baseURL + "/.well-known/jwks.json",
	}
	if s.dynamicClientRegistrationEnabled() {
		meta["registration_endpoint"] = s.baseURL + "/register"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(meta)
}

func (s *OAuthServer) HandleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	// MCP clients request every protected-resource scope when the initial 401
	// challenge has no scope hint. Advertise only the least-privilege bootstrap
	// scope here; write access is acquired per tool through step-up challenges.
	scopesSupported := []string{models.OAuthScopeLearner}
	if s.granularScopes {
		scopesSupported = []string{models.OAuthScopeLearnerRead}
	}
	meta := map[string]interface{}{
		"resource":              s.baseURL + "/mcp",
		"authorization_servers": []string{s.baseURL},
		"scopes_supported":      scopesSupported,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(meta)
}

// validateRedirectURI returns the server-side registered URI that is strictly
// equal to the supplied redirectURI. Callers must use the returned value for
// redirects and persisted grants so request data never reaches a redirect sink.
func (s *OAuthServer) validateRedirectURI(ctx context.Context, clientID, redirectURI string) (string, error) {
	if clientID == "" || redirectURI == "" {
		return "", fmt.Errorf("missing client_id or redirect_uri")
	}
	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		return "", fmt.Errorf("unknown client")
	}
	var registered []string
	if err := json.Unmarshal([]byte(client.RedirectURIs), &registered); err != nil {
		return "", fmt.Errorf("malformed registration")
	}
	for _, u := range registered {
		if u == redirectURI {
			return u, nil
		}
	}
	return "", fmt.Errorf("redirect_uri not registered")
}

func validatedAuthorizationScope(responseType, scope string, granularEnabled bool) (string, error) {
	if responseType != "code" {
		return "", fmt.Errorf("unsupported response_type")
	}
	canonical, err := normalizeAuthorizationScope(scope, granularEnabled)
	if err != nil {
		return "", fmt.Errorf("unsupported scope: %w", err)
	}
	return canonical, nil
}

func (s *OAuthServer) HandleAuthorizeGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	resource := q.Get("resource")
	if resource != MCPResource(s.baseURL) {
		s.logger.Debug("authorize GET: resource rejected")
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	registeredRedirectURI, err := s.validateRedirectURI(ctx, clientID, redirectURI)
	if err != nil {
		s.logger.Debug("authorize GET: redirect_uri rejected", "error_type", authLogErrorType(err), "client_id", authLogIdentifier(clientID))
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	redirectURI = registeredRedirectURI
	scope, err := validatedAuthorizationScope(q.Get("response_type"), q.Get("scope"), s.granularScopes)
	if err != nil {
		s.logger.Debug("authorize GET: parameters rejected", "client_id", authLogIdentifier(clientID))
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		s.logger.Debug("authorize GET: client lookup failed", "error_type", authLogErrorType(err), "client_id", authLogIdentifier(clientID))
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")
	if err := s.requirePKCEForPublicClient(ctx, clientID, codeChallenge, codeChallengeMethod); err != nil {
		s.logger.Debug("authorize GET: PKCE missing for public client", "error_type", authLogErrorType(err), "client_id", authLogIdentifier(clientID))
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	s.logger.Info("authorize GET", "client_id", authLogIdentifier(clientID), "state_len", len(q.Get("state")))

	csrfToken, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setAuthorizeCSRFCookie(w, csrfToken, 3600)

	data := authPageData{
		ClientID:            clientID,
		ClientName:          client.ClientName,
		RedirectURI:         redirectURI,
		ResponseType:        q.Get("response_type"),
		State:               q.Get("state"),
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		Scope:               scope,
		Resource:            resource,
		CSRFToken:           csrfToken,
	}
	renderAuthPage(w, data, "", "login")
}

// requirePKCEForPublicClient enforces RFC 9700 §2.1.1: public clients
// (no stored secret) MUST use PKCE with S256. Confidential clients are
// still allowed to skip PKCE — they authenticate via client_secret.
func (s *OAuthServer) requirePKCEForPublicClient(ctx context.Context, clientID, codeChallenge, method string) error {
	if clientID == "" {
		return fmt.Errorf("missing client_id")
	}
	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		return fmt.Errorf("unknown client")
	}
	if codeChallenge == "" {
		if method != "" {
			return fmt.Errorf("code_challenge_method requires code_challenge")
		}
		if client.ClientSecretHash != "" {
			// Confidential client: PKCE optional.
			return nil
		}
		return fmt.Errorf("code_challenge required for public clients")
	}
	if method != "S256" {
		return fmt.Errorf("code_challenge_method must be S256")
	}
	return nil
}

func (s *OAuthServer) HandleAuthorizePost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !parseLimitedForm(w, r, authorizeBodyLimitBytes) {
		return
	}

	cookie, cerr := r.Cookie("csrf_token")
	formCSRF := r.FormValue("csrf_token")
	if cerr != nil || cookie.Value == "" || formCSRF == "" ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formCSRF)) != 1 {
		http.Error(w, "forbidden: csrf check failed", http.StatusForbidden)
		return
	}
	if !s.consumeCSRF(r.Context(), formCSRF) {
		http.Error(w, "forbidden: csrf token already used", http.StatusForbidden)
		return
	}
	// Every accepted POST gets a fresh retry token before any credential or
	// consent validation. Error pages embed it and the browser receives the
	// matching cookie; successful redirects clear it below.
	nextCSRF, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	formCSRF = nextCSRF
	setAuthorizeCSRFCookie(w, nextCSRF, 3600)

	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	resource := r.FormValue("resource")
	if resource != MCPResource(s.baseURL) {
		s.logger.Debug("authorize POST: resource rejected")
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	registeredRedirectURI, err := s.validateRedirectURI(ctx, clientID, redirectURI)
	if err != nil {
		s.logger.Debug("authorize POST: redirect_uri rejected", "error_type", authLogErrorType(err), "client_id", authLogIdentifier(clientID))
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	redirectURI = registeredRedirectURI
	scope, err := validatedAuthorizationScope(r.FormValue("response_type"), r.FormValue("scope"), s.granularScopes)
	if err != nil {
		s.logger.Debug("authorize POST: parameters rejected", "client_id", authLogIdentifier(clientID))
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		s.logger.Debug("authorize POST: client lookup failed", "error_type", authLogErrorType(err), "client_id", authLogIdentifier(clientID))
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	codeChallenge := r.FormValue("code_challenge")
	codeChallengeMethod := r.FormValue("code_challenge_method")
	if err := s.requirePKCEForPublicClient(ctx, clientID, codeChallenge, codeChallengeMethod); err != nil {
		s.logger.Debug("authorize POST: PKCE missing for public client", "error_type", authLogErrorType(err), "client_id", authLogIdentifier(clientID))
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	mode := r.FormValue("mode") // "login" or "register"
	// R002: fold case + trim whitespace before any lookup or failure
	// tracker call. Keeps both buckets and store rows in a single
	// canonical form.
	email := NormalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")

	state := r.FormValue("state")

	data := authPageData{
		ClientID:            clientID,
		ClientName:          client.ClientName,
		RedirectURI:         redirectURI,
		ResponseType:        r.FormValue("response_type"),
		State:               state,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		Scope:               scope,
		Resource:            resource,
		CSRFToken:           formCSRF,
	}

	if email == "" {
		renderAuthPage(w, data, "Email is required.", mode)
		return
	}
	if mode != "register" && password == "" {
		renderAuthPage(w, data, "Email and password are required.", mode)
		return
	}
	if err := validateEmail(email); err != nil {
		renderAuthPage(w, data, "Please enter a valid email address.", mode)
		return
	}

	var selectedPrincipal models.Principal

	if mode == "register" {
		// Registration creates only an inactive placeholder credential. The
		// mailbox holder chooses the first usable password on /verify-email and
		// approves the exact client there. Never persist a password or consent
		// supplied by the unauthenticated initiator: doing so lets an attacker
		// pre-register a victim and take the account over when the victim merely
		// confirms the mailbox link.
		pendingCredential, err := generateCode()
		if err != nil {
			s.logger.Error("generate pending credential failed", "err", err)
			renderAuthPage(w, data, "Internal error. Please try again.", "register")
			return
		}
		pendingHash, err := s.bcrypt.Generate(ctx, []byte(pendingCredential), bcryptCost)
		if err != nil {
			if errors.Is(err, ErrBcryptBusy) {
				renderBcryptBusyAuthPage(w, data, "register")
				return
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Error("bcrypt pending credential failed", "err", err)
			renderAuthPage(w, data, "Internal error. Please try again.", "register")
			return
		}

		// Check if email already exists. A database failure is not equivalent
		// to an available address; continuing could create a duplicate account
		// or expose backend-dependent behavior.
		existingUser, lookupErr := s.store.GetLocalUserByEmail(ctx, email)
		if lookupErr == nil && existingUser != nil {
			// A fresh request for an inactive placeholder replaces the prior OAuth
			// continuation. This lets the mailbox owner recover from an attacker-
			// initiated pending registration without revealing account state.
			if existingUser.EmailVerifiedAt == nil {
				memberships, membershipErr := s.store.ListMembershipsForUser(ctx, existingUser.ID)
				if membershipErr == nil && len(memberships) == 1 && memberships[0].LearnerID != "" {
					membership := memberships[0]
					scope := models.TenantScope{
						TenantID: membership.TenantID, UserID: membership.UserID,
						MembershipID: membership.ID, LearnerID: membership.LearnerID,
					}
					_ = s.store.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
						learner, learnerErr := scoped.GetLearnerByID(txCtx, membership.LearnerID)
						if learnerErr != nil {
							return learnerErr
						}
						return s.sendEmailVerification(txCtx, learner, data)
					})
				}
			}
			s.renderVerificationPending(w)
			return
		}
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) && !errors.Is(lookupErr, storeport.ErrAmbiguousIdentity) {
			s.logger.Error("registration learner lookup failed", "err", lookupErr)
			renderAuthPage(w, data, "Internal error. Please try again.", "register")
			return
		}
		learner, err := s.store.CreateUnverifiedLearner(ctx, email, string(pendingHash), "", "")
		if err != nil {
			s.logger.Error("create learner failed", "err", err)
			renderAuthPage(w, data, "Could not create account. Please try again.", "register")
			return
		}
		if err := s.sendEmailVerification(ctx, learner, data); err != nil {
			s.logger.Error("registration verification delivery failed", "err", err)
		}
		s.renderVerificationPending(w)
		return
	} else {
		// Login flow.
		// Always perform exactly one budgeted bcrypt comparison before applying
		// any account penalty. Unknown accounts use the current cost-12 dummy;
		// inactive accounts use their stored hash but remain indistinguishable
		// from absent/wrong credentials. Most importantly, a correct active
		// credential is never refused because an attacker filled its counter.
		globalUser, lookupErr := s.store.GetLocalUserByEmail(ctx, email)
		passwordHash := dummyPasswordHash
		if lookupErr == nil && globalUser != nil {
			passwordHash = []byte(globalUser.PasswordHash)
		}
		passwordErr := s.bcrypt.CompareCredential(
			ctx, passwordHash, []byte(password),
			dummyPasswordHash, dummyMinCostPasswordHash, bcryptCost,
		)
		if errors.Is(passwordErr, ErrBcryptBusy) {
			renderBcryptBusyAuthPage(w, data, "login")
			return
		}
		if ctx.Err() != nil {
			return
		}
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			s.logger.Error("login learner lookup failed", "err", lookupErr)
			renderAuthPage(w, data, "Internal error. Please try again.", "login")
			return
		}
		if lookupErr != nil || globalUser == nil || passwordErr != nil || globalUser.EmailVerifiedAt == nil || globalUser.Status != models.UserStatusActive {
			count := s.loginFailures.RecordContext(ctx, email)
			status := http.StatusUnauthorized
			if retryAfter := s.loginFailures.RetryAfter(count); retryAfter > 0 {
				status = http.StatusTooManyRequests
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter/time.Second)))
				if count == s.loginFailures.Threshold() {
					s.logger.Warn("login failure threshold reached; correct credentials remain allowed")
				}
			}
			renderAuthPageStatus(w, status, data, "Invalid email or password.", "login")
			return
		}
		if !s.loginFailures.AllowContext(ctx, email) {
			memberships, membershipErr := s.store.ListActiveMembershipsForUser(ctx, globalUser.ID)
			if membershipErr != nil || len(memberships) != 1 {
				renderAuthPage(w, data, "Choose an organization after signing in.", "login")
				return
			}
			trusted, trustErr := s.isTrustedLoginDevice(ctx, r, memberships[0].LearnerID)
			if trustErr != nil {
				s.logger.Error("trusted login device lookup failed", "error_type", fmt.Sprintf("%T", trustErr))
				renderAuthPage(w, data, "Internal error. Please try again.", "login")
				return
			}
			if !trusted {
				adaptiveScope := models.TenantScope{
					TenantID: memberships[0].TenantID, UserID: memberships[0].UserID,
					MembershipID: memberships[0].ID, LearnerID: memberships[0].LearnerID,
				}
				var learner *models.Learner
				learnerErr := s.store.WithTenantTx(ctx, adaptiveScope, func(txCtx context.Context, scoped storeport.Store) error {
					var loadErr error
					learner, loadErr = scoped.GetLearnerByID(txCtx, memberships[0].LearnerID)
					return loadErr
				})
				if learnerErr != nil {
					renderAuthPage(w, data, "Internal error. Please try again.", "login")
					return
				}
				if _, challengeErr := s.sendLoginChallenge(ctx, learner, data); challengeErr != nil {
					s.logger.Error("adaptive login challenge failed", "error_type", fmt.Sprintf("%T", challengeErr))
					renderAuthPageStatus(w, http.StatusServiceUnavailable, data, "Additional sign-in verification is temporarily unavailable.", "login")
					return
				}
				renderAuthPageStatus(w, http.StatusAccepted, data, "Check your email to approve this device before signing in.", "login")
				return
			}
		}
		s.loginFailures.ResetContext(ctx, email)
		memberships, membershipErr := s.store.ListActiveMembershipsForUser(ctx, globalUser.ID)
		if membershipErr != nil || len(memberships) == 0 {
			renderAuthPage(w, data, "No active organization membership is available.", "login")
			return
		}
		selectedTenant := strings.TrimSpace(r.FormValue("tenant_id"))
		if len(memberships) > 1 && selectedTenant == "" {
			data.TenantOptions = make([]tenantOption, 0, len(memberships))
			for _, membership := range memberships {
				data.TenantOptions = append(data.TenantOptions, tenantOption{ID: membership.TenantID, Name: membership.TenantName})
			}
			renderAuthPage(w, data, "Choose the organization for this session.", "login")
			return
		}
		var selected models.TenantMembership
		for _, membership := range memberships {
			if (len(memberships) == 1 && selectedTenant == "") || membership.TenantID == selectedTenant {
				selected = membership
				break
			}
		}
		if selected.ID == "" || selected.LearnerID == "" {
			renderAuthPage(w, data, "The selected organization is not available.", "login")
			return
		}
		tenantScope := models.TenantScope{
			TenantID: selected.TenantID, UserID: selected.UserID,
			MembershipID: selected.ID, LearnerID: selected.LearnerID,
		}
		// R001: if the learner has already approved this client+redirect_uri,
		// the approval screen is no longer meaningful — skip it. Re-prompting
		// every time trained users to click through reflexively, defeating the
		// trust-on-first-use guarantee against a malicious dynamic-client
		// registration. The approval is scoped to redirect_uri so a phishing
		// client cannot reuse a previously-granted consent at a different URL.
		approved := false
		if !isCIMDClientID(clientID) {
			err = s.store.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped storeport.Store) error {
				var approvalErr error
				approved, approvalErr = scoped.IsClientApprovedForScope(txCtx, selected.LearnerID, clientID, redirectURI, scope)
				return approvalErr
			})
			if err != nil {
				s.logger.Error("client approval lookup failed", "error_type", authLogErrorType(err), "learner", authLogIdentifier(selected.LearnerID), "client", authLogIdentifier(clientID))
				renderAuthPage(w, data, "Internal error. Please try again.", "login")
				return
			}
		}
		if !approved && r.FormValue("approve_client") != "yes" {
			renderAuthPage(w, data, "Please confirm that you recognize and approve this OAuth client before continuing.", "login")
			return
		}
		if !approved && !isCIMDClientID(clientID) {
			if err := s.store.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped storeport.Store) error {
				return scoped.ApproveClientForScope(txCtx, selected.LearnerID, clientID, redirectURI, scope)
			}); err != nil {
				s.logger.Warn("persist client approval failed", "error_type", authLogErrorType(err), "learner", authLogIdentifier(selected.LearnerID), "client", authLogIdentifier(clientID))
			}
		}
		selectedPrincipal = models.Principal{
			UserID: selected.UserID, TenantID: selected.TenantID,
			MembershipID: selected.ID, LearnerID: selected.LearnerID,
			Roles: append([]string(nil), selected.Roles...), Scopes: strings.Fields(scope),
			TokenVersion: selected.Version,
		}
		if err := selectedPrincipal.Validate(); err != nil {
			renderAuthPage(w, data, "The selected organization is not available.", "login")
			return
		}
	}

	// Generate auth code
	code, err := generateCode()
	if err != nil {
		s.logger.Error("generate code failed", "err", err)
		renderAuthPage(w, data, "Internal error. Please try again.", mode)
		return
	}

	createCodeErr := s.store.CreateAuthCodeForPrincipal(
		ctx, code, selectedPrincipal, codeChallenge, codeChallengeMethod,
		clientID, redirectURI, resource, scope, time.Now().Add(5*time.Minute),
	)
	if createCodeErr != nil {
		s.logger.Error("create auth code failed", "err", createCodeErr)
		renderAuthPage(w, data, "Internal error. Please try again.", mode)
		return
	}

	u, err := url.Parse(redirectURI)
	if err != nil {
		s.logger.Error("parse redirect_uri failed", "err", err)
		renderAuthPage(w, data, "Internal error. Please try again.", mode)
		return
	}
	qv := u.Query()
	qv.Set("code", code)
	if state != "" {
		qv.Set("state", state)
	}
	// RFC 9207 mix-up mitigation: clients (e.g. Mistral) may require iss in the callback.
	qv.Set("iss", s.baseURL)
	u.RawQuery = qv.Encode()
	s.logger.Info("authorize POST redirect", "state_len", len(state), "redirect_host", authLogIdentifier(u.Host))
	setAuthorizeCSRFCookie(w, "", -1)
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// extractClientCredentials returns (client_id, client_secret) using HTTP Basic
// (client_secret_basic) first, then falling back to form fields (client_secret_post).
// Either or both may be empty if not supplied.
func extractClientCredentials(r *http.Request) (string, string) {
	if id, secret, ok := r.BasicAuth(); ok {
		return id, secret
	}
	return r.FormValue("client_id"), r.FormValue("client_secret")
}

// verifyClientAuthWithBudget enforces secret-based authentication for
// confidential clients. Public clients (empty stored hash) pass through and
// rely on PKCE. Every handler comparison shares the process-wide CPU budget.
func verifyClientAuthWithBudget(ctx context.Context, budget *bcryptBudget, client *models.OAuthClient, suppliedSecret string) error {
	if client.ClientSecretHash == "" {
		return nil
	}
	if suppliedSecret == "" {
		return fmt.Errorf("invalid_client")
	}
	if err := budget.Compare(ctx, []byte(client.ClientSecretHash), []byte(suppliedSecret)); err != nil {
		if errors.Is(err, ErrBcryptBusy) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("invalid_client")
	}
	return nil
}

func (s *OAuthServer) HandleToken(w http.ResponseWriter, r *http.Request) {
	if !parseLimitedForm(w, r, tokenBodyLimitBytes) {
		return
	}

	grantType := r.FormValue("grant_type")

	switch grantType {
	case "authorization_code":
		s.handleAuthorizationCodeGrant(w, r)
	case "refresh_token":
		s.handleRefreshTokenGrant(w, r)
	case "client_credentials":
		s.handleClientCredentialsGrant(w, r)
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
	}
}

func (s *OAuthServer) handleClientCredentialsGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resource := r.FormValue("resource")
	clientID, clientSecret := extractClientCredentials(r)
	if clientID == "" || clientSecret == "" || resource == "" {
		writeTokenError(w, "invalid_request", http.StatusBadRequest)
		return
	}
	if resource != MCPResource(s.baseURL) {
		writeTokenError(w, "invalid_target", http.StatusBadRequest)
		return
	}
	authenticator, ok := s.store.(serviceAccountAuthenticator)
	if !ok {
		s.logger.Error("client credentials store is unavailable")
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}
	principal, err := authenticator.AuthenticateServiceAccount(ctx, models.ServiceAccountCredential{
		ClientID: clientID,
		Secret:   clientSecret,
	})
	if err != nil {
		s.logger.Debug("client credentials authentication failed", "client_id", authLogIdentifier(clientID))
		writeTokenError(w, "invalid_client", http.StatusUnauthorized)
		return
	}
	grantedScope := strings.Join(principal.Scopes, " ")
	requestedScope := r.FormValue("scope")
	if requestedScope != "" {
		canonical, err := models.CanonicalOAuthScope(requestedScope)
		if err != nil || canonical != grantedScope {
			writeTokenError(w, "invalid_scope", http.StatusBadRequest)
			return
		}
	}
	accessToken, err := GenerateJWTForPrincipalAndScope(
		s.baseURL, resource, clientID, principal, grantedScope,
	)
	if err != nil {
		s.logger.Error("generate service-account jwt failed", "err", err)
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}
	writeAccessTokenResponse(w, accessToken, grantedScope)
}

func (s *OAuthServer) handleAuthorizationCodeGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.FormValue("code")
	codeVerifier := r.FormValue("code_verifier")
	redirectURI := r.FormValue("redirect_uri")
	resource := r.FormValue("resource")
	clientID, clientSecret := extractClientCredentials(r)

	s.logger.Debug("token exchange attempt", "code_len", len(code), "verifier_len", len(codeVerifier), "client_id", authLogIdentifier(clientID))

	// Note: code_verifier is *conditionally* required (issue #114). PKCE is
	// only enforced when the auth code was minted with a non-empty challenge,
	// which happens for public clients (always) and for confidential clients
	// that opted in. We therefore validate code_verifier presence later, after
	// loading the auth code, instead of rejecting up-front.
	if code == "" || clientID == "" || resource == "" {
		s.logger.Debug("token exchange: missing code or client_id")
		writeTokenError(w, "invalid_request", http.StatusBadRequest)
		return
	}

	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		s.logger.Debug("token exchange: unknown client", "client_id", authLogIdentifier(clientID))
		writeTokenError(w, "invalid_client", http.StatusUnauthorized)
		return
	}
	if err := verifyClientAuthWithBudget(ctx, s.bcrypt, client, clientSecret); err != nil {
		if errors.Is(err, ErrBcryptBusy) {
			writeBcryptBusyJSON(w)
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.logger.Debug("token exchange: client auth failed", "client_id", authLogIdentifier(clientID))
		writeTokenError(w, "invalid_client", http.StatusUnauthorized)
		return
	}

	authCode, err := s.store.GetAuthCode(ctx, code, clientID)
	if err != nil {
		s.logger.Debug("token exchange: code not found or expired", "err", err)
		writeTokenError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	if resource != MCPResource(s.baseURL) || authCode.Resource == "" || resource != authCode.Resource {
		s.logger.Debug("token exchange: resource mismatch", "client_id", authLogIdentifier(clientID))
		writeTokenError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	// RFC 6749 §4.1.3: when redirect_uri was present in the authorization
	// request, the token request must carry the identical value. Empty binding
	// denotes a pre-upgrade code and is accepted once for compatibility.
	if authCode.RedirectURI != "" && redirectURI != authCode.RedirectURI {
		s.logger.Debug("token exchange: redirect_uri mismatch", "client_id", authLogIdentifier(clientID))
		writeTokenError(w, "invalid_grant", http.StatusBadRequest)
		return
	}

	// Verify PKCE only when the auth code was minted with a challenge
	// (issue #114). Confidential clients legitimately skip PKCE at /authorize
	// and authenticate with their client_secret instead, so requiring a
	// verifier here would lock them out. When a challenge *was* recorded, the
	// verifier must be supplied AND match. A public client (no stored secret)
	// MUST always have a non-empty challenge: an empty challenge for a public
	// client indicates a malformed state, and we refuse to issue a token —
	// otherwise a leaked public-client auth code could be redeemed without any
	// verifier (defense-in-depth; the /authorize gate already prevents this).
	if authCode.CodeChallenge == "" {
		if client.ClientSecretHash == "" {
			s.logger.Warn("token exchange: empty PKCE challenge for public client — refusing", "client_id", authLogIdentifier(clientID))
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		if codeVerifier != "" {
			// RFC 9700 §2.1.1 downgrade guard: a verifier cannot appear at
			// the token endpoint unless its challenge was bound to the code.
			s.logger.Debug("token exchange: unexpected code_verifier without challenge", "client_id", authLogIdentifier(clientID))
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		// Confidential client that opted out of PKCE — accept. Its identity
		// was already verified by verifyClientAuth above.
	} else {
		method := authCode.CodeChallengeMethod
		if method == "" {
			// Codes created before method persistence were S256 in every
			// supported public-client flow.
			method = "S256"
		}
		if method != "S256" {
			s.logger.Warn("token exchange: unsupported stored PKCE method", "client_id", authLogIdentifier(clientID), "method", sanitizeAuthLogField(method))
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		if codeVerifier == "" {
			s.logger.Debug("token exchange: code_verifier required but missing")
			writeTokenError(w, "invalid_request", http.StatusBadRequest)
			return
		}
		h := sha256.Sum256([]byte(codeVerifier))
		computed := base64.RawURLEncoding.EncodeToString(h[:])
		if subtle.ConstantTimeCompare([]byte(computed), []byte(authCode.CodeChallenge)) != 1 {
			s.logger.Debug("token exchange: PKCE mismatch")
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
	}

	// Resolve the currently active tenant membership immediately before signing.
	// A suspended membership or stale authorization state cannot be converted
	// into a bearer, even if its short-lived code has not expired yet.
	principal, err := s.store.GetPrincipalForLearner(ctx, authCode.LearnerID, strings.Fields(authCode.Scope))
	if err != nil {
		s.logger.Debug("token exchange: tenant membership inactive", "err", err)
		writeTokenError(w, "invalid_grant", http.StatusBadRequest)
		return
	}

	// Sign before mutating persistence. Configuration/signing failures must not
	// consume a valid one-time code.
	accessToken, err := GenerateJWTForPrincipalAndScope(s.baseURL, resource, clientID, principal, authCode.Scope)
	if err != nil {
		s.logger.Error("generate jwt failed", "err", err)
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}

	// Consume the code and bind the refresh token atomically. Concurrent valid
	// exchanges still have one winner; an insert failure rolls the consume back.
	_, rt, err := s.store.ExchangeAuthCodeForRefreshToken(ctx, code, clientID)
	if err != nil {
		if errors.Is(err, storeport.ErrInvalidAuthCode) {
			s.logger.Debug("token exchange: code already consumed", "err", err)
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		s.logger.Error("token exchange: create refresh credential", "err", err)
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}

	if rt.Scope != authCode.Scope {
		s.logger.Error("token exchange: persisted scope mismatch")
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}

	writeTokenResponse(w, accessToken, rt.Token, rt.Scope)
}

func (s *OAuthServer) handleRefreshTokenGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	refreshToken := r.FormValue("refresh_token")
	resource := r.FormValue("resource")
	if refreshToken == "" || resource == "" {
		writeTokenError(w, "invalid_request", http.StatusBadRequest)
		return
	}
	if resource != MCPResource(s.baseURL) {
		writeTokenError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	requestedScope, err := normalizeRefreshScope(r.FormValue("scope"), s.granularScopes)
	if err != nil {
		writeTokenError(w, "invalid_scope", http.StatusBadRequest)
		return
	}

	// Per RFC 6749 §6 the client must authenticate on every refresh_token
	// grant. The previous "if clientID != ''" bypass let a stolen refresh
	// token be redeemed by any anonymous caller (issue #30 part 1). An empty
	// client_id is now treated like malformed credentials → invalid_client.
	clientID, clientSecret := extractClientCredentials(r)
	if clientID == "" {
		writeTokenError(w, "invalid_client", http.StatusUnauthorized)
		return
	}
	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		writeTokenError(w, "invalid_client", http.StatusUnauthorized)
		return
	}
	if err := verifyClientAuthWithBudget(ctx, s.bcrypt, client, clientSecret); err != nil {
		if errors.Is(err, ErrBcryptBusy) {
			writeBcryptBusyJSON(w)
			return
		}
		if ctx.Err() != nil {
			return
		}
		writeTokenError(w, "invalid_client", http.StatusUnauthorized)
		return
	}

	// Check the exact active membership before consuming the rotating
	// credential. This keeps suspension/revocation fail-closed and avoids
	// needlessly burning a valid token when authorization has been withdrawn.
	currentRT, lookupErr := s.store.GetRefreshToken(ctx, refreshToken)
	var principal models.Principal
	if lookupErr == nil {
		if currentRT.ClientID != clientID || currentRT.Resource != resource {
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		principal, err = s.store.GetPrincipalForLearner(ctx, currentRT.LearnerID, strings.Fields(currentRT.Scope))
		if err != nil {
			s.logger.Debug("refresh grant: tenant membership inactive", "err", err)
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
	}

	// Rotation is one DB transaction. The store rechecks validity and client
	// binding while atomically consuming the row. A replay of any retained
	// ancestor revokes the whole family before returning invalid_grant.
	newRT, err := s.store.RotateRefreshTokenWithScope(ctx, refreshToken, clientID, resource, requestedScope)
	if err != nil {
		if errors.Is(err, storeport.ErrInvalidOAuthScope) {
			writeTokenError(w, "invalid_scope", http.StatusBadRequest)
			return
		}
		if errors.Is(err, storeport.ErrRefreshTokenReuse) {
			s.logger.Warn("refresh token reuse detected; family revoked", "client_id", authLogIdentifier(clientID))
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		if errors.Is(err, storeport.ErrInvalidRefreshToken) {
			s.logger.Warn("refresh token rejected (expired, mismatched, or already rotated)", "client_id", authLogIdentifier(clientID))
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		s.logger.Error("rotate refresh token failed", "err", err)
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}
	if lookupErr != nil {
		// A successful rotation must have had an active lookup above. Reaching
		// this branch would indicate inconsistent persistence semantics; never
		// mint a bearer without the validated tenant membership.
		s.logger.Error("refresh rotation succeeded without active principal", "lookup_err", lookupErr)
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}

	principal.Scopes = strings.Fields(newRT.Scope)
	accessToken, err := GenerateJWTForPrincipalAndScope(s.baseURL, newRT.Resource, clientID, principal, newRT.Scope)
	if err != nil {
		s.logger.Error("generate jwt failed after refresh rotation", "err", err)
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}

	writeTokenResponse(w, accessToken, newRT.Token, newRT.Scope)
}

func writeTokenResponse(w http.ResponseWriter, accessToken, refreshToken, scope string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token":  accessToken,
		"token_type":    "bearer",
		"expires_in":    int(AccessTokenTTL.Seconds()),
		"refresh_token": refreshToken,
		"scope":         scope,
	})
}

func writeAccessTokenResponse(w http.ResponseWriter, accessToken, scope string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": accessToken,
		"token_type":   "bearer",
		"expires_in":   int(AccessTokenTTL.Seconds()),
		"scope":        scope,
	})
}

func writeTokenError(w http.ResponseWriter, errCode string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": errCode})
}

// validateRegistrationRedirectURIs enforces https-or-loopback and rejects
// private IPs to prevent SSRF / open-redirect through client registration.
func validateRegistrationRedirectURIs(uris []string) error {
	if len(uris) > 5 {
		return fmt.Errorf("too many redirect_uris (max 5)")
	}
	for _, raw := range uris {
		if len(raw) > 512 {
			return fmt.Errorf("redirect_uri too long (max 512 chars)")
		}
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid redirect_uri: %w", err)
		}
		if !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
			return fmt.Errorf("redirect_uri must be an absolute hierarchical URI with a host")
		}
		if u.Fragment != "" {
			return fmt.Errorf("redirect_uri must not contain a fragment")
		}
		if u.User != nil {
			return fmt.Errorf("redirect_uri must not contain userinfo")
		}

		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		if loopback {
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("loopback redirect_uri must use http or https (got %q)", u.Scheme)
			}
			continue
		}
		if u.Scheme != "https" {
			return fmt.Errorf("redirect_uri must use https (got %q)", u.Scheme)
		}
		if ip != nil && isPrivateIP(ip) {
			return fmt.Errorf("redirect_uri points to private IP range")
		}
	}
	return nil
}

var privateCIDRs = func() []*net.IPNet {
	blocks := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"127.0.0.0/8",
		"::1/128",
		"fc00::/7",
	}
	var out []*net.IPNet
	for _, b := range blocks {
		_, n, err := net.ParseCIDR(b)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}()

func isPrivateIP(ip net.IP) bool {
	for _, n := range privateCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// HandleRegister implements RFC 7591 dynamic client registration.
// Admission policy is enforced before body parsing or persistence; development
// defaults to open compatibility while production must select token/disabled.
func (s *OAuthServer) HandleRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admission, admitted := dynamicClientRegistrationAdmission(ctx)
	if !admitted {
		admittedRequest, ok := s.admitDynamicClientRegistration(w, r)
		if !ok {
			return
		}
		r = admittedRequest
		ctx = r.Context()
		admission, _ = dynamicClientRegistrationAdmission(ctx)
	}
	if r.ContentLength > registerBodyLimitBytes {
		writeRegistrationErrorStatus(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "request body too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, registerBodyLimitBytes)

	var req map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeRegistrationErrorStatus(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "request body too large")
			return
		}
		http.Error(w, `{"error":"invalid_client_metadata"}`, http.StatusBadRequest)
		return
	}

	clientName := ""
	if name, ok := req["client_name"].(string); ok {
		clientName = name
	}
	// R001: cap server-side. An attacker-controlled multi-KB phishing
	// paragraph in client_name would otherwise be displayed verbatim on
	// the consent screen. Truncate to 120 bytes at a valid UTF-8 boundary
	// so html/template still escapes safely.
	if len(clientName) > clientNameMaxLen {
		originalLen := len(clientName)
		truncated := clientName[:clientNameMaxLen]
		for !utf8.ValidString(truncated) && len(truncated) > 0 {
			truncated = truncated[:len(truncated)-1]
		}
		clientName = truncated
		s.logger.Warn("client_name truncated", "original_len", originalLen, "truncated_len", len(clientName))
	}

	// RFC 7591: confidential clients announce a secret-based auth method.
	authMethod := "none"
	if rawMethod, exists := req["token_endpoint_auth_method"]; exists {
		method, ok := rawMethod.(string)
		if !ok || method == "" {
			writeRegistrationError(w, "invalid_client_metadata", "token_endpoint_auth_method must be a supported string")
			return
		}
		authMethod = method
	}
	switch authMethod {
	case "none", "client_secret_basic", "client_secret_post":
	default:
		writeRegistrationError(w, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	confidential := authMethod == "client_secret_basic" || authMethod == "client_secret_post"
	applicationType := "web"
	if rawType, exists := req["application_type"]; exists {
		value, ok := rawType.(string)
		if !ok || (value != "web" && value != "native") {
			writeRegistrationError(w, "invalid_client_metadata", "application_type must be web or native")
			return
		}
		applicationType = value
	}

	s.logger.Info("dynamic client registration request", "auth_method", sanitizeAuthLogField(authMethod), "metadata_key_count", len(req))

	var uris []string
	if raw, ok := req["redirect_uris"]; ok {
		arr, ok := raw.([]interface{})
		if !ok {
			writeRegistrationError(w, "invalid_redirect_uri", "redirect_uris must be an array of strings")
			return
		}
		for _, v := range arr {
			uri, ok := v.(string)
			if !ok || uri == "" {
				writeRegistrationError(w, "invalid_redirect_uri", "redirect_uris must contain only non-empty strings")
				return
			}
			uris = append(uris, uri)
		}
	}
	// Redirect URIs may contain fixed query parameters. Treat the full values
	// as credential-adjacent input and never copy them into logs.
	s.logger.Info("registration redirect URIs parsed", "count", len(uris))
	if len(uris) == 0 {
		writeRegistrationError(w, "invalid_redirect_uri", "at least one redirect_uri required")
		return
	}
	if err := validateRegistrationRedirectURIs(uris); err != nil {
		writeRegistrationError(w, "invalid_redirect_uri", err.Error())
		return
	}
	sort.Strings(uris)
	for index := 1; index < len(uris); index++ {
		if uris[index] == uris[index-1] {
			writeRegistrationError(w, "invalid_redirect_uri", "redirect_uris must not contain duplicates")
			return
		}
	}

	clientID, err := generateCode()
	if err != nil {
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}

	redirectURIsJSON, err := json.Marshal(uris)
	if err != nil {
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}

	var clientSecret, secretHash string
	if confidential {
		clientSecret, err = generateCode()
		if err != nil {
			http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
			return
		}
		hash, herr := s.bcrypt.Generate(ctx, []byte(clientSecret), bcryptCost)
		if herr != nil {
			if errors.Is(herr, ErrBcryptBusy) {
				w.Header().Set("Retry-After", bcryptBusyRetryAfter)
				writeRegistrationErrorStatus(w, http.StatusServiceUnavailable, "temporarily_unavailable", "server is busy; retry shortly")
				return
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Error("bcrypt client secret failed", "err", herr)
			http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
			return
		}
		secretHash = string(hash)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(dynamicClientTTL)
	registration, err := s.store.RegisterDynamicOAuthClient(ctx, models.DynamicClientRegistration{
		TokenID:                   admission.TokenID,
		Fingerprint:               dynamicRegistrationFingerprint(clientName, string(redirectURIsJSON), authMethod, applicationType),
		CandidateClientID:         clientID,
		ClientName:                clientName,
		RedirectURIsJSON:          string(redirectURIsJSON),
		AuthMethod:                authMethod,
		ApplicationType:           applicationType,
		CandidateClientSecret:     clientSecret,
		CandidateClientSecretHash: secretHash,
		MaxClients:                s.maxRegisteredClients,
		ExpiresAt:                 expiresAt,
		Now:                       now,
	})
	if err != nil {
		if errors.Is(err, storeport.ErrOAuthClientLimitReached) {
			writeRegistrationError(w, "registration_disabled", "client cap reached")
			return
		}
		if errors.Is(err, storeport.ErrDCRInitialAccessTokenQuota) {
			writeRegistrationError(w, "registration_disabled", "initial access token quota reached")
			return
		}
		if errors.Is(err, storeport.ErrDCRInvalidInitialAccessToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="oauth-dcr", error="invalid_token"`)
			writeRegistrationErrorStatus(w, http.StatusUnauthorized, "invalid_token", "initial access token is missing or invalid")
			return
		}
		if errors.Is(err, storeport.ErrDCRReplaySecretUnavailable) {
			writeRegistrationError(w, "registration_disabled", "equivalent confidential client cannot be replayed safely")
			return
		}
		s.logger.Error("persist client registration failed", "err", err)
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}

	// Echo back all client metadata + add our fields (RFC 7591 compliance)
	resp := map[string]interface{}{
		"client_id":                  registration.ClientID,
		"client_id_issued_at":        registration.IssuedAt.Unix(),
		"client_name":                clientName,
		"redirect_uris":              uris,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": authMethod,
		"application_type":           applicationType,
		"client_id_expires_at":       registration.ExpiresAt.Unix(),
	}
	if confidential {
		resp["client_secret"] = registration.ClientSecret
		// 0 = secret never expires (RFC 7591 §3.2.1)
		resp["client_secret_expires_at"] = 0
	}

	s.logger.Info("dynamic client registered", "client_id", registration.ClientID, "confidential", confidential, "replayed", registration.Replayed)

	// RFC 6749 §5.1: responses with credentials must not be cached.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	status := http.StatusCreated
	if registration.Replayed {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func dynamicRegistrationFingerprint(clientName, redirectURIsJSON, authMethod, applicationType string) string {
	hash := sha256.New()
	for _, field := range []string{clientName, redirectURIsJSON, authMethod, applicationType} {
		_, _ = fmt.Fprintf(hash, "%d:", len(field))
		_, _ = hash.Write([]byte(field))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func writeRegistrationError(w http.ResponseWriter, errCode, desc string) {
	writeRegistrationErrorStatus(w, http.StatusBadRequest, errCode, desc)
}

func writeRegistrationErrorStatus(w http.ResponseWriter, status int, errCode, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error":             errCode,
		"error_description": desc,
	})
}

func generateCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
