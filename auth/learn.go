// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const learnerSessionCookieName = "tutor_learn"

// A domain-separated digest in the existing revocable browser session store
// prevents a learner token from being used as a console token, even if copied
// into the console cookie manually. Neither cookie is an OAuth credential.
func learnerSessionCredential(raw string) models.ConsoleSessionCredential {
	return models.ConsoleSessionCredential{Token: "learner-browser:" + raw}
}

func setLearnerSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	cookie := &http.Cookie{Name: learnerSessionCookieName, Value: value, Path: "/learn", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
	if maxAge < 0 {
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	http.SetCookie(w, cookie)
}

func (s *OAuthServer) learnerSession(w http.ResponseWriter, r *http.Request, requireMFA bool) (*consoleContext, bool) {
	if s.accounts == nil {
		http.NotFound(w, r)
		return nil, false
	}
	fail := func() (*consoleContext, bool) {
		setLearnerSessionCookie(w, "", -1)
		http.Redirect(w, r, "/learn/login", http.StatusSeeOther)
		return nil, false
	}
	cookie, err := r.Cookie(learnerSessionCookieName)
	if err != nil || !validRawAccountToken(cookie.Value) {
		return fail()
	}
	credential := learnerSessionCredential(cookie.Value)
	session, err := s.accounts.GetConsoleSession(r.Context(), credential)
	if err != nil {
		return fail()
	}
	now := time.Now().UTC()
	membership, err := s.accounts.GetConsoleMembership(r.Context(), session.TenantScope())
	if err != nil || !now.Before(session.ExpiresAt) || now.Sub(session.LastSeenAt) > consoleIdleTimeout || membership.Version != session.MembershipVersion || !slices.Contains(membership.Roles, models.RoleLearner) || membership.LearnerID == "" {
		_ = s.accounts.DeleteConsoleSession(r.Context(), credential)
		return fail()
	}
	if err := s.accounts.UpdateConsoleSession(r.Context(), credential, now, 0, nil); err != nil {
		return fail()
	}
	c := &consoleContext{credential: credential, session: session, membership: membership,
		principal: models.Principal{TenantID: session.TenantID, UserID: session.UserID, MembershipID: session.MembershipID, LearnerID: membership.LearnerID, Roles: append([]string(nil), membership.Roles...), TokenVersion: membership.Version, Scopes: []string{models.OAuthScopeLearner}}}
	if requireMFA && c.gate() != consoleGateOpen {
		path := "/learn/mfa"
		if c.gate() == consoleGateMFASetup {
			path += "/setup"
		}
		http.Redirect(w, r, path, http.StatusSeeOther)
		return nil, false
	}
	return c, true
}

func (s *OAuthServer) renderLearn(w http.ResponseWriter, status int, data learnPageData) {
	csrf, ok := consoleCSRF(w, "/learn")
	if !ok {
		return
	}
	key, err := newOpaqueToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.CSRFToken, data.RetryKey, data.MCPURL = csrf, key, s.baseURL+"/mcp"
	renderLearnPage(w, status, data)
}

func (s *OAuthServer) HandleLearnerLoginGet(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		http.NotFound(w, r)
		return
	}
	s.renderLearn(w, http.StatusOK, learnPageData{Page: "login", Title: "Your learning space"})
}

func (s *OAuthServer) HandleLearnerLoginPost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	data := learnPageData{Page: "login", Title: "Your learning space", Email: NormalizeEmail(r.FormValue("email"))}
	password := r.FormValue("password")
	if validateEmail(data.Email) != nil || password == "" || len(password) > passwordMaxLen {
		data.Error = "Enter your email and password."
		s.renderLearn(w, http.StatusUnauthorized, data)
		return
	}
	user, ok := s.verifyLocalPassword(w, r, data.Email, password)
	if !ok {
		if w.Header().Get("Retry-After") == "" {
			data.Error = "Invalid email or password."
			s.renderLearn(w, http.StatusUnauthorized, data)
		}
		return
	}
	memberships, err := s.store.ListActiveMembershipsForUser(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var eligible []models.TenantMembership
	for _, m := range memberships {
		if slices.Contains(m.Roles, models.RoleLearner) && m.LearnerID != "" {
			eligible = append(eligible, m)
		}
	}
	if len(eligible) == 0 {
		data.Error = "This account has no active learner membership."
		s.renderLearn(w, http.StatusForbidden, data)
		return
	}
	var selected *models.TenantMembership
	for i := range eligible {
		if eligible[i].TenantID == r.FormValue("tenant_id") || (len(eligible) == 1 && r.FormValue("tenant_id") == "") {
			selected = &eligible[i]
			break
		}
	}
	if selected == nil {
		for _, m := range eligible {
			data.TenantOptions = append(data.TenantOptions, tenantOption{ID: m.TenantID, Name: m.TenantName})
		}
		data.Error = "Choose the institution for this session."
		s.renderLearn(w, http.StatusOK, data)
		return
	}
	token, err := newOpaqueToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	scope := models.TenantScope{TenantID: selected.TenantID, UserID: selected.UserID, MembershipID: selected.ID}
	if err := s.accounts.CreateConsoleSession(r.Context(), learnerSessionCredential(token), scope, selected.Version, nil, time.Now().UTC().Add(consoleSessionTTL)); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setLearnerSessionCookie(w, token, int(consoleSessionTTL/time.Second))
	setAccountCSRFCookie(w, "", "/learn", -1)
	http.Redirect(w, r, "/learn", http.StatusSeeOther)
}

