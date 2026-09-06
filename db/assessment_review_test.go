// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"tutor-mcp/adminapi"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func reviewAttemptForTest(t *testing.T, s *Store, learnerID, id, state string) (pedagogicalDecisionFixture, *models.AssessmentAttempt) {
	t.Helper()
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, learnerID, id)
	a := f.attempt(t, id)
	a.RubricJSON = `{"criteria":[{"id":"correctness","description":"A correct response.","max_score":1,"anchors":[{"score":1,"description":"Complete reasoning."}]}],"passing_score":1,"answer_key":"Generated reference, frozen before the answer."}`
	if state == "unbound" {
		a.DecisionID = ""
	}
	if state == "hash_only" {
		a.TaskText = ""
	}
	if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	if state == "prepared" {
		return f, a
	}
	response := "Original learner response."
	if state == "hash_only" {
		response = ""
	}
	if err := s.SubmitAssessmentAttempt(ctx, learnerID, id, response, "response-hash", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	switch state {
	case "passed", "failed":
		score, total, passed := completeBoundScore, 1.0, true
		if state == "failed" {
			score, total, passed = `{"criteria_scores":[{"id":"correctness","score":0,"evidence":"SECRET_HOST_VERDICT"}]}`, 0, false
		}
		if err := s.CompleteAssessmentEvaluation(ctx, learnerID, id, score, "SECRET_HOST_ID", models.EvaluationMethodHostLLM,
			`{"private":"SECRET_PROVENANCE"}`, total, passed, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	case "cancelled":
		if err := s.CancelAssessmentAttempt(ctx, learnerID, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	case "invalidated":
		if err := s.CompareAndSwapCurriculum(ctx, learnerID, f.domain.ID, f.curriculum.Version, revisionForTest(f.curriculum)); err != nil {
			t.Fatal(err)
		}
	case "trusted":
		// Only a fixture may seed trust; there is still no application setter.
		if _, err := s.exec(ctx, `UPDATE assessment_attempts SET status = 'evaluated',
 rubric_score_json = ?, score = 1, passed = 1, evaluator_id = 'fixture',
 evaluation_method = 'human_review', trusted_evaluation = 1, evaluated_at = ? WHERE id = ?`,
			completeBoundScore, time.Now().UTC(), id); err != nil {
			t.Fatal(err)
		}
	}
	return f, a
}

func TestAssessmentReviewBlindProjectionAndCandidateFiltering(t *testing.T) {
	s := setupTestDB(t) // SQLite and isolated PostgreSQL via TUTOR_TEST_PG_DSN.
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	for _, state := range []string{"prepared", "cancelled", "invalidated", "unbound", "trusted"} {
		reviewAttemptForTest(t, s, "L2", "a-"+state, state)
	}
	reviewAttemptForTest(t, s, "L1", "a-self", "submitted")
	for _, state := range []string{"submitted", "passed", "failed", "hash_only"} {
		reviewAttemptForTest(t, s, "L2", "z-"+state, state)
	}
	first, err := s.ListAssessmentReviewCandidates(ctx, owner, "", "", 2)
	if err != nil || len(first.Items) != 2 || first.Items[0].AttemptID != "z-failed" || first.Items[1].AttemptID != "z-hash_only" || first.NextAfter != "z-hash_only" {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	second, err := s.ListAssessmentReviewCandidates(ctx, owner, "", first.NextAfter, 2)
	if err != nil || len(second.Items) != 2 || second.Items[0].AttemptID != "z-passed" || second.Items[1].AttemptID != "z-submitted" || second.NextAfter != "" {
		t.Fatalf("second page=%+v err=%v", second, err)
	}
	for _, id := range []string{"a-prepared", "a-cancelled", "a-invalidated", "a-unbound", "a-trusted", "a-self", "missing"} {
		if _, err := s.GetAssessmentReviewMaterial(ctx, owner, id); !errors.Is(err, storeport.ErrNotFound) {
			t.Errorf("%s detail err=%v, want opaque not found", id, err)
		}
	}
	before, err := s.GetAssessmentAttempt(ctx, "L2", "z-failed")
	if err != nil {
		t.Fatal(err)
	}
	material, err := s.GetAssessmentReviewMaterial(ctx, owner, "z-failed")
	if err != nil {
		t.Fatal(err)
	}
	if !material.TextAvailable || material.ResponseText != before.ResponseText || material.TaskText != before.TaskText ||
		material.Rubric.AnswerKey == "" || len(material.Rubric.Criteria[0].Anchors) != 1 ||
		!material.SubmittedAt.Equal(*before.SubmittedAt) || material.Competency.ID == "" {
		t.Fatalf("incomplete frozen review material: %+v", material)
	}
	raw, err := json.Marshal(material)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SECRET_", `"learner_id"`, `"user_id"`, `"passed"`, `"rubric_score"`, `"evaluator_id"`, `"context"`, `"p_mastery"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("blind material contains %s: %s", forbidden, raw)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, leaked := fields["status"]; leaked {
		t.Fatal("review exposed host evaluation status")
	}
	after, err := s.GetAssessmentAttempt(ctx, "L2", "z-failed")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("read changed assessment: err=%v", err)
	}
	hashOnly, err := s.GetAssessmentReviewMaterial(ctx, owner, "z-hash_only")
	if err != nil || hashOnly.TextAvailable || hashOnly.TaskContentHash == "" || hashOnly.ResponseContentHash == "" {
		t.Fatalf("missing plaintext not disclosed: %+v err=%v", hashOnly, err)
	}
}

func reviewRoleForTest(t *testing.T, s *Store, learnerID string, roles ...string) models.Principal {
	t.Helper()
	seedLearner(t, s, learnerID)
	ctx := context.Background()
	scope := models.LegacyPrincipal(learnerID).TenantScope()
	if _, err := s.exec(ctx, `UPDATE learners SET email_verified_at = ? WHERE id = ?`, time.Now().UTC(), learnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, scope, models.MembershipStatusActive, roles); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMembershipMFAVerification(ctx, scope, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	actor, err := s.GetPrincipal(ctx, scope, []string{models.OAuthScopeLearnerRead})
	if err != nil {
		t.Fatal(err)
	}
	return actor
}

func TestAssessmentReviewCurrentMembershipAndCohortAuthorization(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	assigned, _ := reviewAttemptForTest(t, s, "L2", "z-assigned", "submitted")
	other, _ := reviewAttemptForTest(t, s, "L2", "a-other", "submitted")
	trainer := reviewRoleForTest(t, s, "trainer", models.RoleTrainer)
	cohortID := "domain_cohort_" + assigned.domain.ID
	if err := s.AssignCohortTrainer(ctx, owner, cohortID, trainer.MembershipID); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListAssessmentReviewCandidates(ctx, trainer, "", "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].AttemptID != "z-assigned" || page.NextAfter != "" {
		t.Fatalf("trainer pagination leaked other cohort: %+v err=%v", page, err)
	}
	if _, err := s.GetAssessmentReviewMaterial(ctx, trainer, "z-assigned"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAssessmentReviewMaterial(ctx, trainer, "a-other"); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("other cohort readable: %v", err)
	}
	page, err = s.ListAssessmentReviewCandidates(ctx, trainer, "domain_cohort_"+other.domain.ID, "", 10)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("cohort filter granted access: %+v err=%v", page, err)
	}
	// An admin who is also a trainer retains the tenant-wide grant.
	combined := reviewRoleForTest(t, s, "combined", models.RoleTrainer, models.RoleAdmin)
	if _, err := s.GetAssessmentReviewMaterial(ctx, combined, "a-other"); err != nil {
		t.Fatalf("combined roles lost admin permission: %v", err)
	}
	trainer = reviewWriter(trainer)
	trainerReview := recordReviewForTest(t, s, trainer, "z-assigned", "trainer-opinion")
	if _, _, err := s.RecordAssessmentReview(ctx, trainer, "a-other", "other-opinion", trainerReview.MaterialHash, completeBoundScore); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("trainer reviewed another cohort: %v", err)
	}
	if _, err := s.exec(ctx, `DELETE FROM cohort_trainers WHERE tenant_id = ? AND cohort_id = ? AND membership_id = ?`,
		owner.TenantID, cohortID, trainer.MembershipID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAssessmentReviewMaterial(ctx, trainer, "z-assigned"); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("revoked assignment still grants access: %v", err)
	}
	if _, err := s.GetOwnAssessmentReview(ctx, trainer, "z-assigned"); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("revoked assignment can read review: %v", err)
	}
	if _, _, err := s.RecordAssessmentReview(ctx, trainer, "z-assigned", "trainer-opinion", trainerReview.MaterialHash, completeBoundScore); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("revoked assignment can retry: %v", err)
	}
	for _, role := range []string{models.RoleLearner, models.RoleAuditor, models.RoleBillingAdmin} {
		actor := reviewRoleForTest(t, s, "role-"+role, role)
		if _, err := s.GetAssessmentReviewMaterial(ctx, actor, "z-assigned"); !errors.Is(err, storeport.ErrInvalidPrincipal) {
			t.Errorf("%s can read raw response: %v", role, err)
		}
	}
	writeOnly := owner
	writeOnly.Scopes = []string{models.OAuthScopeLearnerWrite}
	if _, err := s.GetAssessmentReviewMaterial(ctx, writeOnly, "z-assigned"); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("write-only grant read material: %v", err)
	}
	forged := trainer
	forged.Roles = []string{models.RoleOwner}
	if _, err := s.GetAssessmentReviewMaterial(ctx, forged, "z-assigned"); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("forged role granted access: %v", err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, owner.TenantScope(), models.MembershipStatusSuspended, owner.Roles); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAssessmentReviewMaterial(ctx, owner, "z-assigned"); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("suspended owner granted access: %v", err)
	}
}

