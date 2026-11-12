package engine

import (
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestAssessMasteryStatusEvidenceAxes(t *testing.T) {
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
	// The most recent trusted summative attempt failed: it blocks transfer
	// and also revokes the earlier demonstration (P1-3), leaving retention.
	if status.Transferred || status.Demonstrated || status.TransferReadiness != TransferReadinessBlocked || status.Stage != MasteryStageRetained {
		t.Fatalf("recent trusted transfer failure escaped blocking semantics: %+v", status)
	}
}

// P1-3: a demonstration needs a high estimate and a passed trusted
// MASTERY_CHALLENGE or TRANSFER_PROBE; a cold diagnostic never demonstrates.
func TestAssessMasteryStatusDemonstrationRequiresEstimateAndSummativeAttempt(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	high := &models.ConceptState{LearnerID: "L1", Concept: "fractions", CardState: "review", PMastery: 0.9, LastReview: &now, Stability: 10}
	low := &models.ConceptState{LearnerID: "L1", Concept: "fractions", CardState: "review", PMastery: 0.2, LastReview: &now, Stability: 10}
	diagnostic := masteryStatusAssessment("diag", "fractions", models.ActivityDiagnosticAssessment, true, now)
	challenge := masteryStatusAssessment("challenge", "fractions", models.ActivityMasteryChallenge, true, now)
	for _, tc := range []struct {
		name     string
		state    *models.ConceptState
		attempts []*models.AssessmentAttempt
		want     bool
	}{
		{"diagnostic with high estimate", high, []*models.AssessmentAttempt{diagnostic}, false},
		{"challenge without model state", nil, []*models.AssessmentAttempt{challenge}, false},
		{"challenge with low estimate", low, []*models.AssessmentAttempt{challenge}, false},
		{"challenge with high estimate", high, []*models.AssessmentAttempt{challenge}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := AssessMasteryStatus("L1", "fractions", tc.state, nil, nil, tc.attempts, now)
			if status.Demonstrated != tc.want || status.Retained {
				t.Fatalf("demonstrated=%v want %v: %+v", status.Demonstrated, tc.want, status)
			}
		})
	}
}

// P1-3: the most recent trusted summative attempt decides; a later failure
// revokes an earlier pass and a later pass restores it.
func TestAssessMasteryStatusLatestSummativeAttemptDecidesDemonstration(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cs := &models.ConceptState{LearnerID: "L1", Concept: "fractions", CardState: "review", PMastery: 0.9, LastReview: &now, Stability: 10}
	pass := masteryStatusAssessment("pass", "fractions", models.ActivityMasteryChallenge, true, now.Add(-48*time.Hour))
	fail := masteryStatusAssessment("fail", "fractions", models.ActivityTransferProbe, true, now.Add(-24*time.Hour))
	fail.Passed = false
	if status := AssessMasteryStatus("L1", "fractions", cs, nil, nil, []*models.AssessmentAttempt{fail, pass}, now); status.Demonstrated {
		t.Fatalf("a later trusted failure must revoke the demonstration: %+v", status)
	}
	repass := masteryStatusAssessment("repass", "fractions", models.ActivityMasteryChallenge, true, now.Add(-time.Hour))
	if status := AssessMasteryStatus("L1", "fractions", cs, nil, nil, []*models.AssessmentAttempt{fail, repass, pass}, now); !status.Demonstrated {
		t.Fatalf("a later trusted pass must restore the demonstration: %+v", status)
	}
}

func TestAssessMasteryStatusRetentionIsIndependentOfMasteryEstimate(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cs, recall, attempt := retentionFixture(now)
	cs.PMastery = 0.2
	interactions := []*models.Interaction{
		masteryStatusInteraction("L1", "fractions", models.ActivityPractice, true, now.Add(-48*time.Hour)),
		recall,
	}
	status := AssessMasteryStatus("L1", "fractions", cs, interactions, nil, []*models.AssessmentAttempt{attempt}, now)
	if status.Estimated || !status.Retained || status.Demonstrated || status.Transferred || status.Stage != MasteryStageRetained {
		t.Fatalf("observed delayed retrieval must remain separate from the BKT threshold: %+v", status)
	}
}

