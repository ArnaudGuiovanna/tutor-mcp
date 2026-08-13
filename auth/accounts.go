// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const (
	accountFormBodyLimitBytes     int64 = 16 << 10
	accountTokenMaxLen                  = 512
	emailVerificationTTL                = 30 * time.Minute
	passwordResetTTL                    = 15 * time.Minute
	loginChallengeTTL                   = 10 * time.Minute
	trustedLoginDeviceTTL               = 30 * 24 * time.Hour
	accountCSRFCookieName               = "account_csrf"
	trustedLoginDeviceCookieName        = "tutor_trusted_device"
	accountTokenEmailVerification       = "email_verification"
	accountTokenPasswordReset           = "password_reset"
)

func (s *OAuthServer) SetEmailSender(sender EmailSender) { s.emailSender = sender }

func renderBcryptBusyAccountPage(w http.ResponseWriter) {
	w.Header().Set("Retry-After", bcryptBusyRetryAfter)
	renderAccountPage(w, http.StatusServiceUnavailable, accountPageData{
		Title: "Temporarily busy", Message: "Password processing is at capacity. Please try again shortly.",
	})
}

func accountTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func validRawAccountToken(raw string) bool {
	return raw != "" && len(raw) <= accountTokenMaxLen && !strings.ContainsAny(raw, "\r\n\t ")
}

func setAccountCSRFCookie(w http.ResponseWriter, value, path string, maxAge int) {
	cookie := &http.Cookie{
		Name: accountCSRFCookieName, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
	if maxAge < 0 {
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	http.SetCookie(w, cookie)
}

func (s *OAuthServer) validateAccountCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(accountCSRFCookieName)
	formToken := r.FormValue("csrf_token")
	if err != nil || cookie.Value == "" || formToken == "" ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formToken)) != 1 {
		return false
	}
	return s.consumeCSRF(r.Context(), formToken)
}

func oauthContinuation(data authPageData) *models.AccountToken {
	return &models.AccountToken{
		Purpose:             accountTokenEmailVerification,
		ClientID:            data.ClientID,
		RedirectURI:         data.RedirectURI,
		Resource:            data.Resource,
		State:               data.State,
		Scope:               data.Scope,
		CodeChallenge:       data.CodeChallenge,
		CodeChallengeMethod: data.CodeChallengeMethod,
	}
}

// loadEmailVerificationContext revalidates every OAuth binding carried across
// the mailbox hop. The account token is a bearer capability, but it must never
// revive an expired/changed client, redirect, resource, or PKCE request.
func (s *OAuthServer) loadEmailVerificationContext(ctx context.Context, raw string) (*models.AccountToken, *models.OAuthClient, error) {
	if !validRawAccountToken(raw) {
		return nil, nil, storeport.ErrInvalidAccountToken
	}
	token, err := s.store.GetAccountToken(ctx, accountTokenHash(raw), accountTokenEmailVerification)
	if err != nil || token.Resource != MCPResource(s.baseURL) {
		return nil, nil, storeport.ErrInvalidAccountToken
	}
	canonicalScope, err := models.CanonicalOAuthScope(token.Scope)
	if err != nil || canonicalScope != token.Scope {
		return nil, nil, storeport.ErrInvalidAccountToken
	}
	if err := s.validateRedirectURI(ctx, token.ClientID, token.RedirectURI); err != nil {
		return nil, nil, storeport.ErrInvalidAccountToken
	}
	if err := s.requirePKCEForPublicClient(ctx, token.ClientID, token.CodeChallenge, token.CodeChallengeMethod); err != nil {
		return nil, nil, storeport.ErrInvalidAccountToken
	}
	client, err := s.resolveOAuthClient(ctx, token.ClientID)
	if err != nil {
		return nil, nil, storeport.ErrInvalidAccountToken
	}
	return token, client, nil
}

func (s *OAuthServer) renderEmailVerificationForm(w http.ResponseWriter, status int, raw string, token *models.AccountToken, client *models.OAuthClient, message string) {
	csrfToken, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setAccountCSRFCookie(w, csrfToken, "/verify-email", 1800)
	renderAccountPage(w, status, accountPageData{
		Title:            "Secure your account",
		Message:          message,
		Token:            raw,
		CSRFToken:        csrfToken,
		ClientName:       client.ClientName,
		ClientID:         token.ClientID,
		RedirectOrigin:   formActionOriginFromRedirectURI(token.RedirectURI),
		ScopeDescription: oauthScopeDescription(token.Scope),
		ShowVerify:       true,
	})
}