func TestAssessmentReviewRejectsForeignTenantAndInvalidPages(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	reviewAttemptForTest(t, s, "L2", "local", "submitted")
	now := time.Now().UTC()
	if _, err := s.exec(ctx, `INSERT INTO tenants (id, slug, name, status, region, policy_json, created_at, updated_at)
 VALUES ('review-foreign', 'review-foreign', 'Foreign', 'active', 'default', '{}', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `INSERT INTO users (id, email, normalized_email, password_hash, status, token_version, created_at, updated_at)
 VALUES ('foreign-user', 'foreign@example.com', 'foreign@example.com', 'hash', 'active', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `INSERT INTO tenant_memberships (id, tenant_id, user_id, roles_json, status, version, created_at, updated_at)
 VALUES ('foreign-member', 'review-foreign', 'foreign-user', '["owner"]', 'active', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	foreign := models.Principal{TenantID: "review-foreign", UserID: "foreign-user", MembershipID: "foreign-member",
		Roles: []string{models.RoleOwner}, Scopes: []string{models.OAuthScopeLearnerRead}, TokenVersion: 1}
	if err := s.ValidatePrincipal(ctx, foreign); err != nil {
		t.Fatalf("foreign fixture is not a valid principal: %v", err)
	}
	page, err := s.ListAssessmentReviewCandidates(ctx, foreign, "", "", 10)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("foreign tenant saw candidates: %+v err=%v", page, err)
	}
	if _, err := s.GetAssessmentReviewMaterial(ctx, foreign, "local"); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreign tenant read response: %v", err)
	}
	foreign = reviewWriter(foreign)
	if _, _, err := s.RecordAssessmentReview(ctx, foreign, "local", "foreign-opinion", strings.Repeat("0", 64), completeBoundScore); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreign tenant wrote review: %v", err)
	}
	if _, err := s.GetOwnAssessmentReview(ctx, foreign, "local"); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreign tenant read review: %v", err)
	}
	for _, limit := range []int{0, -1, 101} {
		if _, err := s.ListAssessmentReviewCandidates(ctx, owner, "", "", limit); !errors.Is(err, storeport.ErrInvalidAssessmentReviewRequest) {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
	if _, err := s.ListAssessmentReviewCandidates(ctx, owner, "", strings.Repeat("x", 256), 1); !errors.Is(err, storeport.ErrInvalidAssessmentReviewRequest) {
		t.Fatalf("unbounded cursor: %v", err)
	}
	if _, err := s.GetAssessmentReviewMaterial(ctx, models.Principal{}, "local"); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("invalid principal: %v", err)
	}
}

func TestAssessmentReviewHTTPPreviewDoesNotReplayLearningAndRechecksApplicability(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	f, a := reviewAttemptForTest(t, s, "L2", "preview-integration", "failed")
	state := models.NewConceptStateInDomain("L2", f.domain.ID, "a")
	state.PMastery, state.Stability, state.Reps = 0.75, 8, 3
	if err := s.UpsertConceptState(ctx, state); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAssessmentAttempt(ctx, "L2", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeState, err := s.GetConceptStateInDomain(ctx, "L2", f.domain.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, err := auth.WithPrincipal(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx = auth.WithOAuthScope(requestCtx, models.OAuthScopeLearner)
	handler := adminapi.NewAssessmentReview(s, nil).Handler()
	preview := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/assessment-reviews/attempts/"+a.ID+"/preview",
			strings.NewReader(completeBoundScore)).WithContext(requestCtx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for range 2 {
		rec := preview()
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"recorded":false`) || !strings.Contains(rec.Body.String(), `"passed":true`) {
			t.Fatalf("preview status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	after, err := s.GetAssessmentAttempt(ctx, "L2", a.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("preview replaced host failure: %+v err=%v", after, err)
	}
	afterState, err := s.GetConceptStateInDomain(ctx, "L2", f.domain.ID, "a")
	if err != nil || !reflect.DeepEqual(beforeState, afterState) {
		t.Fatalf("preview changed BKT/FSRS: %+v err=%v", afterState, err)
	}
	for _, table := range []string{"interactions", "transfer_records", "pedagogical_snapshots"} {
		var count int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE learner_id = ? AND domain_id = ?`, "L2", f.domain.ID).Scan(&count); err != nil || count != 0 {
			t.Errorf("preview wrote %s: count=%d err=%v", table, count, err)
		}
	}
	// A fetched envelope is not a reservation. Each preview rereads eligibility.
	if err := s.CompareAndSwapCurriculum(ctx, "L2", f.domain.ID, f.curriculum.Version, revisionForTest(f.curriculum)); err != nil {
		t.Fatal(err)
	}
	if rec := preview(); rec.Code != http.StatusNotFound {
		t.Fatalf("preview reused superseded material: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
