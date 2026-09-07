package db

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"tutor-mcp/adminapi"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

func curriculumFindingsForTest(t *testing.T, snapshot *models.CurriculumSnapshot) string {
	t.Helper()
	raw, err := json.Marshal([]models.CurriculumFinding{{ConceptID: snapshot.Concepts[0].ID, Aspect: "definition", Judgment: "needs_revision", Rationale: "Clarify the observable competency and assess its prerequisites."}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCurriculumReviewHTTPScopesHistoryAndIdempotence(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := reviewWriter(ownerPrincipal(t, s))
	f := newPedagogicalDecisionFixture(t, s, "L2", "semantic-review")
	material, err := s.GetCurriculumReviewMaterial(ctx, owner, f.domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	actorCtx, err := auth.WithPrincipal(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	actorCtx = auth.WithOAuthScope(actorCtx, models.OAuthScopeLearner)
	handler := adminapi.NewCurriculumReview(s, nil).Handler()
	path := fmt.Sprintf("/admin/curriculum-reviews/domains/%s/versions/1", f.domain.ID)
	request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(actorCtx)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("ETag") != `"`+material.MaterialHash+`"` {
		t.Fatalf("material %d %s", response.Code, response.Body.String())
	}
	raw := curriculumFindingsForTest(t, material.Snapshot)
	for i := range 2 {
		request = httptest.NewRequest(http.MethodPost, path+"/opinions", strings.NewReader(raw)).WithContext(actorCtx)
		request.Header.Set("Idempotency-Key", "curriculum-opinion")
		request.Header.Set("If-Match", `"`+material.MaterialHash+`"`)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := 201
		if i == 1 {
			want = 200
		}
		if response.Code != want {
			t.Fatalf("record %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	opinion, err := s.GetOwnCurriculumReview(ctx, owner, f.domain.ID, 1)
	if err != nil || opinion.Certified || opinion.Disposition != "changes_requested" || opinion.ReviewedSections != 1 {
		t.Fatalf("opinion: %+v %v", opinion, err)
	}
	unchanged, err := s.GetCurriculumSnapshot(ctx, "L2", f.domain.ID, 1)
	if err != nil || !reflect.DeepEqual(unchanged, material.Snapshot) {
		t.Fatal("review rewrote curriculum")
	}
	learner := models.LegacyPrincipal("L2")
	if _, err := s.GetCurriculumReviewMaterial(ctx, learner, f.domain.ID, 1); err == nil {
		t.Fatal("learner reviewed own curriculum")
	}
	trainer := reviewWriter(reviewRoleForTest(t, s, "semantic-trainer", models.RoleTrainer))
	if _, err := s.GetCurriculumReviewMaterial(ctx, trainer, f.domain.ID, 1); err == nil {
		t.Fatal("unassigned trainer read curriculum")
	}
	if err := s.AssignCohortTrainer(ctx, owner, "domain_cohort_"+f.domain.ID, trainer.MembershipID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCurriculumReviewMaterial(ctx, trainer, f.domain.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwapCurriculum(ctx, "L2", f.domain.ID, 1, revisionForTest(f.curriculum)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordCurriculumReview(ctx, owner, f.domain.ID, 1, "another-key", material.MaterialHash, raw); err == nil {
		t.Fatal("stale review accepted")
	}
	if _, err := s.GetOwnCurriculumReview(ctx, owner, f.domain.ID, 1); err != nil {
		t.Fatal("historical review lost")
	}
	if _, err := s.exec(ctx, `UPDATE curriculum_review_opinions SET disposition = 'complete_opinion' WHERE id = ?`, opinion.ID); err == nil {
		t.Fatal("review mutable")
	}
	if _, err := s.RunDataRetention(ctx, RetentionPolicy{AssessmentPlaintextDays: 1}, time.Now().Add(48*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	redacted, err := s.GetOwnCurriculumReview(ctx, owner, f.domain.ID, 1)
	if err != nil || redacted.Findings != nil || redacted.FindingsHash != opinion.FindingsHash {
		t.Fatalf("retention: %+v %v", redacted, err)
	}
}