func (s *OAuthServer) sendEmailVerification(ctx context.Context, learner *models.Learner, data authPageData) error {
	if s.emailSender == nil {
		return fmt.Errorf("email delivery is not configured")
	}
	raw, err := generateCode()
	if err != nil {
		return fmt.Errorf("generate verification token: %w", err)
	}
	now := time.Now().UTC()
	token := oauthContinuation(data)
	token.TokenHash = accountTokenHash(raw)
	token.LearnerID = learner.ID
	token.CreatedAt = now
	token.ExpiresAt = now.Add(emailVerificationTTL)
	if err := s.store.CreateAccountToken(ctx, token); err != nil {
		return err
	}
	link := s.baseURL + "/verify-email?" + url.Values{"token": []string{raw}}.Encode()
	if err := s.emailSender.SendVerification(ctx, learner.Email, link); err != nil {
		return fmt.Errorf("send verification email: %w", err)
	}
	return nil
}

func (s *OAuthServer) sendPasswordReset(ctx context.Context, learner *models.Learner) error {
	if s.emailSender == nil {
		return fmt.Errorf("email delivery is not configured")
	}
	raw, err := generateCode()
	if err != nil {
		return fmt.Errorf("generate password reset token: %w", err)
	}
	now := time.Now().UTC()
	token := &models.AccountToken{
		TokenHash: accountTokenHash(raw), LearnerID: learner.ID,
		Purpose: accountTokenPasswordReset, CreatedAt: now,
		ExpiresAt: now.Add(passwordResetTTL),
	}
	if err := s.store.CreateAccountToken(ctx, token); err != nil {
		return err
	}
	link := s.baseURL + "/reset-password?" + url.Values{"token": []string{raw}}.Encode()
	if err := s.emailSender.SendPasswordReset(ctx, learner.Email, link); err != nil {
		return fmt.Errorf("send password reset email: %w", err)
	}
	return nil
}

func (s *OAuthServer) sendLoginChallenge(ctx context.Context, learner *models.Learner, data authPageData) (bool, error) {
	sender, ok := s.emailSender.(LoginChallengeEmailSender)
	if !ok || sender == nil {
		return false, fmt.Errorf("login challenge delivery is not configured")
	}
	raw, err := generateCode()
	if err != nil {
		return false, fmt.Errorf("generate login challenge: %w", err)
	}
	now := time.Now().UTC()
	challenge := &models.LoginChallenge{
		TokenHash: accountTokenHash(raw), LearnerID: learner.ID,
		ClientID: data.ClientID, RedirectURI: data.RedirectURI,
		Resource: data.Resource, State: data.State, Scope: data.Scope,
		CodeChallenge: data.CodeChallenge, CodeChallengeMethod: data.CodeChallengeMethod,
		CreatedAt: now, ExpiresAt: now.Add(loginChallengeTTL),
	}
	created, err := s.store.CreateLoginChallenge(ctx, challenge)
	if err != nil || !created {
		return created, err
	}
	link := s.baseURL + "/login-challenge?" + url.Values{"token": []string{raw}}.Encode()
	if err := sender.SendLoginChallenge(ctx, learner.Email, link); err != nil {
		_ = s.store.DeleteLoginChallenge(ctx, challenge.TokenHash)
		return false, fmt.Errorf("send login challenge: %w", err)
	}
	return true, nil
}

func (s *OAuthServer) isTrustedLoginDevice(ctx context.Context, r *http.Request, learnerID string) (bool, error) {
	cookie, err := r.Cookie(trustedLoginDeviceCookieName)
	if err != nil || !validRawAccountToken(cookie.Value) {
		return false, nil
	}
	return s.store.IsTrustedLoginDevice(ctx, learnerID, accountTokenHash(cookie.Value), time.Now().UTC())
}

func setTrustedLoginDeviceCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name: trustedLoginDeviceCookieName, Value: raw, Path: "/authorize",
		MaxAge: int(trustedLoginDeviceTTL / time.Second), HttpOnly: true,
		Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func (s *OAuthServer) HandleLoginChallengeGet(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("token")
	if !validRawAccountToken(raw) {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This sign-in confirmation link is invalid or expired."})
		return
	}
	challenge, err := s.store.GetLoginChallenge(r.Context(), accountTokenHash(raw), time.Now().UTC())
	if err != nil {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This sign-in confirmation link is invalid or expired."})
		return
	}
	csrfToken, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setAccountCSRFCookie(w, csrfToken, "/login-challenge", int(loginChallengeTTL/time.Second))
	renderAccountPage(w, http.StatusOK, accountPageData{
		Title: "Confirm this sign-in", Message: "Unusual failed sign-in activity was detected before a correct password was entered.",
		Token: raw, CSRFToken: csrfToken, ClientID: challenge.ClientID,
		RedirectOrigin: formActionOriginFromRedirectURI(challenge.RedirectURI), ShowLoginChallenge: true,
	})
}

func (s *OAuthServer) HandleLoginChallengePost(w http.ResponseWriter, r *http.Request) {
	if !parseLimitedForm(w, r, accountFormBodyLimitBytes) {
		return
	}
	if !s.validateAccountCSRF(r) {
		http.Error(w, "forbidden: csrf check failed", http.StatusForbidden)
		return
	}
	raw := r.FormValue("token")
	if !validRawAccountToken(raw) {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This sign-in confirmation link is invalid or expired."})
		return
	}
	now := time.Now().UTC()
	challenge, err := s.store.ConsumeLoginChallenge(
		r.Context(), accountTokenHash(raw), now, now.Add(trustedLoginDeviceTTL),
	)
	if err != nil {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This sign-in confirmation link is invalid or expired."})
		return
	}
	continuation, err := url.Parse(s.baseURL + "/authorize")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	query := continuation.Query()
	query.Set("client_id", challenge.ClientID)
	query.Set("redirect_uri", challenge.RedirectURI)
	query.Set("response_type", "code")
	query.Set("resource", challenge.Resource)
	if challenge.State != "" {
		query.Set("state", challenge.State)
	}
	if challenge.Scope != "" {
		query.Set("scope", challenge.Scope)
	}
	if challenge.CodeChallenge != "" {
		query.Set("code_challenge", challenge.CodeChallenge)
		query.Set("code_challenge_method", challenge.CodeChallengeMethod)
	}
	continuation.RawQuery = query.Encode()
	setTrustedLoginDeviceCookie(w, raw)
	setAccountCSRFCookie(w, "", "/login-challenge", -1)
	http.Redirect(w, r, continuation.String(), http.StatusFound)
}

func (s *OAuthServer) renderVerificationPending(w http.ResponseWriter) {
	renderAccountPage(w, http.StatusAccepted, accountPageData{
		Title:   "Check your email",
		Message: "We sent a short-lived verification link. Open it to finish connecting your MCP client.",
	})
}

func (s *OAuthServer) HandleVerifyEmailGet(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("token")
	token, client, err := s.loadEmailVerificationContext(r.Context(), raw)
	if err != nil {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This verification link is invalid or expired."})
		return
	}
	s.renderEmailVerificationForm(w, http.StatusOK, raw, token, client,
		"Choose a fresh password and verify the exact client below. If you did not initiate this request, close this page without submitting it.")
}

