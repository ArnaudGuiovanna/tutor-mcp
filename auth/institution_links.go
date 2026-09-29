// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const (
	invitationCookiePath = "/invite"
	signupCookiePath     = "/signup"
	signupTTL            = 30 * time.Minute
	tenantNameMaxRunes   = 120
)

var signupSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func invalidLinkPage(w http.ResponseWriter, title, message string) {
	renderConsolePage(w, http.StatusBadRequest, consolePageData{Page: "message", Title: title, Error: message})
}

// choosePassword validates a new password and its confirmation, then hashes
// it within the bcrypt budget. It writes the response on failure.
func (s *OAuthServer) choosePassword(w http.ResponseWriter, r *http.Request, retry func(string)) (string, bool) {
	password := r.FormValue("password")
	if len(password) < passwordMinLen || len(password) > passwordMaxLen {
		retry("Choose a password of 12 to 72 characters.")
		return "", false
	}
	if password != r.FormValue("password_confirm") {
		retry("The two passwords do not match.")
		return "", false
	}
	hash, err := s.bcrypt.Generate(r.Context(), []byte(password), bcryptCost)
	if errors.Is(err, ErrBcryptBusy) {
		renderBcryptBusyAccountPage(w)
		return "", false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", false
	}
	return string(hash), true
}

// ---------------------------------------------------------------------------
// Invitations
// ---------------------------------------------------------------------------

func (s *OAuthServer) HandleInvitationGet(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		http.NotFound(w, r)
		return
	}
	s.renderInvitation(w, r, r.URL.Query().Get("token"), http.StatusOK, "")
}

func (s *OAuthServer) renderInvitation(w http.ResponseWriter, r *http.Request, raw string, status int, message string) {
	if !validRawAccountToken(raw) {
		invalidLinkPage(w, "Invalid invitation", "This invitation link is invalid or expired. Ask your institution for a new one.")
		return
	}
	preview, err := s.accounts.PreviewTenantInvitation(r.Context(), models.InvitationCredential{Token: raw})
	if err != nil {
		invalidLinkPage(w, "Invalid invitation", "This invitation link is invalid or expired. Ask your institution for a new one.")
		return
	}
	csrf, ok := consoleCSRF(w, invitationCookiePath)
	if !ok {
		return
	}
	renderConsolePage(w, status, consolePageData{
		Page: "invite", Title: "Join " + preview.TenantName, Error: message, CSRFToken: csrf, Token: raw,
		TenantName: preview.TenantName, Email: preview.Email, Roles: strings.Join(preview.Roles, ", "),
		ExistingUser: preview.ExistingUser,
	})
}

