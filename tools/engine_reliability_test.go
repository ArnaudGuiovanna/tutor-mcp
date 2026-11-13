// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// P0-1: massed same-day successes must not inflate FSRS stability.
func TestApplyInteraction_SameDaySuccessesDoNotInflateStability(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

	var initial, last *models.ConceptState
	for i := 0; i < 6; i++ {
		cs, meta, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
			Concept: "a", ActivityType: string(models.ActivityPractice), Success: true,
			ResponseTimeSeconds: 10, Confidence: 0.95, DomainID: domain.ID,
		}, start.Add(time.Duration(i)*10*time.Minute))
		if err != nil {
			t.Fatalf("applyInteraction %d: %v", i, err)
		}
		if i == 0 {
			copyCS := *cs
			initial = &copyCS
		} else if meta["fsrs_skip_reason"] != "same_day_success_keeps_memory_state" {
			t.Fatalf("interaction %d: expected same-day skip, got %v", i, meta["fsrs_skip_reason"])
		}
		last = cs
	}
	if last.Stability > 1.5*initial.Stability {
		t.Fatalf("stability inflated within one session: initial=%.3f final=%.3f", initial.Stability, last.Stability)
	}
	if last.ScheduledDays != initial.ScheduledDays || !last.LastReview.Equal(*initial.LastReview) || last.Difficulty != initial.Difficulty {
		t.Fatalf("same-day successes changed the schedule: initial=%+v final=%+v", initial, last)
	}

	// A same-day failure is still a lapse.
	failed, _, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept: "a", ActivityType: string(models.ActivityPractice), Success: false,
		ResponseTimeSeconds: 10, DomainID: domain.ID,
	}, start.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Stability >= initial.Stability || failed.CardState != string(algorithms.Relearning) {
		t.Fatalf("same-day failure must lapse the card: %+v", failed)
	}

	// The next day's success is a spaced review and is applied.
	nextDay, meta, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept: "b", ActivityType: string(models.ActivityPractice), Success: true,
		ResponseTimeSeconds: 10, DomainID: domain.ID,
	}, start)
	if err != nil || meta["fsrs_update_applied"] != true {
		t.Fatalf("first review of b: %v %v", err, meta)
	}
	spaced, meta, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept: "b", ActivityType: string(models.ActivityPractice), Success: true,
		ResponseTimeSeconds: 10, DomainID: domain.ID,
	}, start.Add(26*time.Hour))
	if err != nil || meta["fsrs_update_applied"] != true || spaced.Stability <= nextDay.Stability {
		t.Fatalf("a spaced success must update the memory state: err=%v meta=%v S %.3f -> %.3f", err, meta, nextDay.Stability, spaced.Stability)
	}
}

// P1-5a: an assisted success applies the learning transition only.
func TestApplyInteraction_AssistedSuccessSkipsCorrectObservation(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	now := time.Now().UTC()

	assisted, meta, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept: "a", ActivityType: string(models.ActivityPractice), Success: true,
		HintsRequested: 2, DomainID: domain.ID,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	ref := models.BKTReferenceParams()
	want := algorithms.BKTTransition(algorithms.BKTState{PMastery: 0.1, PLearn: ref.PLearn, PForget: ref.PForget}).PMastery
	if math.Abs(assisted.PMastery-want) > 1e-9 {
		t.Fatalf("assisted success mastery=%.4f, want transition-only %.4f", assisted.PMastery, want)
	}
	if meta["bkt_observation_applied"] != false {
		t.Fatalf("audit must record the skipped observation: %v", meta)
	}

	independent, _, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept: "b", ActivityType: string(models.ActivityPractice), Success: true, DomainID: domain.ID,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if independent.PMastery <= assisted.PMastery {
		t.Fatalf("independent success must count more than an assisted one: %.4f <= %.4f", independent.PMastery, assisted.PMastery)
	}
}

