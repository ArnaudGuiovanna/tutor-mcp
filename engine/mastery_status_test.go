package engine

import (
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestAssessMasteryStatusEvidenceLadder(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	last := now.Add(-time.Hour)
	cs := &models.ConceptState{
		LearnerID: "L1", Concept: "fractions", CardState: "review",
		PMastery: 0.92, Stability: 10, Reps: 6, LastReview: &last,
	}
	interactions := []*models.Interaction{
		masteryStatusInteraction("L1", "fractions", models.ActivityPractice, true, now.Add(-50*time.Hour)),
		masteryStatusInteraction("L1", "fractions", models.ActivityRecall, true, now.Add(-25*time.Hour)),
		masteryStatusInteraction("L1", "fractions", models.ActivityPractice, true, now.Add(-4*time.Hour)),
		masteryStatusInteraction("L1", "fractions", models.ActivityMasteryChallenge, true, now.Add(-3*time.Hour)),
		masteryStatusInteraction("L1", "fractions", models.ActivityFeynmanPrompt, true, now.Add(-2*time.Hour)),
		masteryStatusInteraction("L1", "fractions", models.ActivityTransferProbe, true, now.Add(-time.Hour)),
	}
	interactions[1].AssessmentAttemptID = "retention-recall"
	assessments := []*models.AssessmentAttempt{
		masteryStatusAssessment("retention-recall", "fractions", models.ActivityRecall, false, now.Add(-25*time.Hour)),
		masteryStatusAssessment("mastery", "fractions", models.ActivityMasteryChallenge, true, now.Add(-3*time.Hour)),
	}

	status := AssessMasteryStatus("L1", "fractions", cs, interactions, nil, assessments, now)
	if !status.Estimated || !status.Retained || !status.Demonstrated || status.Transferred {
		t.Fatalf("unexpected demonstrated stage without broad transfer: %+v", status)
	}
	if status.Stage != MasteryStageDemonstrated {
		t.Fatalf("stage=%q, want demonstrated", status.Stage)
	}

	var transfers []*models.TransferRecord
	for _, dimension := range []string{"near", "far", "debugging", "teaching"} {
		attemptID := "transfer-" + dimension
		transfers = append(transfers, &models.TransferRecord{ConceptID: "fractions", AssessmentAttemptID: attemptID, ContextType: dimension, Score: 0.85})
		transfers[len(transfers)-1].CreatedAt = now.Add(-time.Duration(len(transfers)) * time.Hour)
		assessments = append(assessments, masteryStatusAssessment(attemptID, "fractions", models.ActivityTransferProbe, true, transfers[len(transfers)-1].CreatedAt))
	}
	status = AssessMasteryStatus("L1", "fractions", cs, interactions, transfers, assessments, now)
	if !status.Transferred || status.Stage != MasteryStageTransferred {
		t.Fatalf("expected transferred stage, got %+v", status)
	}

	// A later trusted failure must survive the trusted read filter and block
	// transfer, even though three-plus older dimensions passed.
	failureAt := now.Add(-30 * time.Minute)
	failure := masteryStatusAssessment("transfer-near-failure", "fractions", models.ActivityTransferProbe, true, failureAt)
	failure.Passed = false
	assessments = append(assessments, failure)
	transfers = append(transfers, &models.TransferRecord{
		ConceptID: "fractions", AssessmentAttemptID: failure.ID,
		// Deliberately contradictory auxiliary score: the trusted failed
		// assessment outcome remains authoritative and must force failure.
		ContextType: "near", Score: 0.95, CreatedAt: failureAt,
	})
	status = AssessMasteryStatus("L1", "fractions", cs, interactions, transfers, assessments, now)
	if status.Transferred || status.TransferReadiness != TransferReadinessBlocked || status.Stage != MasteryStageDemonstrated {
		t.Fatalf("recent trusted transfer failure escaped blocking semantics: %+v", status)
	}
}

func TestAssessMasteryStatusHighBKTAloneIsOnlyEstimated(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	last := now.Add(-time.Hour)
	cs := &models.ConceptState{LearnerID: "L1", Concept: "dates", CardState: "review", PMastery: 0.95, Stability: 10, LastReview: &last}
	status := AssessMasteryStatus("L1", "dates", cs, nil, nil, nil, now)
	if !status.Estimated || status.Retained || status.Demonstrated || status.Transferred {
		t.Fatalf("high BKT without evidence must not be demonstrated: %+v", status)
	}
}

func TestAssessMasteryStatusUntrustedOrUnlinkedEvidenceCannotCreateClaims(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	last := now.Add(-time.Hour)
	cs := &models.ConceptState{LearnerID: "L1", Concept: "dates", CardState: "review", PMastery: 0.95, Stability: 10, LastReview: &last}
	interactions := []*models.Interaction{
		masteryStatusInteraction("L1", "dates", models.ActivityPractice, true, now.Add(-72*time.Hour)),
		masteryStatusInteraction("L1", "dates", models.ActivityRecall, true, now.Add(-24*time.Hour)),
	}
	untrusted := masteryStatusAssessment("untrusted", "dates", models.ActivityMasteryChallenge, false, now.Add(-time.Hour))
	transfer := &models.TransferRecord{ConceptID: "dates", AssessmentAttemptID: "missing", ContextType: "far", Score: 1}
	status := AssessMasteryStatus("L1", "dates", cs, interactions, []*models.TransferRecord{transfer}, []*models.AssessmentAttempt{untrusted}, now)
	if status.Retained || status.Demonstrated || status.Transferred || status.TransferReadiness != TransferReadinessUnobserved {
		t.Fatalf("untrusted/unlinked evidence escaped the ladder: %+v", status)
	}
	if status.RetentionEvidence != RetentionEvidenceUnverifiedOnly {
		t.Fatalf("unlinked delayed retrieval trust=%q, want %q", status.RetentionEvidence, RetentionEvidenceUnverifiedOnly)
	}
}

func TestAssessMasteryStatusDelayedPublicObservationWithoutAttemptNeverRetained(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	last := now.Add(-time.Hour)
	cs := &models.ConceptState{LearnerID: "L1", Concept: "dates", CardState: "review", PMastery: 0.95, Stability: 30, LastReview: &last, Reps: 6}
	interactions := []*models.Interaction{
		masteryStatusInteraction("L1", "dates", models.ActivityPractice, true, now.Add(-72*time.Hour)),
		masteryStatusInteraction("L1", "dates", models.ActivityRecall, true, now.Add(-24*time.Hour)),
	}
	status := AssessMasteryStatus("L1", "dates", cs, interactions, nil, nil, now)
	if status.Retained || status.Stage != MasteryStageEstimated {
		t.Fatalf("a >24h unlinked public observation established retention: %+v", status)
	}
	if status.RetentionEvidence != RetentionEvidenceUnverifiedOnly {
		t.Fatalf("retention evidence=%q, want explicit unverified marker", status.RetentionEvidence)
	}
}

func masteryStatusInteraction(learnerID, concept string, activity models.ActivityType, success bool, at time.Time) *models.Interaction {
	return &models.Interaction{
		LearnerID: learnerID, Concept: concept, ActivityType: string(activity),
		Success: success, CreatedAt: at,
	}
}

func masteryStatusAssessment(id, concept string, activity models.ActivityType, trusted bool, at time.Time) *models.AssessmentAttempt {
	submittedAt := at.Add(-time.Minute)
	evaluatedAt := at
	method := models.EvaluationMethodHostLLM
	if trusted {
		method = models.EvaluationMethodExternal
	}
	return &models.AssessmentAttempt{
		ID: id, LearnerID: "L1", ConceptID: concept, ActivityType: string(activity),
		Status: models.AssessmentAttemptEvaluated, Passed: true, TrustedEvaluation: trusted,
		EvaluationMethod: method, SubmittedAt: &submittedAt, EvaluatedAt: &evaluatedAt,
	}
}
