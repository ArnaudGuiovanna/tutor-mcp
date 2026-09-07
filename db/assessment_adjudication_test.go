package db

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/adminapi"
	"tutor-mcp/auth"
	"tutor-mcp/certification"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func adjudicatorForTest(t *testing.T, s *Store, tenantID string) (*AssessmentAdjudicator, func(*models.AssessmentReview, string, string, int) string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal([]certification.Authority{{KeyID: "test-key", AuthorityID: "independent", TenantID: tenantID, PublicKey: base64.RawURLEncoding.EncodeToString(public)}})
	verifier, err := certification.New("review-audience", string(config))
	if err != nil {
		t.Fatal(err)
	}
	return NewAssessmentAdjudicator(s, verifier), func(review *models.AssessmentReview, id, verdict string, revision int) string {
		now := time.Now().UTC()
		claims := certification.Claims{ID: id, Audience: "review-audience", TenantID: tenantID, AttemptID: review.AttemptID, ReviewID: review.ID, MaterialHash: review.MaterialHash, ScoreHash: review.RubricScoreHash, Verdict: verdict, ExpectedRevision: revision, IssuedAt: now.Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix()}
		payload, _ := json.Marshal(claims)
		e := certification.Envelope{KeyID: "test-key", Payload: base64.RawURLEncoding.EncodeToString(payload)}
		e.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(certification.MessagePrefix+e.KeyID+"\n"+e.Payload)))
		raw, _ := json.Marshal(e)
		return string(raw)
	}
}

