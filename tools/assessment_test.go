// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
)

func TestCanonicalContentHashRejectsMismatch(t *testing.T) {
	hash, err := canonicalContentHash("answer", "")
	if err != nil || len(hash) != 64 {
		t.Fatalf("hash=%q err=%v", hash, err)
	}
	if _, err := canonicalContentHash("different", hash); err == nil {
		t.Fatal("expected mismatched content hash to fail")
	}
}

func TestPrepareAssessmentAttemptLinksCanonicalSession(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
		"domain_id":     domain.ID,
		"concept":       "a",
		"activity_type": "PRACTICE",
		"observable":    "Apply the concept without a hint.",
		"task_text":     "Solve the task.",
		"rubric_json":   `{"criteria":[{"id":"correctness","max_score":1}],"passing_score":1}`,
	})
	if res.IsError {
		t.Fatalf("prepare attempt: %q", resultText(res))
	}
	out := decodeResult(t, res)
	sessionID, _ := out["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("prepared attempt has no durable session: %v", out)
	}
	attemptID, _ := out["attempt_id"].(string)
	attempt, err := store.GetAssessmentAttempt(context.Background(), "L_owner", attemptID)
	if err != nil || attempt.SessionID != sessionID {
		t.Fatalf("attempt/session mismatch: attempt=%+v err=%v", attempt, err)
	}
}

func TestDeriveAssessmentOutcomeUsesFrozenRubric(t *testing.T) {
	rubric, _, err := normalizeRubricJSON(`{
      "criteria":[
        {"id":"correctness","max_score":2},
        {"id":"reasoning","max_score":1}
      ],
      "passing_score":2.5
    }`)
	if err != nil {
		t.Fatal(err)
	}
	score, _, err := normalizeRubricScoreJSON(`{
      "criteria_scores":[
        {"id":"correctness","score":2,"evidence":"correct"},
        {"id":"reasoning","score":0.5,"evidence":"partial"}
      ]
    }`, rubric)
	if err != nil {
		t.Fatal(err)
	}
	total, passed, err := deriveAssessmentOutcome(rubric, score)
	if err != nil || total != 2.5 || !passed {
		t.Fatalf("total=%v passed=%v err=%v", total, passed, err)
	}
}

func TestDeriveAssessmentOutcomeRejectsContradictoryScores(t *testing.T) {
	rubric, _, _ := normalizeRubricJSON(`{
      "criteria":[{"id":"correctness","max_score":1}],
      "passing_score":1
    }`)
	tests := []map[string]any{
		{"criteria_scores": []map[string]any{{"id": "other", "score": 1.0}}},
		{"criteria_scores": []map[string]any{{"id": "correctness", "score": 2.0}}},
		{"criteria_scores": []map[string]any{}},
	}
	for _, score := range tests {
		if _, _, err := deriveAssessmentOutcome(rubric, score); err == nil {
			t.Fatalf("expected score to fail: %#v", score)
		}
	}
}

func prepareAndSubmitAssessment(t *testing.T, deps *Deps, domainID string) (attemptID, sessionID string) {
	t.Helper()
	prepared := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
		"domain_id":     domainID,
		"concept":       "a",
		"activity_type": "PRACTICE",
		"observable":    "Apply concept a without a hint.",
		"task_text":     "Produce the learner answer.",
		"rubric_json":   `{"criteria":[{"id":"correctness","max_score":2},{"id":"reasoning","max_score":1}],"passing_score":2.5}`,
	})
	if prepared.IsError {
		t.Fatalf("prepare attempt: %q", resultText(prepared))
	}
	out := decodeResult(t, prepared)
	attemptID, _ = out["attempt_id"].(string)
	sessionID, _ = out["session_id"].(string)
	if attemptID == "" || sessionID == "" {
		t.Fatalf("missing attempt/session identifiers: %v", out)
	}

	submitted := callTool(t, deps, registerSubmitAssessmentAttempt, "L_owner", "submit_assessment_attempt", map[string]any{
		"attempt_id":       attemptID,
		"learner_response": "A complete learner-owned response.",
	})
	if submitted.IsError {
		t.Fatalf("submit attempt: %q", resultText(submitted))
	}
	return attemptID, sessionID
}

