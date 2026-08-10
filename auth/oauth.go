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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

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

const (
	authorizeBodyLimitBytes          int64 = 16 << 10
	tokenBodyLimitBytes              int64 = 16 << 10
	registerBodyLimitBytes           int64 = 16 << 10
	defaultMaxRegisteredOAuthClients       = 10_000
	emailMaxLen                            = 254
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

// oauthStore is the persistence surface the OAuth server needs.
type oauthStore interface {
	storeport.LearnerStore
	storeport.AuthStore
}

// OAuthServer implements the OAuth 2.1 authorization server.
type OAuthServer struct {
	store                oauthStore
	baseURL              string
	logger               *slog.Logger
	emailSender          EmailSender
	loginFailures        *LoginFailureTracker
	maxRegisteredClients int
	cimdHTTPClient       *http.Client
	cimdMu               sync.Mutex
	cimdCache            map[string]cachedCIMDClient
	csrfMu               sync.Mutex
	usedCSRF             map[string]time.Time
	lastCSRFPrune        time.Time
}

// NewOAuthServer creates a new OAuthServer. The login-failure tracker locks
// out an email after 5 password mismatches in 10 minutes (issue #36).
func NewOAuthServer(store oauthStore, baseURL string, logger *slog.Logger) *OAuthServer {
	return &OAuthServer{
		store:                store,
		baseURL:              baseURL,
		logger:               logger,
		loginFailures:        NewLoginFailureTracker(5, 10*time.Minute),
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

// consumeCSRF makes a valid double-submit token single-use within this server
// process. Cookie rotation protects normal browser retries; the replay cache
// additionally rejects an exact HTTP replay carrying the old Cookie header.
func (s *OAuthServer) consumeCSRF(token string) bool {
	now := time.Now().UTC()
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
	meta := map[string]interface{}{
		"issuer":                                s.baseURL,
		"authorization_endpoint":                s.baseURL + "/authorize",
		"token_endpoint":                        s.baseURL + "/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{"learner"},
		"registration_endpoint":                 s.baseURL + "/register",
		"client_id_metadata_document_supported": true,
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		// RFC 9207: we include iss in authorization responses.
		"authorization_response_iss_parameter_supported": true,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(meta)
}

func (s *OAuthServer) HandleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	meta := map[string]interface{}{
		"resource":              s.baseURL + "/mcp",
		"authorization_servers": []string{s.baseURL},
		"scopes_supported":      []string{"learner"},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(meta)
}

// validateRedirectURI checks that the supplied redirectURI is strictly equal
// to one of the URIs registered for the given clientID. No prefix / wildcard.
func (s *OAuthServer) validateRedirectURI(ctx context.Context, clientID, redirectURI string) error {
	if clientID == "" || redirectURI == "" {
		return fmt.Errorf("missing client_id or redirect_uri")
	}
	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		return fmt.Errorf("unknown client")
	}
	var registered []string
	if err := json.Unmarshal([]byte(client.RedirectURIs), &registered); err != nil {
		return fmt.Errorf("malformed registration")
	}
	for _, u := range registered {
		if u == redirectURI {
			return nil
		}
	}
	return fmt.Errorf("redirect_uri not registered")
}

func validateAuthorizationParams(responseType, scope string) error {
	if responseType != "code" {
		return fmt.Errorf("unsupported response_type")
	}
	if scope != "" && scope != "learner" {
		return fmt.Errorf("unsupported scope")
	}
	return nil
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

	if err := s.validateRedirectURI(ctx, clientID, redirectURI); err != nil {
		s.logger.Debug("authorize GET: redirect_uri rejected", "err", err, "client_id", clientID)
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	if err := validateAuthorizationParams(q.Get("response_type"), q.Get("scope")); err != nil {
		s.logger.Debug("authorize GET: parameters rejected", "err", err, "client_id", clientID)
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		s.logger.Debug("authorize GET: client lookup failed", "err", err, "client_id", clientID)
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")
	if err := s.requirePKCEForPublicClient(ctx, clientID, codeChallenge, codeChallengeMethod); err != nil {
		s.logger.Debug("authorize GET: PKCE missing for public client", "err", err, "client_id", clientID)
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	s.logger.Info("authorize GET", "client_id", clientID, "state_len", len(q.Get("state")))

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
		Scope:               q.Get("scope"),
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
	if !s.consumeCSRF(formCSRF) {
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
	if err := s.validateRedirectURI(ctx, clientID, redirectURI); err != nil {
		s.logger.Debug("authorize POST: redirect_uri rejected", "err", err, "client_id", clientID)
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	if err := validateAuthorizationParams(r.FormValue("response_type"), r.FormValue("scope")); err != nil {
		s.logger.Debug("authorize POST: parameters rejected", "err", err, "client_id", clientID)
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	client, err := s.resolveOAuthClient(ctx, clientID)
	if err != nil {
		s.logger.Debug("authorize POST: client lookup failed", "err", err, "client_id", clientID)
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	codeChallenge := r.FormValue("code_challenge")
	codeChallengeMethod := r.FormValue("code_challenge_method")
	if err := s.requirePKCEForPublicClient(ctx, clientID, codeChallenge, codeChallengeMethod); err != nil {
		s.logger.Debug("authorize POST: PKCE missing for public client", "err", err, "client_id", clientID)
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
		Scope:               r.FormValue("scope"),
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

	var learnerID string

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
		pendingHash, err := bcrypt.GenerateFromPassword([]byte(pendingCredential), bcryptCost)
		if err != nil {
			s.logger.Error("bcrypt pending credential failed", "err", err)
			renderAuthPage(w, data, "Internal error. Please try again.", "register")
			return
		}

		// Check if email already exists. A database failure is not equivalent
		// to an available address; continuing could create a duplicate account
		// or expose backend-dependent behavior.
		existing, lookupErr := s.store.GetLearnerByEmail(ctx, email)
		if lookupErr == nil && existing != nil {
			// A fresh request for an inactive placeholder replaces the prior OAuth
			// continuation. This lets the mailbox owner recover from an attacker-
			// initiated pending registration without revealing account state.
			if existing.EmailVerifiedAt == nil {
				if err := s.sendEmailVerification(ctx, existing, data); err != nil {
					s.logger.Error("pending registration verification delivery failed", "err", err)
				}
			}
			s.renderVerificationPending(w)
			return
		}
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
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
		// Per-account lockout (issue #36): refuse new attempts when the email
		// has accumulated too many recent failures, regardless of source IP.
		// The per-IP authLimiter still runs in front; this guard catches the
		// distributed-source brute-force the IP limiter cannot see.
		if !s.loginFailures.Allow(email) {
			// Never let an attacker lock the rightful owner out by submitting bad
			// passwords for their email. Continue through the constant-cost check;
			// a correct password resets the bucket, while bad attempts remain
			// covered by the IP limiter and audit signal.
			s.logger.Warn("login failure threshold reached; correct credentials remain allowed")
		}
		existing, err := s.store.GetLearnerByEmail(ctx, email)
		if err != nil {
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
			s.loginFailures.Record(email)
			renderAuthPage(w, data, "Invalid email or password.", "login")
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(existing.PasswordHash), []byte(password)); err != nil {
			s.loginFailures.Record(email)
			renderAuthPage(w, data, "Invalid email or password.", "login")
			return
		}
		s.loginFailures.Reset(email)
		// R001: if the learner has already approved this client+redirect_uri,
		// the approval screen is no longer meaningful — skip it. Re-prompting
		// every time trained users to click through reflexively, defeating the
		// trust-on-first-use guarantee against a malicious dynamic-client
		// registration. The approval is scoped to redirect_uri so a phishing
		// client cannot reuse a previously-granted consent at a different URL.
		approved := false
		if !isCIMDClientID(clientID) {
			approved, err = s.store.IsClientApproved(ctx, existing.ID, clientID, redirectURI)
			if err != nil {
				s.logger.Error("client approval lookup failed", "err", err, "learner", existing.ID, "client", clientID)
				renderAuthPage(w, data, "Internal error. Please try again.", "login")
				return
			}
		}
		if !approved && r.FormValue("approve_client") != "yes" {
			renderAuthPage(w, data, "Please confirm that you recognize and approve this OAuth client before continuing.", "login")
			return
		}
		if existing.EmailVerifiedAt == nil {
			if err := s.sendEmailVerification(ctx, existing, data); err != nil {
				s.logger.Error("login verification delivery failed", "err", err)
				renderAuthPage(w, data, "Internal error. Please try again.", "login")
				return
			}
			s.renderVerificationPending(w)
			return
		}
		if !approved && !isCIMDClientID(clientID) {
			if err := s.store.ApproveClient(ctx, existing.ID, clientID, redirectURI); err != nil {
				s.logger.Warn("persist client approval failed", "err", err, "learner", existing.ID, "client", clientID)
			}
		}
		learnerID = existing.ID
	}

	// Generate auth code
	code, err := generateCode()
	if err != nil {
		s.logger.Error("generate code failed", "err", err)
		renderAuthPage(w, data, "Internal error. Please try again.", mode)
		return
	}

	if err := s.store.CreateAuthCodeWithBinding(
		ctx, code, learnerID, codeChallenge, codeChallengeMethod,
		clientID, redirectURI, resource, time.Now().Add(5*time.Minute),
	); err != nil {
		s.logger.Error("create auth code failed", "err", err)
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
	s.logger.Info("authorize POST redirect", "state_len", len(state), "redirect_host", u.Host)
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

// verifyClientAuth enforces secret-based authentication for confidential clients.
// Public clients (empty stored hash) pass through and rely on PKCE.
// The stored hash is a bcrypt digest; CompareHashAndPassword is constant-time.
func verifyClientAuth(client *models.OAuthClient, suppliedSecret string) error {
	if client.ClientSecretHash == "" {
		return nil
	}
	if suppliedSecret == "" {
		return fmt.Errorf("invalid_client")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(client.ClientSecretHash), []byte(suppliedSecret)); err != nil {
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
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
	}
}

func (s *OAuthServer) handleAuthorizationCodeGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.FormValue("code")
	codeVerifier := r.FormValue("code_verifier")
	redirectURI := r.FormValue("redirect_uri")
	resource := r.FormValue("resource")
	clientID, clientSecret := extractClientCredentials(r)

	s.logger.Debug("token exchange attempt", "code_len", len(code), "verifier_len", len(codeVerifier), "client_id", clientID)

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
		s.logger.Debug("token exchange: unknown client", "client_id", clientID)
		writeTokenError(w, "invalid_client", http.StatusUnauthorized)
		return
	}
	if err := verifyClientAuth(client, clientSecret); err != nil {
		s.logger.Debug("token exchange: client auth failed", "client_id", clientID)
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
		s.logger.Debug("token exchange: resource mismatch", "client_id", clientID)
		writeTokenError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	// RFC 6749 §4.1.3: when redirect_uri was present in the authorization
	// request, the token request must carry the identical value. Empty binding
	// denotes a pre-upgrade code and is accepted once for compatibility.
	if authCode.RedirectURI != "" && redirectURI != authCode.RedirectURI {
		s.logger.Debug("token exchange: redirect_uri mismatch", "client_id", clientID)
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
			s.logger.Warn("token exchange: empty PKCE challenge for public client — refusing", "client_id", clientID)
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		if codeVerifier != "" {
			// RFC 9700 §2.1.1 downgrade guard: a verifier cannot appear at
			// the token endpoint unless its challenge was bound to the code.
			s.logger.Debug("token exchange: unexpected code_verifier without challenge", "client_id", clientID)
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
			s.logger.Warn("token exchange: unsupported stored PKCE method", "client_id", clientID, "method", method)
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

	// Sign before mutating persistence. Configuration/signing failures must not
	// consume a valid one-time code.
	accessToken, err := GenerateJWTForResource(s.baseURL, resource, authCode.LearnerID)
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

	writeTokenResponse(w, accessToken, rt.Token)
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
	if err := verifyClientAuth(client, clientSecret); err != nil {
		writeTokenError(w, "invalid_client", http.StatusUnauthorized)
		return
	}

	// Rotation is one DB transaction. The store rechecks validity and client
	// binding while atomically consuming the row. A replay of any retained
	// ancestor revokes the whole family before returning invalid_grant.
	newRT, err := s.store.RotateRefreshToken(ctx, refreshToken, clientID, resource)
	if err != nil {
		if errors.Is(err, storeport.ErrRefreshTokenReuse) {
			s.logger.Warn("refresh token reuse detected; family revoked", "client_id", clientID)
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		if errors.Is(err, storeport.ErrInvalidRefreshToken) {
			s.logger.Warn("refresh token rejected (expired, mismatched, or already rotated)", "client_id", clientID)
			writeTokenError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		s.logger.Error("rotate refresh token failed", "err", err)
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}

	accessToken, err := GenerateJWTForResource(s.baseURL, newRT.Resource, newRT.LearnerID)
	if err != nil {
		s.logger.Error("generate jwt failed after refresh rotation", "err", err)
		writeTokenError(w, "server_error", http.StatusInternalServerError)
		return
	}

	writeTokenResponse(w, accessToken, newRT.Token)
}

func writeTokenResponse(w http.ResponseWriter, accessToken, refreshToken string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token":  accessToken,
		"token_type":    "bearer",
		"expires_in":    int(AccessTokenTTL.Seconds()),
		"refresh_token": refreshToken,
		"scope":         "learner",
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
// Claude.ai must register as an OAuth client before starting the auth flow.
func (s *OAuthServer) HandleRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := s.store.CleanupExpiredOAuthClients(ctx); err != nil {
		s.logger.Error("cleanup expired dynamic clients failed", "err", err)
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
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

	s.logger.Info("dynamic client registration request", "auth_method", authMethod, "metadata_key_count", len(req))

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

	if err := s.requireRegistrationCapacity(w); err != nil {
		return
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
		hash, herr := bcrypt.GenerateFromPassword([]byte(clientSecret), bcryptCost)
		if herr != nil {
			s.logger.Error("bcrypt client secret failed", "err", herr)
			http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
			return
		}
		secretHash = string(hash)
	}

	expiresAt := time.Now().UTC().Add(dynamicClientTTL)
	if err := s.store.CreateOAuthClientWithSecretCappedTTL(ctx, clientID, clientName, string(redirectURIsJSON), secretHash, s.maxRegisteredClients, expiresAt); err != nil {
		if errors.Is(err, storeport.ErrOAuthClientLimitReached) {
			writeRegistrationError(w, "registration_disabled", "client cap reached")
			return
		}
		s.logger.Error("persist client registration failed", "err", err)
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}

	// Echo back all client metadata + add our fields (RFC 7591 compliance)
	resp := map[string]interface{}{
		"client_id":                  clientID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                clientName,
		"redirect_uris":              uris,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": authMethod,
		"scope":                      "learner",
		"application_type":           applicationType,
		"client_id_expires_at":       expiresAt.Unix(),
	}
	if confidential {
		resp["client_secret"] = clientSecret
		// 0 = secret never expires (RFC 7591 §3.2.1)
		resp["client_secret_expires_at"] = 0
	}

	s.logger.Info("dynamic client registered", "client_id", clientID, "confidential", confidential)

	// RFC 6749 §5.1: responses with credentials must not be cached.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (s *OAuthServer) requireRegistrationCapacity(w http.ResponseWriter) error {
	if s.maxRegisteredClients <= 0 {
		return nil
	}
	n, err := s.store.CountOAuthClients(context.Background())
	if err != nil {
		s.logger.Error("count oauth clients failed", "err", err)
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return err
	}
	if n >= s.maxRegisteredClients {
		writeRegistrationError(w, "registration_disabled", "client cap reached")
		return fmt.Errorf("oauth client cap reached")
	}
	return nil
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
