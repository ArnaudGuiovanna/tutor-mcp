// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"rsc.io/qr"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const (
	consoleSessionCookieName = "tutor_console"
	consoleCookiePath        = "/console"
	consoleSessionTTL        = 12 * time.Hour
	consoleIdleTimeout       = 30 * time.Minute
	invitationTTL            = 7 * 24 * time.Hour
	maxInvitationsPerRequest = 50
)

// InstitutionAccountOptions configures the browser console, invitations and
// self-service institution signup.
type InstitutionAccountOptions struct {
	// SignupOpen enables /signup. Otherwise only the operator provisions
	// institutions with tutor-control-plane.
	SignupOpen bool
	// SignupPlan is the plan attached to self-service institutions.
	SignupPlan string
}

// EnableInstitutionAccounts mounts the institution account model: the console,
// invitations, optional signup, and a second factor at /authorize for roles
// that require it. Self-registration into the shared legacy tenant closes.
func (s *OAuthServer) EnableInstitutionAccounts(options InstitutionAccountOptions) error {
	accounts, ok := s.store.(storeport.InstitutionAccountStore)
	if !ok {
		return errors.New("store does not support institution accounts")
	}
	if s.hobbyAccounts {
		return errors.New("institution accounts and hobby accounts are exclusive")
	}
	if options.SignupOpen && strings.TrimSpace(options.SignupPlan) == "" {
		return errors.New("institution signup requires a plan")
	}
	s.accounts = accounts
	s.signupOpen = options.SignupOpen
	s.signupPlan = strings.TrimSpace(options.SignupPlan)
	return nil
}

// InstitutionAccountsEnabled reports whether EnableInstitutionAccounts ran.
func (s *OAuthServer) InstitutionAccountsEnabled() bool { return s.accounts != nil }

// SignupOpen reports whether self-service institution signup is enabled.
func (s *OAuthServer) SignupOpen() bool { return s.accounts != nil && s.signupOpen }

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

type consoleContext struct {
	credential models.ConsoleSessionCredential
	session    *models.ConsoleSession
	membership *models.ConsoleMembership
	principal  models.Principal
}

type consoleGate int

const (
	consoleGateOpen consoleGate = iota
	consoleGateMFAVerify
	consoleGateMFASetup
)

func (c *consoleContext) gate() consoleGate {
	switch {
	case !c.membership.MFARequired:
		return consoleGateOpen
	case !c.membership.MFAEnrolled:
		return consoleGateMFASetup
	case c.session.MFAVerifiedAt == nil:
		return consoleGateMFAVerify
	default:
		return consoleGateOpen
	}
}

func newOpaqueToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func setConsoleSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	cookie := &http.Cookie{
		Name: consoleSessionCookieName, Value: value, Path: consoleCookiePath, MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
	if maxAge < 0 {
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	http.SetCookie(w, cookie)
}

// startConsoleSession opens a session for a membership that may use the
// console. The second factor, when required, is still pending.
func (s *OAuthServer) startConsoleSession(ctx context.Context, w http.ResponseWriter, scope models.TenantScope, version int64) error {
	token, err := newOpaqueToken()
	if err != nil {
		return err
	}
	expiresAt := time.Now().UTC().Add(consoleSessionTTL)
	if err := s.accounts.CreateConsoleSession(ctx, models.ConsoleSessionCredential{Token: token},
		scope, version, nil, expiresAt); err != nil {
		return err
	}
	setConsoleSessionCookie(w, token, int(consoleSessionTTL/time.Second))
	return nil
}

// consoleSession loads and validates the browser session. On failure it
// clears the cookie, redirects to the sign-in page and returns false.
func (s *OAuthServer) consoleSession(w http.ResponseWriter, r *http.Request) (*consoleContext, bool) {
	if s.accounts == nil {
		http.NotFound(w, r)
		return nil, false
	}
	fail := func(credential *models.ConsoleSessionCredential) (*consoleContext, bool) {
		if credential != nil {
			_ = s.accounts.DeleteConsoleSession(r.Context(), *credential)
		}
		setConsoleSessionCookie(w, "", -1)
		http.Redirect(w, r, "/console/login", http.StatusSeeOther)
		return nil, false
	}
	cookie, err := r.Cookie(consoleSessionCookieName)
	if err != nil || !validRawAccountToken(cookie.Value) {
		return fail(nil)
	}
	credential := models.ConsoleSessionCredential{Token: cookie.Value}
	ctx := r.Context()
	session, err := s.accounts.GetConsoleSession(ctx, credential)
	if err != nil {
		return fail(nil)
	}
	now := time.Now().UTC()
	if !now.Before(session.ExpiresAt) || now.Sub(session.LastSeenAt) > consoleIdleTimeout {
		return fail(&credential)
	}
	membership, err := s.accounts.GetConsoleMembership(ctx, session.TenantScope())
	if err != nil || membership.Version != session.MembershipVersion || !models.RolesAllowConsole(membership.Roles) {
		return fail(&credential)
	}
	if err := s.accounts.UpdateConsoleSession(ctx, credential, now, 0, nil); err != nil {
		return fail(nil)
	}
	scope := session.TenantScope()
	return &consoleContext{
		credential: credential, session: session, membership: membership,
		principal: models.Principal{
			UserID: scope.UserID, TenantID: scope.TenantID, MembershipID: scope.MembershipID,
			LearnerID: membership.LearnerID, Roles: append([]string(nil), membership.Roles...),
			Scopes: []string{models.OAuthScopeLearner}, TokenVersion: membership.Version,
		},
	}, true
}

// requireConsole admits only sessions whose second factor is complete.
func (s *OAuthServer) requireConsole(w http.ResponseWriter, r *http.Request) (*consoleContext, bool) {
	c, ok := s.consoleSession(w, r)
	if !ok {
		return nil, false
	}
	switch c.gate() {
	case consoleGateMFASetup:
		http.Redirect(w, r, "/console/mfa/setup", http.StatusSeeOther)
		return nil, false
	case consoleGateMFAVerify:
		http.Redirect(w, r, "/console/mfa", http.StatusSeeOther)
		return nil, false
	}
	return c, true
}

// consoleCSRF issues the single-use form token for a console page.
func consoleCSRF(w http.ResponseWriter, path string) (string, bool) {
	token, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", false
	}
	setAccountCSRFCookie(w, token, path, 1800)
	return token, true
}

