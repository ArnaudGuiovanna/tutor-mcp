// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"tutor-mcp/adminapi"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func reviewWriter(actor models.Principal) models.Principal {
	actor.Scopes = []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite}
	return actor
}

func recordReviewForTest(t *testing.T, s *Store, actor models.Principal, attemptID, key string) *models.AssessmentReview {
	t.Helper()
	material, err := s.GetAssessmentReviewMaterial(context.Background(), actor, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	review, replayed, err := s.RecordAssessmentReview(context.Background(), actor, attemptID, key, material.MaterialHash, completeBoundScore)
	if err != nil || replayed {
		t.Fatalf("record review: replayed=%t err=%v", replayed, err)
	}
	return review
}

func TestAssessmentReviewJournalHTTPIsSeparateFromLearning(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := reviewWriter(ownerPrincipal(t, s))
	f, a := reviewAttemptForTest(t, s, "L2", "a-review", "failed")
	reviewAttemptForTest(t, s, "L2", "z-next", "submitted")
	state := models.NewConceptStateInDomain("L2", f.domain.ID, "a")
	state.PMastery, state.Stability, state.Reps = .75, 8, 3
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
	counts := map[string]int{}
	for _, table := range []string{"interactions", "transfer_records", "pedagogical_snapshots"} {
		counts[table] = retentionCountTable(t, s, table)
	}
	material, err := s.GetAssessmentReviewMaterial(ctx, owner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, err := auth.WithPrincipal(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx = auth.WithOAuthScope(requestCtx, models.OAuthScopeLearner)
	handler := adminapi.NewAssessmentReview(s, nil).Handler()
	var recorded models.AssessmentReview
	for i := range 2 {
		req := httptest.NewRequest(http.MethodPost, "/admin/assessment-reviews/attempts/"+a.ID+"/reviews", strings.NewReader(completeBoundScore)).WithContext(requestCtx)
		req.Header.Set("Idempotency-Key", "first-opinion")
		req.Header.Set("If-Match", `"`+material.MaterialHash+`"`)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := http.StatusCreated
		if i == 1 {
			want = http.StatusOK
		}
		var body struct {
			Review   models.AssessmentReview `json:"review"`
			Replayed bool                    `json:"replayed"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != want || body.Replayed != (i == 1) || !body.Review.Passed || body.Review.TrustedEvaluation || body.Review.Total != 1 || body.Review.ReviewerUserID != owner.UserID || body.Review.ReviewerMembershipID != owner.MembershipID || body.Review.ReviewerTokenVersion != owner.TokenVersion {
			t.Fatalf("record status=%d body=%s", rec.Code, rec.Body.String())
		}
		if i == 1 && !reflect.DeepEqual(recorded, body.Review) {
			t.Fatal("retry changed immutable opinion")
		}
		recorded = body.Review
	}
	if retentionCountTable(t, s, "assessment_reviews") != 1 {
		t.Fatal("duplicate review")
	}
	var auditCount int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'assessment.review.record' AND target_id = ? AND details_json = '{}'`, recorded.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}
	after, err := s.GetAssessmentAttempt(ctx, "L2", a.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("review overwrote host assessment")
	}
	afterState, err := s.GetConceptStateInDomain(ctx, "L2", f.domain.ID, "a")
	if err != nil || !reflect.DeepEqual(beforeState, afterState) {
		t.Fatal("review replayed learning")
	}
	for table, count := range counts {
		if retentionCountTable(t, s, table) != count {
			t.Fatalf("review wrote %s", table)
		}
	}
	trusted, err := s.GetTrustedPassedAssessmentAttemptsInDomain(ctx, "L2", f.domain.ID, "a", 10)
	if err != nil || len(trusted) != 0 {
		t.Fatal("opinion promoted trusted evidence")
	}
	page, err := s.ListAssessmentReviewCandidates(ctx, owner, "", "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].AttemptID != "z-next" || page.NextAfter != "" {
		t.Fatalf("own opinions filtered after pagination: %+v %v", page, err)
	}
	second := reviewWriter(reviewRoleForTest(t, s, "second", models.RolePedagogyManager))
	if _, err := s.GetOwnAssessmentReview(ctx, second, a.ID); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("other opinion disclosed: %v", err)
	}
	otherMaterial, err := s.GetAssessmentReviewMaterial(ctx, second, a.ID)
	if err != nil || otherMaterial.MaterialHash != material.MaterialHash {
		t.Fatal("material depends on reviewer/previous opinion")
	}
	recordReviewForTest(t, s, second, a.ID, "first-opinion")
	if retentionCountTable(t, s, "assessment_reviews") != 2 {
		t.Fatal("second account cannot submit a blind opinion")
	}
}

func TestAssessmentReviewJournalRejectsChangedDuplicateAndUnauthorizedOpinions(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := reviewWriter(ownerPrincipal(t, s))
	f, a := reviewAttemptForTest(t, s, "L2", "journal-reject", "submitted")
	material, err := s.GetAssessmentReviewMaterial(ctx, owner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, score := range []string{
		`{"criteria_scores":[{"id":"correctness","score":0,"score":1,"evidence":"SECRET"}]}`,
		strings.TrimSuffix(completeBoundScore, "}") + `,"evaluation_method":"human_review"}`,
		strings.TrimSuffix(completeBoundScore, "}") + `,"reviewer_user_id":"forged"}`,
		strings.TrimSuffix(completeBoundScore, "}") + `,"trusted_evaluation":true}`,
		strings.Replace(completeBoundScore, `"score":1`, `"score":2`, 1),
	} {
		if _, _, err := s.RecordAssessmentReview(ctx, owner, a.ID, "k", material.MaterialHash, score); !errors.Is(err, storeport.ErrInvalidAssessmentReviewScore) {
			t.Fatalf("invalid score accepted: %v", err)
		}
	}
	if _, _, err := s.RecordAssessmentReview(ctx, owner, a.ID, "k", strings.Repeat("0", 64), completeBoundScore); !errors.Is(err, storeport.ErrAssessmentReviewMaterialChanged) {
		t.Fatalf("wrong material: %v", err)
	}
	for _, scope := range []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite} {
		restricted := owner
		restricted.Scopes = []string{scope}
		if _, _, err := s.RecordAssessmentReview(ctx, restricted, a.ID, "k", material.MaterialHash, completeBoundScore); !errors.Is(err, storeport.ErrInvalidPrincipal) {
			t.Fatalf("scope bypass: %v", err)
		}
	}
	if retentionCountTable(t, s, "assessment_reviews") != 0 {
		t.Fatal("invalid proposal persisted")
	}
	review := recordReviewForTest(t, s, owner, a.ID, "k")
	canonicalRetry := ` { "criteria_scores": [ { "evidence": "The committed response meets the criterion.", "score": 1.0, "id": "correctness" } ] } `
	if retry, replayed, err := s.RecordAssessmentReview(ctx, owner, a.ID, "k", material.MaterialHash, canonicalRetry); err != nil || !replayed || retry.ID != review.ID {
		t.Fatalf("equivalent canonical retry: %+v %t %v", retry, replayed, err)
	}
	for _, tc := range []struct{ key, score string }{
		{"another-key", completeBoundScore}, {"k", strings.Replace(completeBoundScore, `"score":1`, `"score":0`, 1)},
	} {
		if _, _, err := s.RecordAssessmentReview(ctx, owner, a.ID, tc.key, material.MaterialHash, tc.score); !errors.Is(err, storeport.ErrAssessmentReviewConflict) {
			t.Fatalf("opinion overwrite: %v", err)
		}
	}
	_, another := reviewAttemptForTest(t, s, "L2", "another-attempt", "submitted")
	anotherMaterial, err := s.GetAssessmentReviewMaterial(ctx, owner, another.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordAssessmentReview(ctx, owner, another.ID, "k", anotherMaterial.MaterialHash, completeBoundScore); !errors.Is(err, storeport.ErrAssessmentReviewConflict) {
		t.Fatalf("key reused across attempts: %v", err)
	}
	if err := s.CompareAndSwapCurriculum(ctx, "L2", f.domain.ID, f.curriculum.Version, revisionForTest(f.curriculum)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordAssessmentReview(ctx, owner, a.ID, "k", material.MaterialHash, completeBoundScore); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("invalidated retry accepted: %v", err)
	}
	history, err := s.GetOwnAssessmentReview(ctx, owner, a.ID)
	if err != nil || history.ID != review.ID || history.CurriculumInvalidatedVersion == 0 {
		t.Fatalf("invalidated history lost: %+v %v", history, err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, owner.TenantScope(), models.MembershipStatusSuspended, owner.Roles); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOwnAssessmentReview(ctx, owner, a.ID); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("revoked account can read history: %v", err)
	}
}

func TestAssessmentReviewJournalScopeConstraintsAndSelfExclusion(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := reviewWriter(ownerPrincipal(t, s))
	_, a := reviewAttemptForTest(t, s, "L2", "scope-review", "submitted")
	material, err := s.GetAssessmentReviewMaterial(ctx, owner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ learner, user, membership string }{
		{"L1", owner.UserID, owner.MembershipID},
		{"L2", owner.UserID, "membership_legacy_L2"},
		{"L2", "L2", "membership_legacy_L2"},
	} {
		if _, err := s.exec(ctx, `INSERT INTO assessment_reviews
 (id, tenant_id, learner_id, attempt_id, reviewer_user_id, reviewer_membership_id, reviewer_token_version,
 idempotency_key, material_hash, rubric_score_hash, rubric_score_json, total, passed, created_at)
 VALUES ('scope-probe', ?, ?, ?, ?, ?, 1, 'scope-probe', ?, ?, '{}', 1, 1, ?)`,
			owner.TenantID, tc.learner, a.ID, tc.user, tc.membership, material.MaterialHash, material.MaterialHash, material.SubmittedAt); err == nil {
			t.Fatalf("scope mismatch accepted: %+v", tc)
		}
	}
	if retentionCountTable(t, s, "assessment_reviews") != 0 {
		t.Fatal("scope-invalid row stored")
	}
	_, self := reviewAttemptForTest(t, s, "L1", "self-review", "submitted")
	if _, _, err := s.RecordAssessmentReview(ctx, owner, self.ID, "self", material.MaterialHash, completeBoundScore); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("self opinion accepted: %v", err)
	}
}

func TestAssessmentReviewJournalConcurrentRetriesAndRollback(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := reviewWriter(ownerPrincipal(t, s))
	_, a := reviewAttemptForTest(t, s, "L2", "concurrent-review", "submitted")
	material, err := s.GetAssessmentReviewMaterial(ctx, owner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("abort outer transaction")
	err = s.WithTenantTx(ctx, owner.TenantScope(), func(txCtx context.Context, _ storeport.Store) error {
		if _, _, err := s.RecordAssessmentReview(txCtx, owner, a.ID, "k", material.MaterialHash, completeBoundScore); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) || retentionCountTable(t, s, "assessment_reviews") != 0 {
		t.Fatalf("journal escaped rollback: %v", err)
	}
	var audits int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'assessment.review.record'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal("audit escaped rollback")
	}
	type result struct {
		id       string
		replayed bool
		err      error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, replayed, err := s.RecordAssessmentReview(ctx, owner, a.ID, "k", material.MaterialHash, completeBoundScore)
			id := ""
			if r != nil {
				id = r.ID
			}
			results <- result{id, replayed, err}
		}()
	}
	wg.Wait()
	close(results)
	var id string
	created := 0
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if id == "" {
			id = r.id
		}
		if id != r.id {
			t.Fatal("concurrent retries produced different opinions")
		}
		if !r.replayed {
			created++
		}
	}
	if created != 1 || retentionCountTable(t, s, "assessment_reviews") != 1 {
		t.Fatalf("created=%d", created)
	}
}

func TestAssessmentReviewJournalSQLImmutabilityAndRetention(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := reviewWriter(ownerPrincipal(t, s))
	_, a := reviewAttemptForTest(t, s, "L2", "review-retention", "submitted")
	review := recordReviewForTest(t, s, owner, a.ID, "k")
	for _, mutation := range []string{
		`total = 0`, `passed = 0`, `trusted_evaluation = 1`, `reviewer_user_id = 'L2'`,
		`material_hash = '` + strings.Repeat("0", 64) + `'`, `idempotency_key = 'replace'`, `rubric_score_json = '{}'`,
	} {
		if _, err := s.exec(ctx, `UPDATE assessment_reviews SET `+mutation+` WHERE id = ?`, review.ID); err == nil {
			t.Fatalf("immutable review mutated: %s", mutation)
		}
	}
	now := review.CreatedAt.AddDate(0, 0, 60)
	policy := RetentionPolicy{AssessmentPlaintextDays: 30}
	if err := s.CreateRetentionLegalHold(ctx, RetentionLegalHold{HoldID: "review-hold", LearnerID: "L2", Reason: "pending review", CreatedBy: "legal", CreatedAt: review.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	held, err := s.RunDataRetention(ctx, policy, now, true)
	if err != nil || held.AssessmentReviewPlaintext.Held != 1 || held.AssessmentReviewPlaintext.Applied != 0 {
		t.Fatalf("hold: %+v %v", held, err)
	}
	if _, err := s.ReleaseRetentionLegalHold(ctx, "review-hold", "legal", "closed", now); err != nil {
		t.Fatal(err)
	}
	dry, err := s.RunDataRetention(ctx, policy, now, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionMetric(t, "review plaintext dry", dry.AssessmentReviewPlaintext, 1, 0)
	unredacted, err := s.GetOwnAssessmentReview(ctx, owner, a.ID)
	if err != nil || len(unredacted.RubricScore) == 0 {
		t.Fatal("dry run redacted review")
	}
	applied, err := s.RunDataRetention(ctx, policy, now, true)
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionMetric(t, "review plaintext applied", applied.AssessmentReviewPlaintext, 1, 1)
	eligible, count, heldCount := RetentionReportTotals(applied)
	if eligible != 1 || count != 1 || heldCount != 0 {
		t.Fatalf("retention totals: %d %d %d", eligible, count, heldCount)
	}
	redacted, err := s.GetOwnAssessmentReview(ctx, owner, a.ID)
	if err != nil || redacted.RubricScore != nil || redacted.RubricScoreHash != review.RubricScoreHash || redacted.Total != review.Total || redacted.MaterialHash != review.MaterialHash {
		t.Fatalf("redacted receipt: %+v %v", redacted, err)
	}
	if _, err := s.exec(ctx, `UPDATE assessment_reviews SET rubric_score_json = ? WHERE id = ?`, string(review.RubricScore), review.ID); err == nil {
		t.Fatal("redacted prose restored")
	}
	retry, replayed, err := s.RecordAssessmentReview(ctx, owner, a.ID, "k", review.MaterialHash, completeBoundScore)
	if err != nil || !replayed || retry.RubricScore != nil {
		t.Fatalf("redacted retry: %+v %t %v", retry, replayed, err)
	}
	// A retained opinion never authorizes grading a hash-only attempt.
	if _, err := s.exec(ctx, `UPDATE assessment_attempts SET response_text = NULL WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordAssessmentReview(ctx, owner, a.ID, "k", review.MaterialHash, completeBoundScore); !errors.Is(err, storeport.ErrAssessmentReviewMaterialUnavailable) {
		t.Fatalf("redacted material graded: %v", err)
	}
}

func TestAssessmentReviewJournalPostgresForcedRLS(t *testing.T) {
	dsn := os.Getenv("TUTOR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TUTOR_TEST_PG_DSN is not configured")
	}
	s := setupTestPG(t, dsn)
	ctx := context.Background()
	owner := reviewWriter(ownerPrincipal(t, s))
	_, a := reviewAttemptForTest(t, s, "L2", "rls-review", "submitted")
	material, err := s.GetAssessmentReviewMaterial(ctx, owner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := s.root.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf(`"tutor_review_rls_%d"`, testDBCounter.Add(1))
	if _, err := s.root.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.root.ExecContext(context.Background(), `DROP OWNED BY `+role)
		_, _ = s.root.ExecContext(context.Background(), `DROP ROLE `+role)
	}()
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role,
	} {
		if _, err := s.root.ExecContext(ctx, grant); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+role); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', 'tenant_legacy', true)`); err != nil {
		t.Fatal(err)
	}
	scope := owner.TenantScope()
	limited := &Store{db: tx, dialect: DialectPostgres, tenantScope: &scope}
	review, _, err := limited.RecordAssessmentReview(ctx, owner, a.ID, "rls", material.MaterialHash, completeBoundScore)
	if err != nil {
		t.Fatalf("authorized write under RLS: %v", err)
	}
	if _, err := limited.GetOwnAssessmentReview(ctx, owner, a.ID); err != nil {
		t.Fatalf("authorized read under RLS: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', 'foreign', true)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assessment_reviews`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("review bypassed RLS: %d %v", count, err)
	}
	for _, statement := range []string{`UPDATE assessment_reviews SET rubric_score_json = NULL`, `DELETE FROM assessment_reviews`} {
		result, err := tx.ExecContext(ctx, statement)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := result.RowsAffected(); n != 0 {
			t.Fatal("cross-tenant mutation succeeded")
		}
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT wrong_tenant`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assessment_reviews
 (id, tenant_id, learner_id, attempt_id, reviewer_user_id, reviewer_membership_id, reviewer_token_version,
 idempotency_key, material_hash, rubric_score_hash, rubric_score_json, total, passed, created_at)
 VALUES ('foreign-insert', 'tenant_legacy', 'L2', $1, $2, $3, $4, 'foreign', $5, $6, '{}', 1, 1, $7)`,
		a.ID, owner.UserID, owner.MembershipID, owner.TokenVersion, review.MaterialHash, review.RubricScoreHash, review.CreatedAt); err == nil {
		t.Fatal("cross-tenant insert succeeded")
	}
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT wrong_tenant`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', 'tenant_legacy', true)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assessment_reviews`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("own review was changed: %d %v", count, err)
	}
}
