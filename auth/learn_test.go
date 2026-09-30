// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

var retryKeyPattern = regexp.MustCompile(`name="idempotency_key" value="([^"]+)"`)

func TestLearnerPortalEnrollmentCSRFAndSessionIsolation(t *testing.T) {
	_, store, server := institutionTestServer(t, false)
	ctx := t.Context()
	ownerID := seedLearner(t, store, "owner@portal.test", "owner-password-2026")
	owner, err := store.GetPrincipalForLearner(ctx, ownerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMembershipAuthorization(ctx, owner.TenantScope(), models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordMembershipMFAVerification(ctx, owner.TenantScope(), time.Now()); err != nil {
		t.Fatal(err)
	}
	owner, err = store.GetPrincipalForLearner(ctx, ownerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	f, v, err := store.CreateFormationDraft(ctx, owner, "Shared course <script>alert(1)</script>", "Program")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddFormationConceptsIdempotent(ctx, owner, "content", v.ID, nil, []models.FormationConceptInput{{StableKey: "numbers", Label: "Numbers", Description: "Compare numbers"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishFormationVersion(ctx, owner, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetFormationEnrollmentPolicy(ctx, owner, f.ID, "open"); err != nil {
		t.Fatal(err)
	}
	c, err := store.CreateCohort(ctx, owner, v.ID, "Autumn", 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	learnerID := seedLearner(t, store, "learner@portal.test", "learner-password-2026")
	student, err := store.GetPrincipalForLearner(ctx, learnerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBrowser(t, server)
	resp, page := b.get("/learn")
	if resp.Request.URL.Path != "/learn/login" {
		t.Fatal("anonymous portal open")
	}
	resp, page = b.post("/learn/login", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "email": {"learner@portal.test"}, "password": {"learner-password-2026"}})
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/learn" || !strings.Contains(page, "Autumn") || strings.Contains(page, "<script>alert") || strings.Contains(page, "Legacy recovery") {
		t.Fatalf("portal: %d %s %.600s", resp.StatusCode, resp.Request.URL, page)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("private catalog can be cached")
	}
	learnURL, _ := url.Parse(server.URL + "/learn")
	var raw string
	for _, c := range b.client.Jar.Cookies(learnURL) {
		if c.Name == learnerSessionCookieName {
			raw = c.Value
		}
	}
	if raw == "" {
		t.Fatal("missing learner session")
	}
	copyBrowser := newTestBrowser(t, server)
	copyBrowser.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/console/test-api", nil)
	req.AddCookie(&http.Cookie{Name: consoleSessionCookieName, Value: raw})
	if resp, _ := copyBrowser.do(req); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("learner token used as console token: %d", resp.StatusCode)
	}
	form := url.Values{"csrf_token": {field(t, csrfPattern, page)}, "idempotency_key": {field(t, retryKeyPattern, page)}, "cohort_id": {c.ID}, "action": {"join"}}
	if resp, _ := b.post("/learn/enrollment", url.Values{"cohort_id": {c.ID}, "action": {"join"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatal("write without csrf")
	}
	resp, page = b.post("/learn/enrollment", form)
	if resp.StatusCode != 200 || !strings.Contains(page, "Leave and keep my progress") {
		t.Fatalf("join: %d %.500s", resp.StatusCode, page)
	}
	if resp, _ := b.post("/learn/enrollment", form); resp.StatusCode != http.StatusForbidden {
		t.Fatal("csrf replay accepted")
	}
	before, err := store.GetLearnerFormation(ctx, student, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, program := b.get("/learn/formations/" + c.ID)
	if resp.StatusCode != 200 || !strings.Contains(program, "Compare numbers") {
		t.Fatalf("program: %d %.600s", resp.StatusCode, program)
	}
	resp, page = b.post("/learn/enrollment", url.Values{"csrf_token": {field(t, csrfPattern, program)}, "idempotency_key": {"web-leave"}, "cohort_id": {c.ID}, "action": {"leave"}})
	if resp.StatusCode != 200 || !strings.Contains(page, "previous progress is saved") {
		t.Fatalf("leave: %d %.600s", resp.StatusCode, page)
	}
	resp, _ = b.post("/learn/enrollment", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "idempotency_key": {"web-rejoin"}, "cohort_id": {c.ID}, "action": {"join"}})
	if resp.StatusCode != 200 {
		t.Fatalf("rejoin: %d", resp.StatusCode)
	}
	after, err := store.GetLearnerFormation(ctx, student, c.ID)
	if err != nil || after.Offering.Enrollment.DomainID != before.Offering.Enrollment.DomainID {
		t.Fatalf("rejoin history: %+v %v", after, err)
	}
	resp, page = b.get("/learn/badges?enrollment_id=" + after.Offering.Enrollment.ID)
	if resp.StatusCode != 200 || !strings.Contains(page, "No badges earned") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("badges page: %d %s", resp.StatusCode, page)
	}
	if resp, _ := b.get("/learn/badges?enrollment_id=missing"); resp.StatusCode != 404 {
		t.Fatal("badge existence disclosure")
	}
	if _, err := store.SetMembershipAuthorization(ctx, student.TenantScope(), models.MembershipStatusSuspended, []string{models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	resp, _ = b.get("/learn")
	if resp.Request.URL.Path != "/learn/login" {
		t.Fatalf("revoked session survived: %s", resp.Request.URL.Path)
	}
	if resp, _ := b.get("/learn/badges?enrollment_id=" + after.Offering.Enrollment.ID); resp.Request.URL.Path != "/learn/login" {
		t.Fatal("revoked badge session survived")
	}
}

func TestLearnerPortalRequiresMFAForDualRole(t *testing.T) {
	_, store, server := institutionTestServer(t, false)
	id := seedLearner(t, store, "dual@portal.test", "dual-password-2026")
	p, err := store.GetPrincipalForLearner(t.Context(), id, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMembershipAuthorization(t.Context(), p.TenantScope(), models.MembershipStatusActive, []string{models.RolePedagogyManager, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	b := newTestBrowser(t, server)
	_, page := b.get("/learn/login")
	resp, page := b.post("/learn/login", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "email": {"dual@portal.test"}, "password": {"dual-password-2026"}})
	if resp.Request.URL.Path != "/learn/mfa/setup" {
		t.Fatalf("dual role bypassed MFA: %s", resp.Request.URL.Path)
	}
	secret := field(t, secretPattern, page)
	resp, page = b.post("/learn/mfa/setup", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "credential_id": {field(t, credentialPattern, page)}, "code": {testTOTP(t, secret, time.Now().Add(-30*time.Second))}})
	if resp.StatusCode != 200 || !strings.Contains(page, "Save your recovery codes") {
		t.Fatalf("setup: %d %.500s", resp.StatusCode, page)
	}
	resp, page = b.get("/learn")
	if resp.Request.URL.Path != "/learn" {
		t.Fatalf("verified session: %s", resp.Request.URL.Path)
	}
	b.post("/learn/logout", url.Values{"csrf_token": {field(t, csrfPattern, page)}})
	_, page = b.get("/learn/login")
	resp, page = b.post("/learn/login", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "email": {"dual@portal.test"}, "password": {"dual-password-2026"}})
	if resp.Request.URL.Path != "/learn/mfa" {
		t.Fatalf("new session reused old MFA: %s", resp.Request.URL.Path)
	}
	resp, _ = b.post("/learn/mfa", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "code": {testTOTP(t, secret, time.Now())}})
	if resp.Request.URL.Path != "/learn" {
		t.Fatalf("MFA verification: %s", resp.Request.URL.Path)
	}
}
