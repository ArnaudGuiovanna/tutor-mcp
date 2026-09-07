package db

import (
	"context"
	"reflect"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestLearningEventsFreezeExposureWithoutScoring(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "events")
	d := *f.decision
	d.ID = "decision-events-protocol"
	d.Contract.DecisionID = d.ID
	d.Contract.LearningEventProtocol = models.LearningEventProtocol
	if err := s.CreatePedagogicalDecision(ctx, f.scope, &d); err != nil {
		t.Fatal(err)
	}
	f.decision = &d
	attempt := f.attempt(t, "attempt-events")
	if err := s.CreateAssessmentAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	actor := models.LegacyPrincipal("L1")
	request := models.LearningEventRequest{EventKey: "instruction-1", DomainID: f.domain.ID, ConceptID: "a", Kind: "instruction", AttemptID: attempt.ID}
	before := retentionCountTable(t, s, "interactions")
	event, replayed, err := s.RecordLearningEvent(ctx, actor, request)
	if err != nil || replayed || event.Source != "host_reported" {
		t.Fatalf("instruction: %+v %t %v", event, replayed, err)
	}
	again, replayed, err := s.RecordLearningEvent(ctx, actor, request)
	if err != nil || !replayed || !reflect.DeepEqual(event, again) {
		t.Fatalf("retry: %+v %t %v", again, replayed, err)
	}
	if retentionCountTable(t, s, "interactions") != before {
		t.Fatal("instruction created a scored response")
	}
	bad := request
	bad.EventKey = "feedback-before-response"
	bad.Kind = "feedback"
	if _, _, err := s.RecordLearningEvent(ctx, actor, bad); err == nil {
		t.Fatal("feedback before committed response")
	}
	submittedAt := time.Now().UTC()
	if err := s.SubmitAssessmentAttempt(ctx, "L1", attempt.ID, "answer", "answer-hash", submittedAt); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetAssessmentAttempt(ctx, "L1", attempt.ID)
	if err != nil || stored.EventProtocol != models.LearningEventProtocol || stored.PriorExposureAt == nil || !stored.PriorExposureAt.Equal(event.OccurredAt) {
		t.Fatalf("exposure not frozen: %+v %v", stored, err)
	}
	if retentionCountTable(t, s, "learning_events") != 2 {
		t.Fatal("response event missing or duplicated")
	}
	if err := s.SubmitAssessmentAttempt(ctx, "L1", attempt.ID, "answer", "answer-hash", submittedAt); err == nil {
		t.Fatal("duplicate response accepted")
	}
	if _, err := s.exec(ctx, `UPDATE assessment_attempts SET prior_exposure_at = NULL WHERE id = ?`, attempt.ID); err == nil {
		t.Fatal("frozen exposure is mutable")
	}
	if _, err := s.exec(ctx, `UPDATE learning_events SET kind = 'feedback' WHERE id = ?`, event.ID); err == nil {
		t.Fatal("event is mutable")
	}
	if _, _, err := s.RecordLearningEvent(ctx, models.LegacyPrincipal("L2"), request); err == nil {
		t.Fatal("foreign learner wrote event")
	}
	bad = request
	bad.Kind = "response"
	bad.EventKey = "fake-response"
	if _, _, err := s.RecordLearningEvent(ctx, actor, bad); err == nil {
		t.Fatal("public caller manufactured committed response")
	}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", f.domain.ID, f.curriculum.Version, revisionForTest(f.curriculum)); err != nil {
		t.Fatal(err)
	}
	var current int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM learning_events WHERE curriculum_invalidated_version = 0`).Scan(&current); err != nil || current != 0 {
		t.Fatalf("superseded exposures remain current: %d %v", current, err)
	}
}

func TestLearningEventCommitPrecedesConcurrentResponse(t *testing.T) {
	s := setupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newPedagogicalDecisionFixture(t, s, "L1", "event-order")
	d := *f.decision
	d.ID = "event-order-decision"
	d.Contract.DecisionID = d.ID
	d.Contract.LearningEventProtocol = models.LearningEventProtocol
	if err := s.CreatePedagogicalDecision(ctx, f.scope, &d); err != nil {
		t.Fatal(err)
	}
	f.decision = &d
	a := f.attempt(t, "event-order-attempt")
	if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	actor := models.LegacyPrincipal("L1")
	ready, release := make(chan struct{}), make(chan struct{})
	exposureDone := make(chan error, 1)
	responseDone := make(chan error, 1)
	go func() {
		exposureDone <- s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, _ storeport.Store) error {
			_, _, err := s.RecordLearningEvent(txCtx, actor, models.LearningEventRequest{EventKey: "concurrent-instruction", DomainID: f.domain.ID, ConceptID: "a", Kind: "instruction"})
			close(ready)
			if err != nil {
				return err
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-ready
	go func() { responseDone <- s.SubmitAssessmentAttempt(ctx, "L1", a.ID, "answer", "hash", time.Now().UTC()) }()
	select {
	case err := <-responseDone:
		close(release)
		t.Fatalf("response escaped pending exposure: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-exposureDone; err != nil {
		t.Fatal(err)
	}
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetAssessmentAttempt(ctx, "L1", a.ID)
	if err != nil || stored.PriorExposureAt == nil {
		t.Fatalf("missed committed exposure: %+v %v", stored, err)
	}
}

func TestLearningEventFailureCannotCommitResponseInOuterTransaction(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "event-rollback")
	d := *f.decision
	d.ID = "event-rollback-decision"
	d.Contract.DecisionID = d.ID
	d.Contract.LearningEventProtocol = models.LearningEventProtocol
	if err := s.CreatePedagogicalDecision(ctx, f.scope, &d); err != nil {
		t.Fatal(err)
	}
	f.decision = &d
	a := f.attempt(t, "event-rollback-attempt")
	if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	scope, err := s.resolveLearningScope(ctx, "L1", f.domain.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt fixture simulates a failing journal write after response update.
	if err := s.insertLearningEvent(ctx, scope.TenantID, "L1", scope.EnrollmentID, &models.LearningEvent{ID: "collision", EventKey: "response:" + reviewDigest([]byte(a.ID)), DomainID: f.domain.ID, ConceptID: "a", AttemptID: a.ID, Kind: "response", Source: "committed_response", CurriculumVersion: 1, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var submitErr error
	if err := s.WithTenantTx(ctx, f.scope, func(txCtx context.Context, _ storeport.Store) error {
		submitErr = s.SubmitAssessmentAttempt(txCtx, "L1", a.ID, "answer", "hash", time.Now().UTC())
		return nil // MCP can handle an error as data and commit the outer transaction.
	}); err != nil {
		t.Fatal(err)
	}
	if submitErr == nil {
		t.Fatal("fixture did not exercise failure")
	}
	stored, err := s.GetAssessmentAttempt(ctx, "L1", a.ID)
	if err != nil || stored.Status != models.AssessmentAttemptPrepared || stored.SubmittedAt != nil || stored.PriorExposureAt != nil {
		t.Fatalf("partial response escaped: %+v %v", stored, err)
	}
}