func (s *OAuthServer) consolePost(w http.ResponseWriter, r *http.Request) bool {
	if s.accounts == nil {
		http.NotFound(w, r)
		return false
	}
	if !parseLimitedForm(w, r, accountFormBodyLimitBytes) {
		return false
	}
	if !s.validateAccountCSRF(r) {
		http.Error(w, "forbidden: csrf check failed", http.StatusForbidden)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Sign-in and sign-out
// ---------------------------------------------------------------------------

func (s *OAuthServer) renderConsoleLogin(w http.ResponseWriter, status int, data consolePageData) {
	csrf, ok := consoleCSRF(w, consoleCookiePath)
	if !ok {
		return
	}
	data.Page, data.Title, data.CSRFToken, data.SignupOpen = "login", "Institution console", csrf, s.SignupOpen()
	renderConsolePage(w, status, data)
}

func (s *OAuthServer) HandleConsoleLoginGet(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		http.NotFound(w, r)
		return
	}
	s.renderConsoleLogin(w, http.StatusOK, consolePageData{})
}

func (s *OAuthServer) HandleConsoleLoginPost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	ctx := r.Context()
	email := NormalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")
	data := consolePageData{Email: email}
	if validateEmail(email) != nil || password == "" || len(password) > passwordMaxLen {
		data.Error = "Enter your email and password."
		s.renderConsoleLogin(w, http.StatusUnauthorized, data)
		return
	}
	user, ok := s.verifyLocalPassword(w, r, email, password)
	if !ok {
		if w.Header().Get("Retry-After") == "" {
			data.Error = "Invalid email or password."
			s.renderConsoleLogin(w, http.StatusUnauthorized, data)
		}
		return
	}
	memberships, err := s.store.ListActiveMembershipsForUser(ctx, user.ID)
	if err != nil {
		data.Error = "Internal error. Please try again."
		s.renderConsoleLogin(w, http.StatusInternalServerError, data)
		return
	}
	var eligible []models.TenantMembership
	for _, membership := range memberships {
		if models.RolesAllowConsole(membership.Roles) {
			eligible = append(eligible, membership)
		}
	}
	if len(eligible) == 0 {
		data.Error = "This account does not administer any institution. Learners use their AI client."
		s.renderConsoleLogin(w, http.StatusForbidden, data)
		return
	}
	selectedTenant := strings.TrimSpace(r.FormValue("tenant_id"))
	var selected *models.TenantMembership
	for i := range eligible {
		if (len(eligible) == 1 && selectedTenant == "") || eligible[i].TenantID == selectedTenant {
			selected = &eligible[i]
			break
		}
	}
	if selected == nil {
		for _, membership := range eligible {
			data.TenantOptions = append(data.TenantOptions, tenantOption{ID: membership.TenantID, Name: membership.TenantName})
		}
		data.Error = "Choose the institution for this session."
		s.renderConsoleLogin(w, http.StatusOK, data)
		return
	}
	scope := models.TenantScope{TenantID: selected.TenantID, UserID: selected.UserID, MembershipID: selected.ID}
	if err := s.startConsoleSession(ctx, w, scope, selected.Version); err != nil {
		s.logger.Error("create console session failed", "error_type", authLogErrorType(err))
		data.Error = "Internal error. Please try again."
		s.renderConsoleLogin(w, http.StatusInternalServerError, data)
		return
	}
	setAccountCSRFCookie(w, "", consoleCookiePath, -1)
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

// verifyLocalPassword performs exactly one budgeted bcrypt comparison for an
// email identity and returns the active, verified user it authenticates. On
// failure it records the attempt; it writes a response only when throttled or
// busy, and sets Retry-After in that case.
func (s *OAuthServer) verifyLocalPassword(w http.ResponseWriter, r *http.Request, email, password string) (*models.User, bool) {
	ctx := r.Context()
	user, lookupErr := s.store.GetLocalUserByEmail(ctx, email)
	passwordHash := dummyPasswordHash
	if lookupErr == nil && user != nil {
		passwordHash = []byte(user.PasswordHash)
	}
	passwordErr := s.bcrypt.CompareCredential(ctx, passwordHash, []byte(password),
		dummyPasswordHash, dummyMinCostPasswordHash, bcryptCost)
	if errors.Is(passwordErr, ErrBcryptBusy) {
		w.Header().Set("Retry-After", bcryptBusyRetryAfter)
		renderConsolePage(w, http.StatusServiceUnavailable, consolePageData{
			Page: "message", Title: "Temporarily busy", Error: "Password processing is at capacity. Please try again shortly.",
		})
		return nil, false
	}
	if lookupErr != nil || passwordErr != nil || !s.activeLoginUser(user) {
		count := s.loginFailures.RecordContext(ctx, email)
		if retryAfter := s.loginFailures.RetryAfter(count); retryAfter > 0 {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter/time.Second)))
			renderConsolePage(w, http.StatusTooManyRequests, consolePageData{
				Page: "message", Title: "Too many attempts", Error: "Too many failed sign-in attempts. Wait a few minutes and try again.",
			})
		}
		return nil, false
	}
	s.loginFailures.ResetContext(ctx, email)
	return user, true
}