func assessmentEvaluationArgs(domainID, sessionID, attemptID string, success bool) map[string]any {
	return map[string]any{
		"domain_id":                  domainID,
		"session_id":                 sessionID,
		"concept":                    "a",
		"activity_type":              "PRACTICE",
		"success":                    success,
		"response_time_seconds":      12.0,
		"confidence":                 0.8,
		"notes":                      "Evaluation of the committed response.",
		"attempt_id":                 attemptID,
		"evaluator_id":               "host-model-test-v1",
		"evaluation_method":          "host_llm",
		"evaluation_provenance_json": `{"trace_id":"trace-1"}`,
		"rubric_score_json":          `{"criteria_scores":[{"id":"correctness","score":2,"evidence":"correct"},{"id":"reasoning","score":0.5,"evidence":"adequate"}]}`,
	}
}

func TestAssessmentAttemptEvaluationIsAtomicLinkedAndSingleUse(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	attemptID, sessionID := prepareAndSubmitAssessment(t, deps, domain.ID)

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction",
		assessmentEvaluationArgs(domain.ID, sessionID, attemptID, true))
	if res.IsError {
		t.Fatalf("evaluate attempt: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["derived_success"] != true || out["evaluation_trusted"] != false || out["assessment_status"] != "evaluated" {
		t.Fatalf("unexpected assessment result: %v", out)
	}
	if out["evidence_verification"] != "assessment_linked_untrusted_evaluation" {
		t.Fatalf("assessment evidence marker=%v", out["evidence_verification"])
	}

	attempt, err := store.GetAssessmentAttempt(context.Background(), "L_owner", attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "evaluated" || !attempt.Passed || attempt.TrustedEvaluation || attempt.Score != 2.5 {
		t.Fatalf("unexpected stored evaluation: %+v", attempt)
	}
	interactions, err := store.GetRecentInteractionsInDomain(context.Background(), "L_owner", domain.ID, "a", 10)
	if err != nil || len(interactions) != 1 || interactions[0].AssessmentAttemptID != attemptID {
		t.Fatalf("assessment interaction link missing: interactions=%+v err=%v", interactions, err)
	}

	duplicate := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction",
		assessmentEvaluationArgs(domain.ID, sessionID, attemptID, true))
	if !duplicate.IsError || !strings.Contains(resultText(duplicate), "must be submitted") {
		t.Fatalf("expected single-use rejection, got error=%v text=%q", duplicate.IsError, resultText(duplicate))
	}
}

func TestAssessmentEvaluationRollsBackWhenInteractionWriteFails(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	attemptID, sessionID := prepareAndSubmitAssessment(t, deps, domain.ID)
	if _, err := store.RawDB().Exec(`CREATE TRIGGER fail_assessment_interaction
		BEFORE INSERT ON interactions
		WHEN NEW.assessment_attempt_id = '` + attemptID + `'
		BEGIN SELECT RAISE(ABORT, 'forced interaction failure'); END`); err != nil {
		t.Fatalf("install fault trigger: %v", err)
	}

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction",
		assessmentEvaluationArgs(domain.ID, sessionID, attemptID, true))
	if !res.IsError {
		t.Fatalf("expected forced persistence failure, got %q", resultText(res))
	}
	attempt, err := store.GetAssessmentAttempt(context.Background(), "L_owner", attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "submitted" || attempt.EvaluatedAt != nil {
		t.Fatalf("evaluation escaped rolled-back transaction: %+v", attempt)
	}
	interactions, err := store.GetRecentInteractionsInDomain(context.Background(), "L_owner", domain.ID, "a", 10)
	if err != nil || len(interactions) != 0 {
		t.Fatalf("failed transaction left an interaction: interactions=%+v err=%v", interactions, err)
	}
}

func TestAssessmentContradictorySuccessDoesNotConsumeAttempt(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	attemptID, sessionID := prepareAndSubmitAssessment(t, deps, domain.ID)

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction",
		assessmentEvaluationArgs(domain.ID, sessionID, attemptID, false))
	if !res.IsError || !strings.Contains(resultText(res), "contradicts") {
		t.Fatalf("expected contradiction rejection, got error=%v text=%q", res.IsError, resultText(res))
	}
	attempt, err := store.GetAssessmentAttempt(context.Background(), "L_owner", attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "submitted" || attempt.EvaluatedAt != nil {
		t.Fatalf("rejected evaluation consumed attempt: %+v", attempt)
	}
}