func (s *OAuthServer) HandleInvitationPost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	ctx := r.Context()
	raw := r.FormValue("token")
	retry := func(message string) { s.renderInvitation(w, r, raw, http.StatusBadRequest, message) }
	if !validRawAccountToken(raw) {
		retry("")
		return
	}
	credential := models.InvitationCredential{Token: raw}
	preview, err := s.accounts.PreviewTenantInvitation(ctx, credential)
	if err != nil {
		retry("")
		return
	}
	var membership *models.TenantMembership
	if preview.ExistingUser {
		user, ok := s.verifyLocalPassword(w, r, preview.Email, r.FormValue("password"))
		if !ok {
			if w.Header().Get("Retry-After") == "" {
				retry("Invalid password.")
			}
			return
		}
		membership, err = s.store.AcceptTenantInvitation(ctx, raw, user.ID)
	} else {
		hash, ok := s.choosePassword(w, r, retry)
		if !ok {
			return
		}
		membership, err = s.accounts.AcceptTenantInvitationWithNewUser(ctx, credential, hash)
	}
	switch {
	case errors.Is(err, storeport.ErrAlreadyMember):
		invalidLinkPage(w, "Already a member", "This account already belongs to "+preview.TenantName+".")
		return
	case errors.Is(err, storeport.ErrAccountExists):
		retry("An account now exists for this email. Reload the invitation and sign in.")
		return
	case err != nil:
		s.logger.Warn("accept invitation failed", "error_type", authLogErrorType(err))
		invalidLinkPage(w, "Invitation not accepted", "This invitation could not be accepted. Ask your institution for a new one.")
		return
	}
	setAccountCSRFCookie(w, "", invitationCookiePath, -1)
	if models.RolesAllowConsole(membership.Roles) {
		scope := models.TenantScope{TenantID: membership.TenantID, UserID: membership.UserID, MembershipID: membership.ID}
		if err := s.startConsoleSession(ctx, w, scope, membership.Version); err != nil {
			s.logger.Error("create console session failed", "error_type", authLogErrorType(err))
			http.Redirect(w, r, "/console/login", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/console", http.StatusSeeOther)
		return
	}
	renderConsolePage(w, http.StatusOK, consolePageData{
		Page: "joined", Title: "Welcome to " + preview.TenantName, TenantName: preview.TenantName,
		Email: preview.Email, MCPURL: MCPResource(s.baseURL),
	})
}

// ---------------------------------------------------------------------------
// Institution signup
// ---------------------------------------------------------------------------

func (s *OAuthServer) renderSignup(w http.ResponseWriter, status int, data consolePageData) {
	csrf, ok := consoleCSRF(w, signupCookiePath)
	if !ok {
		return
	}
	data.Page, data.Title, data.CSRFToken = "signup", "Create an institution", csrf
	renderConsolePage(w, status, data)
}

func (s *OAuthServer) HandleSignupGet(w http.ResponseWriter, r *http.Request) {
	if !s.SignupOpen() {
		http.NotFound(w, r)
		return
	}
	s.renderSignup(w, http.StatusOK, consolePageData{})
}

func (s *OAuthServer) HandleSignupPost(w http.ResponseWriter, r *http.Request) {
	if !s.SignupOpen() {
		http.NotFound(w, r)
		return
	}
	if !s.consolePost(w, r) {
		return
	}
	ctx := r.Context()
	name := strings.Join(strings.Fields(r.FormValue("tenant_name")), " ")
	slug := strings.TrimSpace(strings.ToLower(r.FormValue("tenant_slug")))
	email := NormalizeEmail(r.FormValue("email"))
	data := consolePageData{TenantName: name, TenantSlug: slug, Email: email}
	switch {
	case name == "" || utf8.RuneCountInString(name) > tenantNameMaxRunes || strings.ContainsAny(name, "<>\x00"):
		data.Error = "Enter the institution name (up to 120 characters)."
	case !signupSlugPattern.MatchString(slug):
		data.Error = "The identifier uses 3 to 63 lowercase letters, digits or hyphens."
	case validateEmail(email) != nil:
		data.Error = "Enter a valid email address."
	case r.FormValue("accept_terms") != "yes":
		data.Error = "Accept the terms of service to continue."
	}
	if data.Error != "" {
		s.renderSignup(w, http.StatusBadRequest, data)
		return
	}
	available, err := s.accounts.TenantSlugAvailable(ctx, slug)
	if err != nil {
		s.logger.Error("signup slug lookup failed", "error_type", authLogErrorType(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !available {
		data.Error = "This identifier is already taken. Choose another one."
		s.renderSignup(w, http.StatusConflict, data)
		return
	}
	token, err := newOpaqueToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.accounts.CreatePendingSignup(ctx, models.SignupCredential{Token: token}, email, name, slug,
		time.Now().UTC().Add(signupTTL)); err != nil {
		s.logger.Error("create pending signup failed", "error_type", authLogErrorType(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	link := s.baseURL + "/signup/complete?token=" + url.QueryEscape(token)
	if mailer, ok := s.emailSender.(InstitutionEmailSender); ok {
		if err := mailer.SendInstitutionSignup(ctx, email, link); err != nil {
			s.logger.Error("signup email failed", "error_type", authLogErrorType(err))
		}
	} else {
		s.logger.Error("signup email not sent: email delivery is not configured")
	}
	setAccountCSRFCookie(w, "", signupCookiePath, -1)
	renderConsolePage(w, http.StatusAccepted, consolePageData{
		Page: "message", Title: "Check your email",
		Message: "We sent a confirmation link to " + email + ". It expires in 30 minutes.",
	})
}

func (s *OAuthServer) HandleSignupCompleteGet(w http.ResponseWriter, r *http.Request) {
	if !s.SignupOpen() {
		http.NotFound(w, r)
		return
	}
	s.renderSignupComplete(w, r, r.URL.Query().Get("token"), http.StatusOK, "")
}

func (s *OAuthServer) pendingSignup(r *http.Request, raw string) (*models.PendingSignup, *models.User) {
	if !validRawAccountToken(raw) {
		return nil, nil
	}
	pending, err := s.accounts.GetPendingSignup(r.Context(), models.SignupCredential{Token: raw})
	if err != nil {
		return nil, nil
	}
	user, err := s.store.GetLocalUserByEmail(r.Context(), pending.Email)
	if err != nil || !s.activeLoginUser(user) {
		user = nil
	}
	return pending, user
}

func (s *OAuthServer) renderSignupComplete(w http.ResponseWriter, r *http.Request, raw string, status int, message string) {
	pending, user := s.pendingSignup(r, raw)
	if pending == nil {
		invalidLinkPage(w, "Invalid link", "This confirmation link is invalid or expired. Start again from the signup page.")
		return
	}
	csrf, ok := consoleCSRF(w, signupCookiePath)
	if !ok {
		return
	}
	renderConsolePage(w, status, consolePageData{
		Page: "signup-complete", Title: "Create " + pending.TenantName, Error: message, CSRFToken: csrf,
		Token: raw, TenantName: pending.TenantName, TenantSlug: pending.TenantSlug,
		Email: pending.Email, ExistingUser: user != nil,
	})
}

func (s *OAuthServer) HandleSignupCompletePost(w http.ResponseWriter, r *http.Request) {
	if !s.SignupOpen() {
		http.NotFound(w, r)
		return
	}
	if !s.consolePost(w, r) {
		return
	}
	ctx := r.Context()
	raw := r.FormValue("token")
	retry := func(message string) { s.renderSignupComplete(w, r, raw, http.StatusBadRequest, message) }
	pending, existing := s.pendingSignup(r, raw)
	if pending == nil {
		retry("")
		return
	}
	credential := models.SignupCredential{Token: raw}
	var membership *models.TenantMembership
	var err error
	if existing != nil {
		user, ok := s.verifyLocalPassword(w, r, pending.Email, r.FormValue("password"))
		if !ok {
			if w.Header().Get("Retry-After") == "" {
				retry("Invalid password.")
			}
			return
		}
		membership, err = s.accounts.CompleteSignup(ctx, credential, s.signupPlan, user.ID, "")
	} else {
		hash, ok := s.choosePassword(w, r, retry)
		if !ok {
			return
		}
		membership, err = s.accounts.CompleteSignup(ctx, credential, s.signupPlan, "", hash)
	}
	switch {
	case errors.Is(err, storeport.ErrTenantSlugTaken):
		invalidLinkPage(w, "Identifier taken", "Another institution took this identifier in the meantime. Start again with another identifier.")
		return
	case errors.Is(err, storeport.ErrAccountExists):
		retry("An account now exists for this email. Reload this page and sign in.")
		return
	case err != nil:
		s.logger.Error("complete signup failed", "error_type", authLogErrorType(err))
		invalidLinkPage(w, "Institution not created", "The institution could not be created. Start again from the signup page.")
		return
	}
	setAccountCSRFCookie(w, "", signupCookiePath, -1)
	scope := models.TenantScope{TenantID: membership.TenantID, UserID: membership.UserID, MembershipID: membership.ID}
	if err := s.startConsoleSession(ctx, w, scope, membership.Version); err != nil {
		s.logger.Error("create console session failed", "error_type", authLogErrorType(err))
		http.Redirect(w, r, "/console/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}