func TestAssessmentAdjudicationChangesEvidenceWithoutReplayingLearning(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	actor := reviewWriter(ownerPrincipal(t, s))
	f, attempt := reviewAttemptForTest(t, s, "L2", "adjudicate", "failed")
	review := recordReviewForTest(t, s, actor, attempt.ID, "opinion")
	adjudicator, sign := adjudicatorForTest(t, s, actor.TenantID)
	before, _ := s.GetAssessmentAttempt(ctx, "L2", attempt.ID)
	counts := map[string]int{}
	for _, table := range []string{"interactions", "concept_states", "transfer_records", "pedagogical_snapshots"} {
		counts[table] = retentionCountTable(t, s, table)
	}
	certificate := sign(review, "accept-1", "accept", 0)
	accepted, replayed, err := adjudicator.AdjudicateAssessment(ctx, actor, attempt.ID, certificate)
	if err != nil || replayed || accepted.Revision != 1 {
		t.Fatalf("accept: %+v replayed=%t err=%v", accepted, replayed, err)
	}
	retry, replayed, err := adjudicator.AdjudicateAssessment(ctx, actor, attempt.ID, certificate)
	if err != nil || !replayed || !reflect.DeepEqual(accepted, retry) {
		t.Fatalf("retry: %+v %t %v", retry, replayed, err)
	}
	current, err := adjudicator.GetAssessmentAdjudication(ctx, actor, attempt.ID)
	if err != nil || current.ID != accepted.ID {
		t.Fatalf("current: %+v %v", current, err)
	}
	after, _ := s.GetAssessmentAttempt(ctx, "L2", attempt.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rewrote original host evaluation")
	}
	for table, n := range counts {
		if retentionCountTable(t, s, table) != n {
			t.Fatalf("replayed learning into %s", table)
		}
	}
	single, err := s.GetEvaluatedAssessmentAttemptsInDomain(ctx, "L2", f.domain.ID, "a", 10)
	if err != nil || len(single) != 1 || !single[0].TrustedEvaluation || !single[0].Passed || single[0].EvaluationMethod != models.EvaluationMethodExternal {
		t.Fatalf("effective evidence: %+v %v", single, err)
	}
	batch, err := s.GetEvaluatedAssessmentAttemptsBatchInDomain(ctx, "L2", f.domain.ID, []string{"a"}, 10)
	if err != nil || !reflect.DeepEqual(single, batch["a"]) {
		t.Fatalf("batch mismatch: %v", err)
	}
	passed, err := s.GetTrustedPassedAssessmentAttemptsInDomain(ctx, "L2", f.domain.ID, "a", 10)
	if err != nil || len(passed) != 1 {
		t.Fatalf("trusted passed: %+v %v", passed, err)
	}
	if _, err := s.exec(ctx, `UPDATE domains SET high_stakes = 1 WHERE id = ?`, f.domain.ID); err != nil {
		t.Fatal(err)
	}
	passed, err = s.GetTrustedPassedAssessmentAttemptsInDomain(ctx, "L2", f.domain.ID, "a", 10)
	if err != nil || len(passed) != 0 {
		t.Fatal("external certificate bypassed high-stakes policy")
	}
	if _, err := s.exec(ctx, `UPDATE domains SET high_stakes = 0 WHERE id = ?`, f.domain.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := adjudicator.AdjudicateAssessment(ctx, actor, attempt.ID, sign(review, "stale", "reject", 0)); err == nil {
		t.Fatal("accepted stale revision")
	}
	rejected, _, err := adjudicator.AdjudicateAssessment(ctx, actor, attempt.ID, sign(review, "reject-2", "reject", 1))
	if err != nil || rejected.Revision != 2 {
		t.Fatalf("withdraw: %+v %v", rejected, err)
	}
	passed, err = s.GetTrustedPassedAssessmentAttemptsInDomain(ctx, "L2", f.domain.ID, "a", 10)
	if err != nil || len(passed) != 0 {
		t.Fatal("withdrawn evidence remains trusted")
	}
	if _, err := s.exec(ctx, `UPDATE assessment_adjudications SET verdict = 'accept' WHERE id = ?`, rejected.ID); err == nil {
		t.Fatal("adjudication is mutable")
	}
}

func TestAssessmentAdjudicationRejectsScopeAndSerializesCertificates(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	actor := reviewWriter(ownerPrincipal(t, s))
	f, attempt := reviewAttemptForTest(t, s, "L2", "concurrent-adjudication", "failed")
	review := recordReviewForTest(t, s, actor, attempt.ID, "opinion")
	adjudicator, sign := adjudicatorForTest(t, s, actor.TenantID)
	certificate := sign(review, "concurrent", "accept", 0)
	for _, bad := range []models.Principal{reviewWriter(reviewRoleForTest(t, s, "trainer-no-adjudicate", models.RoleTrainer)), reviewWriter(reviewRoleForTest(t, s, "learner-no-adjudicate", models.RoleLearner))} {
		if _, _, err := adjudicator.AdjudicateAssessment(ctx, bad, attempt.ID, certificate); err == nil {
			t.Fatal("unauthorized adjudication")
		}
	}
	if _, _, err := adjudicator.AdjudicateAssessment(ctx, actor, "other-attempt", certificate); err == nil {
		t.Fatal("transplanted certificate")
	}
	badReview := *review
	badReview.MaterialHash = review.RubricScoreHash
	if _, _, err := adjudicator.AdjudicateAssessment(ctx, actor, attempt.ID, sign(&badReview, "changed", "accept", 0)); err == nil {
		t.Fatal("changed material")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := adjudicator.AdjudicateAssessment(ctx, actor, attempt.ID, certificate)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if retentionCountTable(t, s, "assessment_adjudications") != 1 {
		t.Fatal("duplicate dispositions")
	}
	if err := s.CompareAndSwapCurriculum(ctx, "L2", f.domain.ID, f.curriculum.Version, revisionForTest(f.curriculum)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := adjudicator.AdjudicateAssessment(ctx, actor, attempt.ID, sign(review, "invalidated", "accept", 1)); err == nil {
		t.Fatal("certified stale curriculum")
	}
	current, err := adjudicator.GetAssessmentAdjudication(ctx, actor, attempt.ID)
	if err != nil || current.CurriculumInvalidatedVersion == 0 {
		t.Fatalf("history lost: %+v %v", current, err)
	}
}

func TestAssessmentAdjudicationHTTPAndAtomicRollback(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	actor := reviewWriter(ownerPrincipal(t, s))
	_, a := reviewAttemptForTest(t, s, "L2", "http-adjudication", "failed")
	review := recordReviewForTest(t, s, actor, a.ID, "http-opinion")
	adjudicator, sign := adjudicatorForTest(t, s, actor.TenantID)
	certificate := sign(review, "http-cert", "accept", 0)
	abort := errors.New("abort outer transaction")
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, _ storeport.Store) error {
		if _, _, err := adjudicator.AdjudicateAssessment(txCtx, actor, a.ID, certificate); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) || retentionCountTable(t, s, "assessment_adjudications") != 0 {
		t.Fatalf("escaped rollback: %v", err)
	}
	var audits int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'assessment.adjudicate'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal("audit escaped rollback")
	}
	actorCtx, err := auth.WithPrincipal(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	actorCtx = auth.WithOAuthScope(actorCtx, models.OAuthScopeLearner)
	handler := adminapi.NewAssessmentReview(s, nil, adjudicator).Handler()
	path := "/admin/assessment-reviews/attempts/" + a.ID + "/adjudications"
	for _, tc := range []struct {
		ctx    context.Context
		body   string
		status int
	}{
		{ctx, certificate, 403},
		{auth.WithOAuthScope(actorCtx, models.OAuthScopeLearnerRead), certificate, 403},
		{actorCtx, `{"evaluation_method":"human_review"}`, 400},
		{actorCtx, certificate, 201}, {actorCtx, certificate, 200},
	} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body)).WithContext(tc.ctx)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("HTTP %d expected %d: %s", response.Code, tc.status, response.Body.String())
		}
	}
}