func TestAssessMasteryStatusTransferIsIndependentOfRetentionButRequiresTrustedEvidence(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	var transfers []*models.TransferRecord
	var attempts []*models.AssessmentAttempt
	for _, dimension := range []string{"near", "far", "teaching"} {
		id := "transfer-" + dimension
		transfers = append(transfers, &models.TransferRecord{
			LearnerID: "L1", ConceptID: "fractions", AssessmentAttemptID: id,
			ContextType: dimension, Score: 0.9, CreatedAt: now,
		})
		attempts = append(attempts, masteryStatusAssessment(id, "fractions", models.ActivityTransferProbe, true, now))
	}
	// Demonstration (and so transfer) requires the model estimate (P1-3), but
	// a card without any review history still has no retention claim.
	estimated := &models.ConceptState{LearnerID: "L1", Concept: "fractions", CardState: "new", PMastery: 0.9}
	status := AssessMasteryStatus("L1", "fractions", estimated, nil, transfers, attempts, now)
	if !status.Demonstrated || !status.Transferred || !status.Estimated || status.Retained || status.Stage != MasteryStageTransferred {
		t.Fatalf("trusted transfer evidence must not manufacture retention or depend on its absence: %+v", status)
	}
	for _, attempt := range attempts {
		attempt.TrustedEvaluation = false
	}
	status = AssessMasteryStatus("L1", "fractions", estimated, nil, transfers, attempts, now)
	if status.Demonstrated || status.Transferred {
		t.Fatalf("untrusted transfer observations must not establish either axis: %+v", status)
	}
}

func TestAssessMasteryStatusDemonstrationRejectsForeignAndFutureEvidence(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []string{"another learner", "future evaluation", "response after evaluation"} {
		t.Run(tc, func(t *testing.T) {
			attempt := masteryStatusAssessment("expert-performance", "fractions", models.ActivityMasteryChallenge, true, now)
			future := now.Add(time.Hour)
			switch tc {
			case "another learner":
				attempt.LearnerID = "L2"
			case "future evaluation":
				attempt.EvaluatedAt = &future
			case "response after evaluation":
				attempt.SubmittedAt = &future
			}
			estimated := &models.ConceptState{LearnerID: "L1", Concept: "fractions", CardState: "new", PMastery: 0.9}
			status := AssessMasteryStatus("L1", "fractions", estimated, nil, nil, []*models.AssessmentAttempt{attempt}, now)
			if status.Demonstrated || status.Transferred {
				t.Fatalf("invalid evidence established a claim: %+v", status)
			}
		})
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

func TestAssessMasteryStatus_TrustedHostLLMEvaluationDemonstrates(t *testing.T) {
	// The persistence projection grants TrustedEvaluation to a decision-bound
	// host_llm evaluation under the deployment policy; the engine must then
	// accept that method like the server-side ones.
	now := time.Now().UTC()
	attempt := masteryStatusAssessment("host-trusted", "a", models.ActivityMasteryChallenge, false, now)
	attempt.TrustedEvaluation = true
	estimated := &models.ConceptState{LearnerID: "L1", Concept: "a", CardState: "new", PMastery: 0.9}
	status := AssessMasteryStatus("L1", "a", estimated, nil, nil, []*models.AssessmentAttempt{attempt}, now)
	if !status.Demonstrated || status.Stage != MasteryStageDemonstrated {
		t.Fatalf("trusted host_llm evaluation must demonstrate: %+v", status)
	}
	untrusted := masteryStatusAssessment("host-raw", "a", models.ActivityMasteryChallenge, false, now)
	status = AssessMasteryStatus("L1", "a", estimated, nil, nil, []*models.AssessmentAttempt{untrusted}, now)
	if status.Demonstrated {
		t.Fatalf("a raw host_llm row without the trusted flag must not demonstrate: %+v", status)
	}
}

// P1-3: a demonstrated concept whose retention has decayed is fragile.
func TestClassifyOLMNodeFragileBeforeDemonstrated(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	last := now.Add(-90 * 24 * time.Hour)
	cs := &models.ConceptState{LearnerID: "L1", Concept: "a", CardState: "review", PMastery: 0.9, LastReview: &last, Stability: 2}
	status := MasteryStatus{Demonstrated: true, RetentionEstimate: 0.2}
	if got := classifyOLMNode(cs, status); got != olmNodeFragile {
		t.Fatalf("decayed demonstrated concept classified %d, want fragile", got)
	}
	status.RetentionEstimate = 0.95
	if got := classifyOLMNode(cs, status); got != olmNodeSolid {
		t.Fatalf("retained demonstrated concept classified %d, want solid", got)
	}
}

// P1-9: every linked trusted transfer record is scored by its rubric ratio,
// not by the host's auxiliary score.
func TestTrustedTransferRecordsUseRubricRatio(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	attempt := masteryStatusAssessment("t1", "a", models.ActivityTransferProbe, true, now)
	attempt.RubricJSON = `{"criteria":[{"id":"x","description":"d","max_score":2},{"id":"y","description":"d","max_score":2}],"passing_score":2.4}`
	attempt.Score = 3
	record := &models.TransferRecord{ConceptID: "a", AssessmentAttemptID: "t1", ContextType: "near", Score: 1, CreatedAt: now}
	verified := trustedTransferRecords("a", []*models.TransferRecord{record}, []*models.AssessmentAttempt{attempt})
	if len(verified) != 1 || verified[0].Score != 0.75 {
		t.Fatalf("verified score=%v, want rubric ratio 0.75", verified)
	}
	if record.Score != 1 {
		t.Fatalf("the caller's record must not be rewritten")
	}
}