func (s *OAuthServer) HandleVerifyEmailPost(w http.ResponseWriter, r *http.Request) {
	if !parseLimitedForm(w, r, accountFormBodyLimitBytes) {
		return
	}
	if !s.validateAccountCSRF(r) {
		http.Error(w, "forbidden: csrf check failed", http.StatusForbidden)
		return
	}
	raw := r.FormValue("token")
	token, client, err := s.loadEmailVerificationContext(r.Context(), raw)
	if err != nil {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This verification link is invalid or expired."})
		return
	}
	password := r.FormValue("password")
	if password != r.FormValue("password_confirm") || len(password) < passwordMinLen || len(password) > passwordMaxLen {
		s.renderEmailVerificationForm(w, http.StatusBadRequest, raw, token, client,
			fmt.Sprintf("Choose matching passwords between %d and %d characters.", passwordMinLen, passwordMaxLen))
		return
	}
	if r.FormValue("approve_client") != "yes" {
		s.renderEmailVerificationForm(w, http.StatusBadRequest, raw, token, client,
			"Explicit client approval is required. If you do not recognize this request, close the page.")
		return
	}
	passwordHash, err := s.bcrypt.Generate(r.Context(), []byte(password), bcryptCost)
	if err != nil {
		if errors.Is(err, ErrBcryptBusy) {
			renderBcryptBusyAccountPage(w)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	code, err := generateCode()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token, err = s.store.ActivateLearnerAndCreateAuthCode(
		r.Context(), accountTokenHash(raw), string(passwordHash), code,
		time.Now().Add(5*time.Minute), !isCIMDClientID(token.ClientID),
	)
	if errors.Is(err, storeport.ErrInvalidAccountToken) {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This verification link is invalid or expired."})
		return
	}
	if err != nil {
		s.logger.Error("verify email failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	redirect, err := url.Parse(token.RedirectURI)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	query := redirect.Query()
	query.Set("code", code)
	if token.State != "" {
		query.Set("state", token.State)
	}
	query.Set("iss", s.baseURL)
	redirect.RawQuery = query.Encode()
	setAccountCSRFCookie(w, "", "/verify-email", -1)
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (s *OAuthServer) HandleRecoverGet(w http.ResponseWriter, _ *http.Request) {
	csrfToken, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setAccountCSRFCookie(w, csrfToken, "/recover", 1800)
	renderAccountPage(w, http.StatusOK, accountPageData{
		Title: "Reset your password", Message: "Enter your account email.",
		CSRFToken: csrfToken, ShowRecover: true,
	})
}

func (s *OAuthServer) HandleRecoverPost(w http.ResponseWriter, r *http.Request) {
	if !parseLimitedForm(w, r, accountFormBodyLimitBytes) {
		return
	}
	if !s.validateAccountCSRF(r) {
		http.Error(w, "forbidden: csrf check failed", http.StatusForbidden)
		return
	}
	email := NormalizeEmail(r.FormValue("email"))
	if validateEmail(email) == nil {
		learner, err := s.store.GetLearnerByEmail(r.Context(), email)
		if err == nil && learner != nil {
			if sendErr := s.sendPasswordReset(r.Context(), learner); sendErr != nil {
				s.logger.Error("password reset delivery failed", "err", sendErr)
			}
		} else if err != nil {
			// Deliberately do not distinguish sql.ErrNoRows or any backend
			// failure in the public response.
			s.logger.Debug("password reset lookup did not produce an account")
		}
	}
	setAccountCSRFCookie(w, "", "/recover", -1)
	renderAccountPage(w, http.StatusAccepted, accountPageData{
		Title:   "Check your email",
		Message: "If an account can receive mail at that address, a short-lived reset link has been sent.",
	})
}

func (s *OAuthServer) HandleResetPasswordGet(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("token")
	if !validRawAccountToken(raw) {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This reset link is invalid or expired."})
		return
	}
	if _, err := s.store.GetAccountToken(r.Context(), accountTokenHash(raw), accountTokenPasswordReset); err != nil {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This reset link is invalid or expired."})
		return
	}
	csrfToken, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setAccountCSRFCookie(w, csrfToken, "/reset-password", 1800)
	renderAccountPage(w, http.StatusOK, accountPageData{
		Title: "Choose a new password", Token: raw, CSRFToken: csrfToken, ShowReset: true,
	})
}

func (s *OAuthServer) HandleResetPasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseLimitedForm(w, r, accountFormBodyLimitBytes) {
		return
	}
	if !s.validateAccountCSRF(r) {
		http.Error(w, "forbidden: csrf check failed", http.StatusForbidden)
		return
	}
	raw := r.FormValue("token")
	password := r.FormValue("password")
	if !validRawAccountToken(raw) || password != r.FormValue("password_confirm") ||
		len(password) < passwordMinLen || len(password) > passwordMaxLen {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Could not reset password", Message: "The link or password is invalid."})
		return
	}
	hash, err := s.bcrypt.Generate(r.Context(), []byte(password), bcryptCost)
	if err != nil {
		if errors.Is(err, ErrBcryptBusy) {
			renderBcryptBusyAccountPage(w)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := s.store.ResetPasswordWithToken(r.Context(), accountTokenHash(raw), string(hash)); err != nil {
		if !errors.Is(err, storeport.ErrInvalidAccountToken) {
			s.logger.Error("password reset failed", "err", err)
		}
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Could not reset password", Message: "The link or password is invalid."})
		return
	}
	setAccountCSRFCookie(w, "", "/reset-password", -1)
	renderAccountPage(w, http.StatusOK, accountPageData{
		Title: "Password updated", Message: "Stored sign-ins have been revoked. An already-issued access token can remain valid for up to 30 minutes; then sign in again from your MCP client.",
	})
}