// P1-6: the FSRS rating no longer follows the host's free confidence.
func TestFSRSRatingForObservation(t *testing.T) {
	calibrated := &models.CalibrationRecord{ConceptID: "a", Predicted: 1}
	cases := []struct {
		name        string
		input       interactionInput
		pass, max   float64
		calibration *models.CalibrationRecord
		want        algorithms.Rating
	}{
		{"failure", interactionInput{Success: false, Confidence: 1}, 0, 0, nil, algorithms.Again},
		{"high free confidence is Good", interactionInput{Success: true, Confidence: 0.99}, 0, 0, nil, algorithms.Good},
		{"low free confidence is Good", interactionInput{Success: true, Confidence: 0.1}, 0, 0, nil, algorithms.Good},
		{"absent confidence is Good", interactionInput{Success: true}, 0, 0, nil, algorithms.Good},
		{"hints are Hard", interactionInput{Success: true, HintsRequested: 1}, 0, 0, nil, algorithms.Hard},
		{"pass at threshold is Hard", interactionInput{Success: true, AssessmentAttemptID: "x", AssessmentScore: 2}, 2, 3, nil, algorithms.Hard},
		{"comfortable pass is Good", interactionInput{Success: true, AssessmentAttemptID: "x", AssessmentScore: 2.5}, 2, 3, nil, algorithms.Good},
		{"full marks without calibration is Good", interactionInput{Success: true, AssessmentAttemptID: "x", AssessmentScore: 3, Confidence: 1}, 2, 3, nil, algorithms.Good},
		{"full marks with calibrated prediction is Easy", interactionInput{Success: true, AssessmentAttemptID: "x", AssessmentScore: 3, CalibrationID: "c"}, 2, 3, calibrated, algorithms.Easy},
		{"calibrated but not full marks is Good", interactionInput{Success: true, AssessmentAttemptID: "x", AssessmentScore: 2.8, CalibrationID: "c"}, 2, 3, calibrated, algorithms.Good},
		{"low calibrated prediction is Good", interactionInput{Success: true, AssessmentAttemptID: "x", AssessmentScore: 3, CalibrationID: "c"}, 2, 3, &models.CalibrationRecord{Predicted: 0.5}, algorithms.Good},
	}
	for _, tc := range cases {
		if got := fsrsRatingForObservation(tc.input, tc.pass, tc.max, tc.calibration); got != tc.want {
			t.Errorf("%s: rating=%d want %d", tc.name, got, tc.want)
		}
	}
}

// P1-9: an attempt-linked transfer score is the rubric ratio.
func TestRecordInteraction_LinkedTransferScoreFollowsRubric(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	prepared := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
		"domain_id":     domain.ID,
		"concept":       "a",
		"activity_type": "TRANSFER_PROBE",
		"observable":    "Apply concept a in a new context.",
		"task_text":     "Transfer task.",
		"rubric":        json.RawMessage(`{"criteria":[{"id":"correctness","max_score":2},{"id":"reasoning","max_score":1}],"passing_score":2}`),
	})
	if prepared.IsError {
		t.Fatalf("prepare: %q", resultText(prepared))
	}
	out := decodeResult(t, prepared)
	attemptID, _ := out["attempt_id"].(string)
	sessionID, _ := out["session_id"].(string)
	if submitted := callTool(t, deps, registerSubmitAssessmentAttempt, "L_owner", "submit_assessment_attempt", map[string]any{
		"attempt_id": attemptID, "learner_response": "Learner response.",
	}); submitted.IsError {
		t.Fatalf("submit: %q", resultText(submitted))
	}
	args := map[string]any{
		"domain_id": domain.ID, "session_id": sessionID, "concept": "a",
		"activity_type": "TRANSFER_PROBE", "success": true, "response_time_seconds": 20.0, "confidence": 0.7,
		"attempt_id": attemptID, "evaluator_id": "host-test", "evaluation_method": "host_llm",
		"transfer_dimension": "near", "transfer_score": 0.95,
		"rubric_score": json.RawMessage(`{"criteria_scores":[{"id":"correctness","score":2,"evidence":"ok"},{"id":"reasoning","score":0.5,"evidence":"partial"}]}`),
	}
	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", args)
	if !res.IsError || !strings.Contains(resultText(res), "transfer_score=0.83") {
		t.Fatalf("divergent transfer_score must be rejected with the derived value: %q", resultText(res))
	}
	args["transfer_score"] = 0.85
	res = callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", args)
	if res.IsError {
		t.Fatalf("record: %q", resultText(res))
	}
	records, err := store.GetTransferScoresInDomain(context.Background(), "L_owner", domain.ID, "a")
	if err != nil || len(records) != 1 {
		t.Fatalf("transfer records: %v %v", records, err)
	}
	if math.Abs(records[0].Score-2.5/3) > 1e-9 {
		t.Fatalf("stored transfer score=%.4f, want derived ratio %.4f", records[0].Score, 2.5/3)
	}
}

// P2-15: a routed activity before the due date is not a proactive review.
func TestApplyInteraction_ProactiveReviewRequiresLearnerInitiative(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if _, _, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept: "a", ActivityType: string(models.ActivityPractice), Success: true, DomainID: domain.ID,
	}, start); err != nil {
		t.Fatal(err)
	}
	for i, selfInitiated := range []bool{false, true} {
		if _, _, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
			Concept: "a", ActivityType: string(models.ActivityRecall), Success: true, DomainID: domain.ID,
			SelfInitiated: selfInitiated,
		}, start.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 2)
	if err != nil || len(recent) != 2 {
		t.Fatalf("recent: %v %v", recent, err)
	}
	if !recent[0].IsProactiveReview || recent[1].IsProactiveReview {
		t.Fatalf("only the self-initiated early review is proactive: newest=%v older=%v", recent[0].IsProactiveReview, recent[1].IsProactiveReview)
	}
}
