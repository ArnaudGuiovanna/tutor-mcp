// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func revisionForTest(parent *models.CurriculumSnapshot) *models.CurriculumSnapshot {
	next := models.CloneCurriculumSnapshot(parent)
	next.Operation = models.CurriculumOperation{Type: models.CurriculumOperationUpdateMetadata, Rationale: "Change the declared competency"}
	next.Provenance = models.CurriculumProvenance{SourceType: "test", Author: "L1", Rationale: "Change the declared competency"}
	next.Concepts[0].Description += " revised"
	return next
}

func TestCurriculumReconciliationInvalidatesEvidenceAndPreservesAudit(t *testing.T) {
	s := setupTestDB(t) // also exercised on PostgreSQL with TUTOR_TEST_PG_DSN
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "semantic-change")
	key := f.curriculum.Concepts[0].Key
	if key != "a" {
		t.Fatal("fixture expects a first")
	}
	for _, concept := range []string{"a", "b"} {
		state := models.NewConceptStateInDomain("L1", f.domain.ID, concept)
		state.PMastery, state.Stability, state.Reps = 0.94, 20, 8
		if err := s.UpsertConceptState(ctx, state); err != nil {
			t.Fatal(err)
		}
	}
	prepared := f.attempt(t, "prepared-before-change")
	if err := s.CreateAssessmentAttempt(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	submitted := *prepared
	submitted.ID, submitted.DecisionID = "submitted-before-change", ""
	if err := s.CreateAssessmentAttempt(ctx, &submitted); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitAssessmentAttempt(ctx, "L1", submitted.ID, "Original committed answer", "hash", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	seedBatchAssessment(t, s, f.domain.ID, "a", time.Now().UTC(), models.EvaluationMethodHumanReview)
	evaluated, err := s.GetEvaluatedAssessmentAttemptsInDomain(ctx, "L1", f.domain.ID, "a", 10)
	if err != nil || len(evaluated) != 1 {
		t.Fatalf("fixture: %v %v", evaluated, err)
	}
	evaluatedID := evaluated[0].ID
	for _, concept := range []string{"a", "b"} {
		// Even a future-dated legacy row is superseded: no timestamp cutoff.
		if err := s.CreateInteraction(ctx, &models.Interaction{LearnerID: "L1", DomainID: f.domain.ID, Concept: concept,
			ActivityType: "PRACTICE", MisconceptionType: "KNOWLEDGE_GAP", CreatedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	seedBatchTransfer(t, s, f.domain.ID, "a", "near", 1, time.Now().UTC())
	if reviewed, err := s.HasHumanReviewedEvaluationInDomain(ctx, "L1", f.domain.ID); err != nil || !reviewed {
		t.Fatalf("fixture review gate: %t %v", reviewed, err)
	}
	next := revisionForTest(f.curriculum)
	// Persistence must ignore authored claims about the effects of a revision.
	next.Reconciliation = &models.CurriculumReconciliation{PolicyVersion: "host-preserve", InvalidatedConceptIDs: []string{}}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", f.domain.ID, 1, next); err != nil {
		t.Fatal(err)
	}
	published, err := s.GetCurriculumSnapshot(ctx, "L1", f.domain.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	r := published.Reconciliation
	if r == nil || r.PolicyVersion == "host-preserve" || len(r.InvalidatedConceptIDs) != 1 || r.InvalidatedConceptIDs[0] != f.curriculum.Concepts[0].ID {
		t.Fatalf("runtime reconciliation audit incomplete: %+v", r)
	}
	state, err := s.GetConceptStateInDomain(ctx, "L1", f.domain.ID, "a")
	if err != nil || state.PMastery != 0.1 || state.Reps != 0 || state.Stability != 1 || state.LastReview != nil || state.NextReview != nil {
		t.Fatalf("old estimate/schedule survived: %+v %v", state, err)
	}
	other, err := s.GetConceptStateInDomain(ctx, "L1", f.domain.ID, "b")
	if err != nil || other.PMastery != 0.94 || other.Reps != 8 {
		t.Fatalf("unaffected competency reset: %+v %v", other, err)
	}
	for _, id := range []string{prepared.ID, submitted.ID, evaluatedID} {
		attempt, err := s.GetAssessmentAttempt(ctx, "L1", id)
		if err != nil || attempt.CurriculumInvalidatedVersion != 2 {
			t.Fatalf("invalidation missing for %s: %+v %v", id, attempt, err)
		}
		if id == submitted.ID && (attempt.ResponseText != "Original committed answer" || attempt.Status != models.AssessmentAttemptSubmitted) {
			t.Fatal("original response/status overwritten")
		}
		if id == evaluatedID && (!attempt.Passed || !attempt.TrustedEvaluation || attempt.Status != models.AssessmentAttemptEvaluated) {
			t.Fatal("historical outcome or provenance overwritten")
		}
	}
	if err := s.SubmitAssessmentAttempt(ctx, "L1", prepared.ID, "late response", "", time.Now().UTC()); err == nil {
		t.Fatal("superseded prepared attempt submitted")
	}
	if err := s.CompleteAssessmentEvaluation(ctx, "L1", submitted.ID, `{}`, "host", models.EvaluationMethodHostLLM, "", 1, true, time.Now().UTC()); err == nil {
		t.Fatal("superseded response evaluated")
	}
	if attempts, err := s.GetEvaluatedAssessmentAttemptsInDomain(ctx, "L1", f.domain.ID, "a", 1); err != nil || len(attempts) != 0 {
		t.Fatalf("old evidence leaked: %v %v", attempts, err)
	}
	if attempts, err := s.GetTrustedPassedAssessmentAttemptsInDomain(ctx, "L1", f.domain.ID, "a", 1); err != nil || len(attempts) != 0 {
		t.Fatalf("old trusted evidence leaked: %v %v", attempts, err)
	}
	if batch, err := s.GetEvaluatedAssessmentAttemptsBatchInDomain(ctx, "L1", f.domain.ID, []string{"a"}, 1); err != nil || len(batch["a"]) != 0 {
		t.Fatalf("batch evidence leaked: %v %v", batch, err)
	}
	if records, err := s.GetTransferScoresInDomain(ctx, "L1", f.domain.ID, "a"); err != nil || len(records) != 0 {
		t.Fatalf("old transfer leaked: %v %v", records, err)
	}
	if batch, err := s.GetTransferScoresBatchInDomain(ctx, "L1", f.domain.ID, []string{"a"}); err != nil || len(batch["a"]) != 0 {
		t.Fatalf("batch transfer leaked: %v %v", batch, err)
	}
	if interactions, err := s.GetRecentInteractionsInDomain(ctx, "L1", f.domain.ID, "a", 1); err != nil || len(interactions) != 0 {
		t.Fatalf("old BKT history leaked: %v %v", interactions, err)
	}
	if groups, err := s.GetMisconceptionGroupsInDomain(ctx, "L1", f.domain.ID, map[string]bool{"a": true}); err != nil || len(groups) != 0 {
		t.Fatalf("old misconception leaked: %v %v", groups, err)
	}
	if reviewed, err := s.HasHumanReviewedEvaluationInDomain(ctx, "L1", f.domain.ID); err != nil || reviewed {
		t.Fatalf("old review passed gate: %t %v", reviewed, err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM interactions WHERE domain_id = ?`, f.domain.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("historical rows deleted: %d %v", count, err)
	}
	for _, table := range []string{"interactions", "assessment_attempts", "transfer_records"} {
		// Use independent root statements: an expected PostgreSQL trigger error
		// must not abort any of the assertions' surrounding transactions.
		if _, err := s.root.ExecContext(ctx, rb(s, `UPDATE `+table+` SET curriculum_invalidated_version = 0 WHERE domain_id = ? AND curriculum_invalidated_version > 0`), f.domain.ID); err == nil {
			t.Fatalf("%s evidence was resurrected", table)
		}
	}
	// New observations are usable, regardless of old event timestamps; another
	// metadata revision invalidates them without rewriting earlier annotations.
	if err := s.CreateInteraction(ctx, &models.Interaction{LearnerID: "L1", DomainID: f.domain.ID, Concept: "a", ActivityType: "PRACTICE", CreatedAt: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.GetRecentInteractionsInDomain(ctx, "L1", f.domain.ID, "a", 1); err != nil || len(rows) != 1 {
		t.Fatalf("new observation unavailable: %v %v", rows, err)
	}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", f.domain.ID, 2, revisionForTest(published)); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetAssessmentAttempt(ctx, "L1", submitted.ID)
	if err != nil || old.CurriculumInvalidatedVersion != 2 {
		t.Fatalf("first invalidation rewritten: %+v %v", old, err)
	}
}

func TestCurriculumReconciliationRenameAndRollback(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "rollback")
	state := models.NewConceptStateInDomain("L1", f.domain.ID, "a")
	state.PMastery, state.Reps = 0.9, 10
	if err := s.UpsertConceptState(ctx, state); err != nil {
		t.Fatal(err)
	}
	a := f.attempt(t, "rename-attempt")
	if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	rename := models.CloneCurriculumSnapshot(f.curriculum)
	rename.Concepts[0].Label = "Presentation only"
	rename.Operation = models.CurriculumOperation{Type: models.CurriculumOperationRename, Rationale: "Cosmetic rename"}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", f.domain.ID, 1, rename); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitAssessmentAttempt(ctx, "L1", a.ID, "still applicable", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetCurriculumSnapshot(ctx, "L1", f.domain.ID, 2)
	if len(current.Reconciliation.InvalidatedConceptIDs) != 0 {
		t.Fatal("rename invalidated evidence")
	}
	changed := revisionForTest(current)
	// A metadata identity collision is detected during snapshot insertion,
	// after reconciliation has run. The whole revision must nevertheless roll back.
	changed.Concepts[0].Outcomes = []models.CurriculumOutcome{{ID: "identity-in-another-domain", Statement: "new"}}
	foreign := newPedagogicalDecisionFixture(t, s, "L2", "foreign")
	foreignNext := revisionForTest(foreign.curriculum)
	foreignNext.Concepts[0].Outcomes = []models.CurriculumOutcome{{ID: "identity-in-another-domain", Statement: "foreign"}}
	if err := s.CompareAndSwapCurriculum(ctx, "L2", foreign.domain.ID, 1, foreignNext); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", f.domain.ID, 2, changed); err == nil {
		t.Fatal("identity collision accepted")
	}
	stored, err := s.GetConceptStateInDomain(ctx, "L1", f.domain.ID, "a")
	if err != nil || stored.PMastery != 0.9 || stored.Reps != 10 {
		t.Fatalf("failed revision changed state: %+v %v", stored, err)
	}
	stillValid, err := s.GetAssessmentAttempt(ctx, "L1", a.ID)
	if err != nil || stillValid.CurriculumInvalidatedVersion != 0 {
		t.Fatalf("failed revision invalidated evidence: %+v %v", stillValid, err)
	}
	if latest, err := s.GetCurriculumSnapshot(ctx, "L1", f.domain.ID, 0); err != nil || latest.Version != 2 {
		t.Fatalf("failed revision committed: %+v %v", latest, err)
	}
	stale := *a
	stale.ID, stale.DecisionID = "stale-standalone-preparation", ""
	if err := s.CreateAssessmentAttempt(ctx, &stale); err == nil {
		t.Fatal("stale standalone preparation accepted")
	}
	if err := s.CompareAndSwapCurriculum(ctx, "L2", f.domain.ID, 2, revisionForTest(current)); err == nil {
		t.Fatal("foreign learner changed curriculum")
	}
}

func TestCurriculumReconciliationSerializesObservationBeforeReset(t *testing.T) {
	s := setupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := newPedagogicalDecisionFixture(t, s, "L1", "serialization")
	locked, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- s.WithTx(ctx, func(tx storeport.Store) error {
			if _, err := tx.GetOrCreateConceptStateForUpdateInDomain(ctx, "L1", f.domain.ID, "a"); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			state := models.NewConceptStateInDomain("L1", f.domain.ID, "a")
			state.PMastery = 0.8
			return tx.UpsertConceptState(ctx, state)
		})
	}()
	select {
	case <-locked:
	case err := <-writer:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	revision := make(chan error, 1)
	go func() {
		revision <- s.CompareAndSwapCurriculum(ctx, "L1", f.domain.ID, 1, revisionForTest(f.curriculum))
	}()
	close(release)
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if err := <-revision; err != nil {
		t.Fatal(err)
	}
	state, err := s.GetConceptStateInDomain(ctx, "L1", f.domain.ID, "a")
	if err != nil || state.PMastery != 0.1 {
		t.Fatalf("concurrent observation escaped reset: %+v %v", state, err)
	}
	current, err := s.GetCurriculumSnapshot(ctx, "L1", f.domain.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Reconciliation.InvalidatedConceptIDs) != 1 {
		t.Fatalf("concurrent reset was not audited: %+v", current.Reconciliation)
	}
}
