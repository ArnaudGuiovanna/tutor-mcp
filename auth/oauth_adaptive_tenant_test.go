// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A user with two organizations must still be able to sign in once someone
// has filled the account's failure counter. Before the fix the correct
// password hit a "choose an organization" page that ignored tenant_id, so
// anyone knowing the address could keep the owner out indefinitely.
//
// The second membership's learner profile carries the synthetic address that
// organization invitations create; the device challenge must still reach the
// account's real mailbox.
func TestAuthorizePost_LoginFailureThresholdLetsMultiOrganizationOwnerChooseTenant(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	userID := seedLearner(t, store, "bob@x.com", "correct-password")

	ctx := context.Background()
	raw := store.RawDB()
	seedOrganizationProfile(t, store, userID)
	memberships, err := store.ListActiveMembershipsForUser(ctx, userID)
	if err != nil || len(memberships) != 2 {
		t.Fatalf("seeded memberships=%+v err=%v", memberships, err)
	}

	postLogin := adaptiveLoginPoster(t, s)

	for i := 0; i < 5; i++ {
		postLogin("wrong-password", "")
	}
	if s.loginFailures.Allow("bob@x.com") {
		t.Fatal("failure threshold was not reached")
	}

	// The correct password first asks which organization the session is for,
	// with the same page the unlocked multi-organization path renders.
	rec := postLogin("correct-password", "")
	body := rec.Body.String()
	if !strings.Contains(body, "Choose the organization for this session.") || !strings.Contains(body, `name="tenant_id"`) ||
		!strings.Contains(body, `value="tenant-b"`) || !strings.Contains(body, `value="tenant_legacy"`) {
		t.Fatalf("locked multi-organization login did not offer a tenant choice: status=%d body=%q", rec.Code, body)
	}

	// An organization the user does not belong to is refused without a challenge.
	rec = postLogin("correct-password", "tenant-unknown")
	if !strings.Contains(rec.Body.String(), "The selected organization is not available.") {
		t.Fatalf("unknown tenant status=%d body=%q", rec.Code, rec.Body.String())
	}
	sender := s.emailSender.(*testEmailSender)
	if len(sender.challengeLinks) != 0 {
		t.Fatalf("unknown tenant sent a challenge: %v", sender.challengeLinks)
	}

	// Choosing an organization runs the device challenge for that membership.
	rec = postLogin("correct-password", "tenant-b")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("adaptive challenge status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(sender.challengeLinks) != 1 || len(sender.challengeTo) != 1 || sender.challengeTo[0] != "bob@x.com" {
		t.Fatalf("challenge deliveries=%v recipients=%v", sender.challengeLinks, sender.challengeTo)
	}
	var challengeLearner string
	if err := raw.QueryRowContext(ctx, `SELECT learner_id FROM login_challenges`).Scan(&challengeLearner); err != nil {
		t.Fatalf("read login challenge: %v", err)
	}
	if challengeLearner != "learner-b" {
		t.Fatalf("challenge bound to learner %q, want learner-b", challengeLearner)
	}

	// Approving the email link trusts this device for the chosen membership,
	// and the owner then completes the authorization.
	challengeURL, err := url.Parse(sender.challengeLinks[0])
	if err != nil {
		t.Fatal(err)
	}
	getRec := httptest.NewRecorder()
	s.HandleLoginChallengeGet(getRec, httptest.NewRequest(http.MethodGet, challengeURL.String(), nil))
	var accountCookie *http.Cookie
	for _, cookie := range getRec.Result().Cookies() {
		if cookie.Name == accountCSRFCookieName {
			accountCookie = cookie
		}
	}
	if accountCookie == nil {
		t.Fatalf("challenge page status=%d omitted CSRF cookie", getRec.Code)
	}
	confirm := url.Values{"token": {challengeURL.Query().Get("token")}, "csrf_token": {accountCookie.Value}}
	confirmReq := httptest.NewRequest(http.MethodPost, "/login-challenge", strings.NewReader(confirm.Encode()))
	confirmReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmReq.AddCookie(accountCookie)
	confirmRec := httptest.NewRecorder()
	s.HandleLoginChallengePost(confirmRec, confirmReq)
	var trustedCookie *http.Cookie
	for _, cookie := range confirmRec.Result().Cookies() {
		if cookie.Name == trustedLoginDeviceCookieName {
			trustedCookie = cookie
		}
	}
	if trustedCookie == nil {
		t.Fatalf("challenge confirmation status=%d did not trust the device", confirmRec.Code)
	}
	rec = postLogin("correct-password", "tenant-b", trustedCookie)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://good.example/cb?") {
		t.Fatalf("trusted owner sign-in status=%d location=%q body=%q", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if !s.loginFailures.Allow("bob@x.com") {
		t.Fatal("successful sign-in did not reset the failure signal")
	}
}

// seedOrganizationProfile gives userID an active learner membership in
// "tenant-b", shaped like the profiles organization invitations create: the
// learner row carries a synthetic address, the real one stays on users.
func seedOrganizationProfile(t *testing.T, store interface{ RawDB() *sql.DB }, userID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	raw := store.RawDB()
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, slug, name, status, region, policy_json, created_at, updated_at)
			VALUES ('tenant-b', 'tenant-b', 'Tenant B', 'active', 'default', '{}', ?, ?)`, []any{now, now}},
		{`INSERT INTO tenant_memberships (id, tenant_id, user_id, learner_id, roles_json, status, version, created_at, updated_at, mfa_required)
			VALUES ('membership-b', 'tenant-b', ?, NULL, '["learner"]', 'active', 1, ?, ?, 0)`, []any{userID, now, now}},
		{`INSERT INTO learners (id, email, password_hash, objective, webhook_url, profile_json, created_at, email_verified_at, tenant_id, user_id, membership_id, identity_mode)
			VALUES ('learner-b', 'tenant-b+learner-b@profile.invalid', '', '', '', '{}', ?, ?, 'tenant-b', ?, 'membership-b', 'email')`, []any{now, now, userID}},
		{`UPDATE tenant_memberships SET learner_id = 'learner-b' WHERE id = 'membership-b'`, nil},
	} {
		if _, err := raw.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatalf("seed organization profile: %v", err)
		}
	}
}

func adaptiveLoginPoster(t *testing.T, s *OAuthServer) func(password, tenantID string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return func(password, tenantID string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		csrf, err := generateCSRFToken()
		if err != nil {
			t.Fatalf("generate csrf: %v", err)
		}
		form := url.Values{}
		form.Set("csrf_token", csrf)
		form.Set("mode", "login")
		form.Set("client_id", "cid")
		form.Set("redirect_uri", "https://good.example/cb")
		form.Set("response_type", "code")
		form.Set("resource", testOAuthResource)
		form.Set("code_challenge", "ch")
		form.Set("code_challenge_method", "S256")
		form.Set("email", "bob@x.com")
		form.Set("password", password)
		form.Set("approve_client", "yes")
		if tenantID != "" {
			form.Set("tenant_id", tenantID)
		}
		req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "csrf_token", Value: csrf})
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		s.HandleAuthorizePost(rec, req)
		return rec
	}
}

// The common institution case: the user's only membership is an organization
// profile. Before the fix the challenge could not be stored outside the legacy
// tenant (503) and would have been mailed to the synthetic profile address.
func TestAuthorizePost_LoginFailureThresholdChallengesOrganizationLearner(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	userID := seedLearner(t, store, "bob@x.com", "correct-password")
	seedOrganizationProfile(t, store, userID)
	ctx := context.Background()
	raw := store.RawDB()
	if _, err := raw.ExecContext(ctx, `UPDATE tenant_memberships SET status = 'suspended' WHERE tenant_id = 'tenant_legacy'`); err != nil {
		t.Fatal(err)
	}
	memberships, err := store.ListActiveMembershipsForUser(ctx, userID)
	if err != nil || len(memberships) != 1 || memberships[0].TenantID != "tenant-b" {
		t.Fatalf("active memberships=%+v err=%v", memberships, err)
	}

	postLogin := adaptiveLoginPoster(t, s)
	for i := 0; i < 5; i++ {
		postLogin("wrong-password", "")
	}
	rec := postLogin("correct-password", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("adaptive challenge status=%d, want 202", rec.Code)
	}
	sender := s.emailSender.(*testEmailSender)
	if len(sender.challengeTo) != 1 || sender.challengeTo[0] != "bob@x.com" {
		t.Fatalf("challenge recipients=%v, want the account address", sender.challengeTo)
	}
	var tenantID, learnerID string
	if err := raw.QueryRowContext(ctx, `SELECT tenant_id, learner_id FROM login_challenges`).Scan(&tenantID, &learnerID); err != nil {
		t.Fatalf("read login challenge: %v", err)
	}
	if tenantID != "tenant-b" || learnerID != "learner-b" {
		t.Fatalf("challenge scope=%s/%s, want tenant-b/learner-b", tenantID, learnerID)
	}
}
