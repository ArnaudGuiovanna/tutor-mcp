// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tutor-mcp/assessment"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type assessmentReviewStoreStub struct {
	material    models.AssessmentReviewMaterial
	err         error
	calls       int
	lastActor   models.Principal
	lastAttempt string
	lastCohort  string
	lastAfter   string
	lastLimit   int
}

func (s *assessmentReviewStoreStub) ListAssessmentReviewCandidates(_ context.Context, actor models.Principal, cohort, after string, limit int) (models.AssessmentReviewPage, error) {
	s.calls++
	s.lastActor, s.lastCohort, s.lastAfter, s.lastLimit = actor, cohort, after, limit
	return models.AssessmentReviewPage{Items: []models.AssessmentReviewItem{s.material.AssessmentReviewItem}}, s.err
}

func (s *assessmentReviewStoreStub) GetAssessmentReviewMaterial(_ context.Context, actor models.Principal, attemptID string) (*models.AssessmentReviewMaterial, error) {
	s.calls++
	s.lastActor, s.lastAttempt = actor, attemptID
	copy := s.material
	return &copy, s.err
}

const reviewAPIPath = "/admin/assessment-reviews/attempts"
const reviewScoreJSON = `{"criteria_scores":[{"id":"reasoning","score":1,"evidence":"Reasoning matches the frozen criterion."}]}`

func newAssessmentReviewStub(t *testing.T) *assessmentReviewStoreStub {
	t.Helper()
	rubric, err := assessment.ParseRubric(`{"criteria":[{"id":"reasoning","description":"Explain the inference.","max_score":1}],"passing_score":1,"answer_key":"Generated reference."}`)
	if err != nil {
		t.Fatal(err)
	}
	return &assessmentReviewStoreStub{material: models.AssessmentReviewMaterial{
		AssessmentReviewItem: models.AssessmentReviewItem{AttemptID: "attempt-1", ActivityType: "PRACTICE", CurriculumVersion: 1},
		TaskText:             "Generated task.", ResponseText: "Learner response.", Rubric: rubric, TextAvailable: true,
	}}
}

func TestAssessmentReviewHTTPScopesRoutingAndPagination(t *testing.T) {
	store := newAssessmentReviewStub(t)
	handler := NewAssessmentReview(store, nil).Handler()
	for _, path := range []string{reviewAPIPath, reviewAPIPath + "/attempt-1"} {
		for _, scope := range []string{models.OAuthScopeLearnerWrite} {
			rec := adminRequest(t, handler, http.MethodGet, path, "", scope, nil)
			if rec.Code != http.StatusForbidden || store.calls != 0 {
				t.Fatalf("scope=%q path=%s status=%d calls=%d", scope, path, rec.Code, store.calls)
			}
		}
	}
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, reviewAPIPath, nil))
	if anonymous.Code != http.StatusForbidden || store.calls != 0 {
		t.Fatal("anonymous caller reached store")
	}
	rec := adminRequest(t, handler, http.MethodGet, reviewAPIPath+"?limit=101", "", models.OAuthScopeLearnerRead, nil)
	if rec.Code != http.StatusBadRequest || store.calls != 0 {
		t.Fatal("unbounded page reached store")
	}
	rec = adminRequest(t, handler, http.MethodGet, reviewAPIPath+"?cohort_id=c1&after=a1&limit=2", "", models.OAuthScopeLearnerRead, nil)
	if rec.Code != http.StatusOK || store.calls != 1 || store.lastCohort != "c1" || store.lastAfter != "a1" || store.lastLimit != 2 || store.lastActor.TenantID != "tenant" {
		t.Fatalf("list status=%d store=%+v", rec.Code, store)
	}
	rec = adminRequest(t, handler, http.MethodGet, reviewAPIPath+"/attempt-1", "", models.OAuthScopeLearner, nil)
	if rec.Code != http.StatusOK || store.lastAttempt != "attempt-1" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("detail status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, suffix := range []string{"", "/attempt-1", "/attempt-1/complete"} {
		rec = adminRequest(t, handler, http.MethodPost, reviewAPIPath+suffix, reviewScoreJSON, models.OAuthScopeLearner, nil)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("unexpected assessment mutation route %s: %d", suffix, rec.Code)
		}
	}
}