func (s *OAuthServer) HandleLearnerLogoutPost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	if cookie, err := r.Cookie(learnerSessionCookieName); err == nil && validRawAccountToken(cookie.Value) {
		_ = s.accounts.DeleteConsoleSession(r.Context(), learnerSessionCredential(cookie.Value))
	}
	setLearnerSessionCookie(w, "", -1)
	http.Redirect(w, r, "/learn/login", http.StatusSeeOther)
}

func (s *OAuthServer) HandleLearnerHome(w http.ResponseWriter, r *http.Request) {
	c, ok := s.learnerSession(w, r, true)
	if !ok {
		return
	}
	store, ok := s.store.(storeport.LearnerFormationStore)
	if !ok {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	mine := r.URL.Query().Get("mine") == "1"
	page, err := store.ListLearnerFormations(r.Context(), c.principal, mine, r.URL.Query().Get("after"), 20)
	if err != nil {
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	s.renderLearn(w, http.StatusOK, learnPageData{Page: "catalog", Title: "Your formations", Institution: c.membership.TenantName, Email: c.membership.Email, Formations: page.Items, NextAfter: page.NextAfter, Mine: mine})
}

func (s *OAuthServer) HandleLearnerFormation(w http.ResponseWriter, r *http.Request) {
	c, ok := s.learnerSession(w, r, true)
	if !ok {
		return
	}
	store, ok := s.store.(storeport.LearnerFormationStore)
	if !ok {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	detail, err := store.GetLearnerFormation(r.Context(), c.principal, r.PathValue("cohortID"))
	if err != nil {
		http.Error(w, "formation unavailable", http.StatusNotFound)
		return
	}
	s.renderLearn(w, http.StatusOK, learnPageData{Page: "program", Title: detail.Offering.Name, Institution: c.membership.TenantName, Email: c.membership.Email, Formations: []models.LearnerFormation{detail.Offering}, Detail: detail})
}

func (s *OAuthServer) HandleLearnerEnrollmentPost(w http.ResponseWriter, r *http.Request) {
	if !s.consolePost(w, r) {
		return
	}
	c, ok := s.learnerSession(w, r, true)
	if !ok {
		return
	}
	store, ok := s.store.(storeport.LearnerFormationStore)
	if !ok {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	key, cohortID := r.FormValue("idempotency_key"), r.FormValue("cohort_id")
	var err error
	switch r.FormValue("action") {
	case "join":
		_, _, err = store.JoinFormation(r.Context(), c.principal, key, cohortID)
	case "leave":
		_, _, err = store.LeaveFormation(r.Context(), c.principal, key, cohortID)
	default:
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.renderLearn(w, http.StatusConflict, learnPageData{Page: "message", Title: "Enrollment could not be updated", Error: "Refresh the catalog and check the admission policy and available seats. Your history is preserved."})
		return
	}
	http.Redirect(w, r, "/learn?mine=1", http.StatusSeeOther)
}

func (s *OAuthServer) HandleLearnerMFA(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && !s.consolePost(w, r) {
		return
	}
	c, ok := s.learnerSession(w, r, false)
	if !ok {
		return
	}
	if c.gate() == consoleGateOpen {
		http.Redirect(w, r, "/learn", http.StatusSeeOther)
		return
	}
	setup := c.gate() == consoleGateMFASetup
	data := learnPageData{Page: "mfa", Title: "Two-factor authentication", Institution: c.membership.TenantName, Email: c.membership.Email}
	if setup {
		data.Page = "setup"
		data.Title = "Set up two-factor authentication"
	}
	if r.Method == http.MethodPost {
		now := time.Now().UTC()
		var codes []string
		var err error
		verified, throttled := false, false
		if setup {
			codes, err = s.accounts.ConfirmTOTPEnrollment(r.Context(), c.principal, r.FormValue("credential_id"), strings.TrimSpace(r.FormValue("code")), now)
			verified = err == nil
		} else {
			verified, throttled = s.checkSecondFactor(r.Context(), c.session.TenantScope(), r.FormValue("code"), now)
		}
		if throttled {
			_ = s.accounts.DeleteConsoleSession(r.Context(), c.credential)
			setLearnerSessionCookie(w, "", -1)
			w.Header().Set("Retry-After", s.secondFactorRetryAfter())
			http.Error(w, "too many invalid codes; sign in again later", http.StatusTooManyRequests)
			return
		}
		if verified {
			if err := s.completeConsoleMFA(r.Context(), c, now); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if setup {
				s.renderLearn(w, http.StatusOK, learnPageData{Page: "codes", Title: "Save your recovery codes", RecoveryCodes: codes})
				return
			}
			http.Redirect(w, r, "/learn", http.StatusSeeOther)
			return
		}
		data.Error = "Invalid or already used code. For setup, use the new secret below."
	}
	if setup {
		var err error
		data.CredentialID, data.Secret, err = s.accounts.BeginTOTPEnrollment(r.Context(), c.principal, "Authenticator app")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	status := http.StatusOK
	if data.Error != "" {
		status = http.StatusUnauthorized
	}
	s.renderLearn(w, status, data)
}
