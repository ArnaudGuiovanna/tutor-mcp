// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"tutor-mcp/models"
)

func TestInstitutionProgressConsoleSessionAndAssignments(t *testing.T) {
	_, store, server := institutionTestServer(t, false)
	ctx := t.Context()
	ownerID := seedLearner(t, store, "owner@progress.test", "owner-password-2026")
	owner, err := store.GetPrincipalForLearner(ctx, ownerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMembershipAuthorization(ctx, owner.TenantScope(), models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	b := newTestBrowser(t, server)
	resp, page := b.get("/console/progress")
	if resp.Request.URL.Path != "/console/login" {
		t.Fatal("anonymous progress access")
	}
	resp, _ = b.post("/console/login", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "email": {"owner@progress.test"}, "password": {"owner-password-2026"}})
	if resp.Request.URL.Path != "/console/mfa/setup" {
		t.Fatalf("missing MFA gate: %s", resp.Request.URL)
	}
	if resp, _ := b.get("/console/progress"); resp.Request.URL.Path != "/console/mfa/setup" {
		t.Fatal("progress bypassed MFA setup")
	}
	_, page = b.get("/console/mfa/setup")
	enrollTOTP(t, b, page)
	owner, err = store.GetPrincipalForLearner(ctx, ownerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	_, version, err := store.CreateFormationDraft(ctx, owner, "Course <script>alert(1)</script>", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddFormationConceptsIdempotent(ctx, owner, "progress-content", version.ID, nil, []models.FormationConceptInput{{StableKey: "numbers", Label: "Numbers <script>"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishFormationVersion(ctx, owner, version.ID); err != nil {
		t.Fatal(err)
	}
	cohort, err := store.CreateCohort(ctx, owner, version.ID, "Autumn", 5, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	studentID := seedLearner(t, store, "learner@progress.test", "learner-password-2026")
	student, err := store.GetPrincipalForLearner(ctx, studentID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := store.EnrollMembership(ctx, owner, cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	trainerID := seedLearner(t, store, "trainer@progress.test", "trainer-password-2026")
	trainer, err := store.GetPrincipalForLearner(ctx, trainerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMembershipAuthorization(ctx, trainer.TenantScope(), models.MembershipStatusActive, []string{models.RoleTrainer}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordMembershipMFAVerification(ctx, trainer.TenantScope(), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/console/progress", "/console/progress?cohort_id=" + cohort.ID, "/console/progress?enrollment_id=" + enrollment.ID} {
		resp, page = b.get(path)
		if resp.StatusCode != 200 || strings.Contains(page, "<script>") || !strings.Contains(page, "&lt;script&gt;") || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("progress page %s: %d %.1000s", path, resp.StatusCode, page)
		}
	}
	if !strings.Contains(page, "No estimate") || !strings.Contains(page, "No sessions recorded") {
		t.Fatal("missing empty evidence guidance")
	}
	statsPath := "/console/progress?view=statistics&cohort_id=" + cohort.ID
	if resp, body := b.get(statsPath); resp.StatusCode != 200 || !strings.Contains(body, "have not been computed yet") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("statistics before computation: %d %.800s", resp.StatusCode, body)
	}
	if _, cohortPage := b.get("/console/progress?cohort_id=" + cohort.ID); !strings.Contains(cohortPage, "view=statistics") {
		t.Fatal("cohort page does not link to the statistics")
	}
	if _, err := store.RecomputeInstitutionStatistics(ctx, owner.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if resp, body := b.get(statsPath); resp.StatusCode != 200 || !strings.Contains(body, "Computed at") || !strings.Contains(body, "no collective figure is available") || strings.Contains(body, "<script>") {
		t.Fatalf("statistics for a one-learner cohort: %d %.800s", resp.StatusCode, body)
	}
	if resp, body := b.get("/console/progress?view=statistics&cohort_id=missing"); resp.StatusCode != 404 || !strings.Contains(body, "not_found") {
		t.Fatalf("statistics error: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.get("/console/progress/badges?enrollment_id=" + enrollment.ID); resp.StatusCode != 200 || !strings.Contains(body, "No badges earned") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("staff badges: %d %s", resp.StatusCode, body)
	}
	if resp, _ := b.post("/console/progress/trainers", url.Values{"cohort_id": {cohort.ID}, "email": {"trainer@progress.test"}, "assigned": {"true"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatal("assignment missing CSRF accepted")
	}
	_, page = b.get("/console/progress?cohort_id=" + cohort.ID)
	form := url.Values{"csrf_token": {field(t, csrfPattern, page)}, "cohort_id": {cohort.ID}, "email": {"trainer@progress.test"}, "assigned": {"true"}}
	resp, page = b.post("/console/progress/trainers", form)
	if resp.StatusCode != 200 || !strings.Contains(page, "trainer@progress.test") {
		t.Fatalf("assignment: %d %.1000s", resp.StatusCode, page)
	}
	if resp, _ := b.post("/console/progress/trainers", form); resp.StatusCode != http.StatusForbidden {
		t.Fatal("assignment CSRF replay accepted")
	}
	trainer, err = store.GetPrincipalForLearner(ctx, trainerID, []string{models.OAuthScopeProgressRead})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetLearnerProgress(ctx, trainer, enrollment.ID, "", 10); err != nil {
		t.Fatal(err)
	}
	_, page = b.get("/console/progress?cohort_id=" + cohort.ID)
	if resp, _ := b.post("/console/progress/trainers", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "cohort_id": {cohort.ID}, "email": {"trainer@progress.test"}, "assigned": {"false"}}); resp.StatusCode != 200 {
		t.Fatal("assignment revoke failed")
	}
	if _, err := store.GetLearnerProgress(ctx, trainer, enrollment.ID, "", 10); err == nil {
		t.Fatal("revoked trainer still reads")
	}
	if resp, body := b.get("/console/progress?cohort_id=missing"); resp.StatusCode != 404 || !strings.Contains(body, "not_found") {
		t.Fatalf("normalized error: %d %s", resp.StatusCode, body)
	}
	// An OAuth bearer by itself never opens the independent console boundary.
	stranger := newTestBrowser(t, server)
	stranger.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/console/progress", nil)
	req.Header.Set("Authorization", "Bearer unrelated-token")
	if resp, _ := stranger.do(req); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("bearer entered console")
	}
	if _, err := store.SetMembershipAuthorization(ctx, owner.TenantScope(), models.MembershipStatusSuspended, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if resp, _ := b.get("/console/progress"); resp.Request.URL.Path != "/console/login" {
		t.Fatal("suspended owner retained progress access")
	}
}