func TestAssessmentReviewHTTPPreviewIsStrictAndNeverRecordsTrust(t *testing.T) {
	store := newAssessmentReviewStub(t)
	handler := NewAssessmentReview(store, nil).Handler()
	path := reviewAPIPath + "/attempt-1/preview"
	before, _ := json.Marshal(store.material)
	rec := adminRequest(t, handler, http.MethodPost, path, reviewScoreJSON, models.OAuthScopeLearnerRead, nil)
	var result struct {
		AttemptID         string  `json:"attempt_id"`
		Total             float64 `json:"total"`
		Passed            bool    `json:"passed"`
		Recorded          *bool   `json:"recorded"`
		TrustedEvaluation *bool   `json:"trusted_evaluation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || result.Total != 1 || !result.Passed || result.AttemptID != "attempt-1" ||
		result.Recorded == nil || *result.Recorded || result.TrustedEvaluation == nil || *result.TrustedEvaluation || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview endorsed a review or failed scoring: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, raw := range []string{
		`{"criteria_scores":[{"id":"reasoning","score":1}]}`,
		`{"criteria_scores":[{"id":"reasoning","score":0,"score":1,"evidence":"Observed."}]}`,
		`{"criteria_scores":[{"id":"reasoning","score":"1","evidence":"Observed."}]}`,
		strings.Replace(reviewScoreJSON, `"score":1`, `"score":2`, 1),
		strings.Replace(reviewScoreJSON, `"reasoning"`, `"unknown"`, 1),
		strings.TrimSuffix(reviewScoreJSON, "}") + `,"total":0}`,
		strings.TrimSuffix(reviewScoreJSON, "}") + `,"trusted_evaluation":true}`,
		strings.TrimSuffix(reviewScoreJSON, "}") + `,"evaluation_method":"human_review"}`,
		reviewScoreJSON + `{}`,
		`null`,
	} {
		rec := adminRequest(t, handler, http.MethodPost, path, raw, models.OAuthScopeLearnerRead, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_score"`) {
			t.Errorf("invalid score accepted: status=%d body=%s input=%s", rec.Code, rec.Body.String(), raw)
		}
	}
	rec = adminRequest(t, handler, http.MethodPost, path, strings.Repeat(" ", assessment.MaxJSONBytes+1), models.OAuthScopeLearnerRead, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized score: %d", rec.Code)
	}
	after, _ := json.Marshal(store.material)
	if !bytes.Equal(before, after) {
		t.Fatal("preview changed material")
	}
	calls := store.calls
	rec = adminRequest(t, handler, http.MethodPost, path, reviewScoreJSON, models.OAuthScopeLearnerWrite, nil)
	if rec.Code != http.StatusForbidden || store.calls != calls {
		t.Fatal("preview bypassed read scope")
	}
	store.material.TextAvailable = false
	rec = adminRequest(t, handler, http.MethodPost, path, reviewScoreJSON, models.OAuthScopeLearnerRead, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "review_text_unavailable") {
		t.Fatal("hash-only material accepted for score preview")
	}
}

func TestAssessmentReviewHTTPErrorsAreOpaqueAndRecheckedForPreview(t *testing.T) {
	store := newAssessmentReviewStub(t)
	var logs bytes.Buffer
	handler := NewAssessmentReview(store, slog.New(slog.NewTextHandler(&logs, nil))).Handler()
	for _, tc := range []struct {
		err  error
		want int
	}{
		{storeport.ErrInvalidPrincipal, http.StatusForbidden},
		{storeport.WrapNotFound(errors.New("SECRET_FOREIGN_RESPONSE")), http.StatusNotFound},
		{storeport.ErrInvalidAssessmentReviewRequest, http.StatusBadRequest},
		{storeport.ErrAssessmentReviewMaterialUnavailable, http.StatusConflict},
		{errors.New("SECRET_DATABASE_PAYLOAD"), http.StatusInternalServerError},
	} {
		store.err = tc.err
		for _, suffix := range []string{"", "/attempt-1", "/attempt-1/preview"} {
			method := http.MethodGet
			if strings.HasSuffix(suffix, "preview") {
				method = http.MethodPost
			}
			rec := adminRequest(t, handler, method, reviewAPIPath+suffix, reviewScoreJSON, models.OAuthScopeLearnerRead, nil)
			if rec.Code != tc.want || strings.Contains(rec.Body.String(), "SECRET") || rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s status=%d body=%s want=%d", suffix, rec.Code, rec.Body.String(), tc.want)
			}
		}
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatal("review error logged private content")
	}
}