func (s *OAuthServer) HandleConsoleLogoutPost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	if cookie, err := r.Cookie(consoleSessionCookieName); err == nil && validRawAccountToken(cookie.Value) {
		_ = s.accounts.DeleteConsoleSession(r.Context(), models.ConsoleSessionCredential{Token: cookie.Value})
	}
	setConsoleSessionCookie(w, "", -1)
	http.Redirect(w, r, "/console/login", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Second factor
// ---------------------------------------------------------------------------

func (s *OAuthServer) HandleConsoleMFAGet(w http.ResponseWriter, r *http.Request) {
	c, ok := s.consoleSession(w, r)
	if !ok {
		return
	}
	if c.gate() != consoleGateMFAVerify {
		http.Redirect(w, r, "/console", http.StatusSeeOther)
		return
	}
	s.renderConsoleMFA(w, http.StatusOK, c, "")
}

func (s *OAuthServer) renderConsoleMFA(w http.ResponseWriter, status int, c *consoleContext, message string) {
	csrf, ok := consoleCSRF(w, consoleCookiePath)
	if !ok {
		return
	}
	renderConsolePage(w, status, consolePageData{
		Page: "mfa", Title: "Two-factor authentication", Error: message, CSRFToken: csrf,
		Institution: c.membership.TenantName, UserEmail: c.membership.Email,
	})
}

func (s *OAuthServer) HandleConsoleMFAPost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	c, ok := s.consoleSession(w, r)
	if !ok {
		return
	}
	if c.gate() != consoleGateMFAVerify {
		http.Redirect(w, r, "/console", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	ok, throttled := s.checkSecondFactor(ctx, c.session.TenantScope(), r.FormValue("code"), now)
	if throttled {
		// The attempt budget is spent: end the session without checking the
		// code. The password must be entered again once the window passes.
		_ = s.accounts.DeleteConsoleSession(ctx, c.credential)
		setConsoleSessionCookie(w, "", -1)
		w.Header().Set("Retry-After", s.secondFactorRetryAfter())
		s.renderConsoleLogin(w, http.StatusTooManyRequests, consolePageData{Error: "Too many invalid codes. Sign in again in a few minutes."})
		return
	}
	if !ok {
		s.renderConsoleMFA(w, http.StatusUnauthorized, c, "Invalid or already used code.")
		return
	}
	if err := s.completeConsoleMFA(ctx, c, now); err != nil {
		s.logger.Error("record console MFA failed", "error_type", authLogErrorType(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setAccountCSRFCookie(w, "", consoleCookiePath, -1)
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

func secondFactorFailureKey(userID string) string { return "mfa:" + userID }

// checkSecondFactor verifies a TOTP or recovery code within a per-user attempt
// budget. A correct password never resets that budget, and a spent budget
// refuses before the code is checked.
func (s *OAuthServer) checkSecondFactor(ctx context.Context, scope models.TenantScope, code string, at time.Time) (ok, throttled bool) {
	key := secondFactorFailureKey(scope.UserID)
	if !s.loginFailures.AllowContext(ctx, key) {
		return false, true
	}
	if err := s.accounts.VerifySecondFactor(ctx, scope, strings.TrimSpace(code), at); err != nil {
		count := s.loginFailures.RecordContext(ctx, key)
		return false, s.loginFailures.RetryAfter(count) > 0
	}
	s.loginFailures.ResetContext(ctx, key)
	return true, false
}

func (s *OAuthServer) secondFactorRetryAfter() string {
	retry := s.loginFailures.RetryAfter(s.loginFailures.Threshold())
	if retry <= 0 {
		retry = time.Minute
	}
	return fmt.Sprintf("%d", int(retry/time.Second))
}

func (s *OAuthServer) completeConsoleMFA(ctx context.Context, c *consoleContext, at time.Time) error {
	version, err := s.accounts.EnsureMembershipMFAVerified(ctx, c.session.TenantScope(), at)
	if err != nil {
		return err
	}
	return s.accounts.UpdateConsoleSession(ctx, c.credential, at, version, &at)
}

func (s *OAuthServer) HandleConsoleMFASetupGet(w http.ResponseWriter, r *http.Request) {
	c, ok := s.consoleSession(w, r)
	if !ok {
		return
	}
	if c.gate() != consoleGateMFASetup {
		http.Redirect(w, r, "/console", http.StatusSeeOther)
		return
	}
	credentialID, secret, err := s.accounts.BeginTOTPEnrollment(r.Context(), c.principal, "Authenticator app")
	if err != nil {
		s.logger.Error("begin TOTP enrollment failed", "error_type", authLogErrorType(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderConsoleMFASetup(w, http.StatusOK, c, credentialID, secret, "")
}

func (s *OAuthServer) renderConsoleMFASetup(w http.ResponseWriter, status int, c *consoleContext, credentialID, secret, message string) {
	csrf, ok := consoleCSRF(w, consoleCookiePath)
	if !ok {
		return
	}
	otpauth := totpProvisioningURI(s.baseURL, c.membership.Email, secret)
	data := consolePageData{
		Page: "setup", Title: "Set up two-factor authentication", Error: message, CSRFToken: csrf,
		Institution: c.membership.TenantName, UserEmail: c.membership.Email,
		CredentialID: credentialID, Secret: secret, OTPAuthURI: otpauth,
	}
	if code, err := qr.Encode(otpauth, qr.M); err == nil {
		code.Scale = 6
		data.QRDataURI = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
	}
	renderConsolePage(w, status, data)
}

// totpProvisioningURI follows the Key Uri Format understood by authenticator
// apps. The issuer is the service host, so several deployments stay distinct.
func totpProvisioningURI(baseURL, email, secret string) string {
	issuer := "tutor-mcp"
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Hostname() != "" {
		issuer = "tutor-mcp " + parsed.Hostname()
	}
	query := url.Values{}
	query.Set("secret", secret)
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", "6")
	query.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+email) + "?" + query.Encode()
}

func (s *OAuthServer) HandleConsoleMFASetupPost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	c, ok := s.consoleSession(w, r)
	if !ok {
		return
	}
	if c.gate() != consoleGateMFASetup {
		http.Redirect(w, r, "/console", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	credentialID := r.FormValue("credential_id")
	now := time.Now().UTC()
	codes, err := s.accounts.ConfirmTOTPEnrollment(ctx, c.principal, credentialID, strings.TrimSpace(r.FormValue("code")), now)
	if err != nil {
		// Show a fresh seed: the previous one cannot be read back.
		credentialID, secret, beginErr := s.accounts.BeginTOTPEnrollment(ctx, c.principal, "Authenticator app")
		if beginErr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.renderConsoleMFASetup(w, http.StatusUnauthorized, c, credentialID, secret,
			"That code did not match. Remove the previous entry from your app, scan this new code and try again.")
		return
	}
	if err := s.completeConsoleMFA(ctx, c, now); err != nil {
		s.logger.Error("record console MFA failed", "error_type", authLogErrorType(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setAccountCSRFCookie(w, "", consoleCookiePath, -1)
	renderConsolePage(w, http.StatusOK, consolePageData{
		Page: "codes", Title: "Save your recovery codes", RecoveryCodes: codes,
	})
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

var inviteRoleOptions = []string{models.RoleLearner, models.RoleTrainer, models.RolePedagogyManager, models.RoleAdmin, models.RoleOwner}

func (c *consoleContext) roleOptions() []string {
	if slices.Contains(c.principal.Roles, models.RoleOwner) {
		return inviteRoleOptions
	}
	return inviteRoleOptions[:len(inviteRoleOptions)-1]
}

func (s *OAuthServer) HandleConsoleHome(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireConsole(w, r)
	if !ok {
		return
	}
	s.renderConsoleMembers(w, r.Context(), http.StatusOK, c, "", "", nil)
}

func (s *OAuthServer) renderConsoleMembers(w http.ResponseWriter, ctx context.Context, status int, c *consoleContext, message, errMsg string, links []consoleInviteLink) {
	csrf, ok := consoleCSRF(w, consoleCookiePath)
	if !ok {
		return
	}
	data := consolePageData{
		Page: "members", Title: c.membership.TenantName, Message: message, Error: errMsg, CSRFToken: csrf,
		Institution: c.membership.TenantName, UserEmail: c.membership.Email,
		MCPURL: MCPResource(s.baseURL), InviteLinks: links, RoleOptions: c.roleOptions(),
		IsOwner: slices.Contains(c.principal.Roles, models.RoleOwner),
		CanManage: c.principal.Authorize(models.PermissionMembershipManage,
			models.AuthorizationResource{TenantID: c.principal.TenantID}),
	}
	if data.CanManage {
		members, err := s.accounts.ListTenantMembers(ctx, c.principal)
		if err != nil {
			s.logger.Error("list members failed", "error_type", authLogErrorType(err))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, member := range members {
			roles := make(map[string]bool, len(member.Roles))
			for _, role := range member.Roles {
				roles[role] = true
			}
			data.Members = append(data.Members, consoleMemberRow{
				MembershipID: member.MembershipID, Email: member.Email, Status: member.Status,
				MFAEnrolled: member.MFAEnrolled, Self: member.MembershipID == c.principal.MembershipID,
				Roles: roles, RoleList: strings.Join(member.Roles, ", "),
			})
		}
		invitations, err := s.accounts.ListTenantInvitations(ctx, c.principal)
		if err != nil {
			s.logger.Error("list invitations failed", "error_type", authLogErrorType(err))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, invitation := range invitations {
			data.Invitations = append(data.Invitations, consoleInvitationRow{
				ID: invitation.ID, Email: invitation.Email,
				Roles: strings.Join(invitation.Roles, ", "), ExpiresAt: invitation.ExpiresAt,
			})
		}
	}
	renderConsolePage(w, status, data)
}

// selectedRoles returns the submitted roles in canonical order, restricted to
// the roles this actor may assign.
func (c *consoleContext) selectedRoles(submitted []string) ([]string, error) {
	allowed := c.roleOptions()
	var roles []string
	for _, role := range inviteRoleOptions {
		if slices.Contains(submitted, role) {
			if !slices.Contains(allowed, role) {
				return nil, fmt.Errorf("you cannot assign the %s role", role)
			}
			roles = append(roles, role)
		}
	}
	for _, role := range submitted {
		if !slices.Contains(inviteRoleOptions, role) {
			return nil, fmt.Errorf("unknown role")
		}
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("choose at least one role")
	}
	return roles, nil
}

func parseInvitationEmails(raw string) ([]string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';' || r == ' ' || r == '\t'
	})
	seen := map[string]bool{}
	var emails []string
	for _, field := range fields {
		email := NormalizeEmail(field)
		if email == "" || seen[email] {
			continue
		}
		if validateEmail(email) != nil {
			return nil, fmt.Errorf("invalid email address: %s", email)
		}
		seen[email] = true
		emails = append(emails, email)
	}
	if len(emails) == 0 {
		return nil, fmt.Errorf("enter at least one email address")
	}
	if len(emails) > maxInvitationsPerRequest {
		return nil, fmt.Errorf("at most %d invitations at a time", maxInvitationsPerRequest)
	}
	return emails, nil
}

func (s *OAuthServer) HandleConsoleInvitePost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	c, ok := s.requireConsole(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	roles, err := c.selectedRoles(r.Form["roles"])
	if err != nil {
		s.renderConsoleMembers(w, ctx, http.StatusBadRequest, c, "", "Invitation not sent: "+err.Error()+".", nil)
		return
	}
	emails, err := parseInvitationEmails(r.FormValue("emails"))
	if err != nil {
		s.renderConsoleMembers(w, ctx, http.StatusBadRequest, c, "", "Invitation not sent: "+err.Error()+".", nil)
		return
	}
	// Invitation links go to the invitee's mailbox only. Holding one stands
	// for owning the address, so the inviter never sees it.
	mailer, canMail := s.emailSender.(InstitutionEmailSender)
	if !canMail {
		s.renderConsoleMembers(w, ctx, http.StatusServiceUnavailable, c, "", "Invitation not sent: email delivery is not configured on this server.", nil)
		return
	}
	expiresAt := time.Now().UTC().Add(invitationTTL)
	links := make([]consoleInviteLink, 0, len(emails))
	for _, email := range emails {
		invitation, rawToken, err := s.store.CreateTenantInvitation(ctx, c.principal, email, roles, expiresAt)
		if err != nil {
			s.logger.Error("create invitation failed", "error_type", authLogErrorType(err))
			links = append(links, consoleInviteLink{Email: email, Error: "could not create the invitation"})
			continue
		}
		link := s.baseURL + "/invite?token=" + url.QueryEscape(rawToken)
		if err := mailer.SendInvitation(ctx, email, link); err != nil {
			s.logger.Warn("invitation email failed", "error_type", authLogErrorType(err))
			_ = s.accounts.RevokeTenantInvitation(ctx, c.principal, invitation.ID)
			links = append(links, consoleInviteLink{Email: email, Error: "the email could not be sent; try again later"})
			continue
		}
		links = append(links, consoleInviteLink{Email: email, Mailed: true})
	}
	s.renderConsoleMembers(w, ctx, http.StatusOK, c, fmt.Sprintf("%d invitation(s) created.", len(emails)), "", links)
}

func (s *OAuthServer) HandleConsoleMemberUpdatePost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	c, ok := s.requireConsole(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	roles, err := c.selectedRoles(r.Form["roles"])
	if err == nil {
		err = s.accounts.UpdateTenantMember(ctx, c.principal, r.FormValue("membership_id"), r.FormValue("status"), roles)
	}
	if err != nil {
		s.renderConsoleMembers(w, ctx, http.StatusBadRequest, c, "", "Change refused: "+memberErrorMessage(err)+".", nil)
		return
	}
	if r.FormValue("membership_id") == c.principal.MembershipID {
		// A change to one's own roles bumps the membership version.
		http.Redirect(w, r, "/console/login", http.StatusSeeOther)
		return
	}
	s.renderConsoleMembers(w, ctx, http.StatusOK, c, "Member updated.", "", nil)
}

func memberErrorMessage(err error) string {
	switch {
	case errors.Is(err, storeport.ErrInvalidPrincipal):
		return "your role does not allow this change"
	case errors.Is(err, storeport.ErrNotFound):
		return "member not found"
	}
	message := err.Error()
	if rest, ok := strings.CutPrefix(message, "update member: "); ok {
		return rest
	}
	if strings.HasPrefix(message, "you cannot") || strings.HasPrefix(message, "choose at least") || message == "unknown role" {
		return message
	}
	return "invalid change"
}

func (s *OAuthServer) HandleConsoleInvitationRevokePost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	c, ok := s.requireConsole(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := s.accounts.RevokeTenantInvitation(ctx, c.principal, r.FormValue("invitation_id")); err != nil {
		s.renderConsoleMembers(w, ctx, http.StatusBadRequest, c, "", "Invitation not revoked: "+memberErrorMessage(err)+".", nil)
		return
	}
	s.renderConsoleMembers(w, ctx, http.StatusOK, c, "Invitation revoked.", "", nil)
}

// verifyAuthorizeSecondFactor requires a second factor at /authorize for a
// membership whose roles need one. It writes the response on failure and
// returns the membership version to bind into the authorization code.
func (s *OAuthServer) verifyAuthorizeSecondFactor(w http.ResponseWriter, r *http.Request, data authPageData, scope models.TenantScope) (int64, bool) {
	ctx := r.Context()
	enrolled, err := s.accounts.HasConfirmedTOTP(ctx, scope)
	if err != nil {
		renderAuthPage(w, data, "Internal error. Please try again.", "login")
		return 0, false
	}
	if !enrolled {
		renderAuthPageStatus(w, http.StatusForbidden, data,
			"Your role requires two-factor authentication. Set it up first in the institution console at "+s.baseURL+"/console.", "login")
		return 0, false
	}
	code := strings.TrimSpace(r.FormValue("totp_code"))
	if code == "" {
		renderAuthPage(w, data, "Enter the code from your authenticator app. Your role requires it.", "login")
		return 0, false
	}
	now := time.Now().UTC()
	ok, throttled := s.checkSecondFactor(ctx, scope, code, now)
	if throttled {
		w.Header().Set("Retry-After", s.secondFactorRetryAfter())
		renderAuthPageStatus(w, http.StatusTooManyRequests, data, "Too many invalid authentication codes. Try again in a few minutes.", "login")
		return 0, false
	}
	if !ok {
		renderAuthPageStatus(w, http.StatusUnauthorized, data, "Invalid or already used authentication code.", "login")
		return 0, false
	}
	version, err := s.accounts.EnsureMembershipMFAVerified(ctx, scope, now)
	if err != nil {
		renderAuthPage(w, data, "Internal error. Please try again.", "login")
		return 0, false
	}
	return version, true
}
