// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"tutor-mcp/models"
)

// A successful attempt-linked diagnostic on a dependent concept propagates its
// placement estimate to the prerequisite through record_interaction, writing
// only P(L): the prerequisite never receives an FSRS card.
func TestDiagnosticPlacementPropagatesToPrerequisitesWithoutFSRSCard(t *testing.T) {
	store, deps := setupToolsTest(t)
	ctx := context.Background()
	created := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "placement",
		"concepts":      []string{"base", "advanced"},
		"prerequisites": map[string][]string{"advanced": {"base"}},
	})
	if created.IsError {
		t.Fatalf("init_domain: %s", resultText(created))
	}
	domainID := decodeResult(t, created)["domain_id"].(string)
	if err := store.UpdateDomainPhase(ctx, domainID, models.PhaseDiagnostic, 0, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("enter diagnostic: %v", err)
	}
	before, err := store.GetConceptStatesByDomain(ctx, "L_owner", domainID)
	if err != nil {
		t.Fatal(err)
	}
	priorBase := -1.0
	for _, cs := range before {
		if cs.Concept == "base" {
			priorBase = cs.PMastery
		}
	}

	prepared := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
		"domain_id":     domainID,
		"concept":       "advanced",
		"activity_type": "DIAGNOSTIC_ASSESSMENT",
		"observable":    "Cold response without hints",
		"task_text":     "Solve the advanced item independently.",
		"rubric":        json.RawMessage(`{"criteria":[{"id":"correctness","max_score":1}],"passing_score":0.6}`),
	})
	if prepared.IsError {
		t.Fatalf("prepare: %s", resultText(prepared))
	}
	attempt := decodeResult(t, prepared)
	attemptID, _ := attempt["attempt_id"].(string)
	sessionID, _ := attempt["session_id"].(string)
	if submitted := callTool(t, deps, registerSubmitAssessmentAttempt, "L_owner", "submit_assessment_attempt", map[string]any{
		"attempt_id": attemptID, "learner_response": "Independent correct response.",
	}); submitted.IsError {
		t.Fatalf("submit: %s", resultText(submitted))
	}
	recorded := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"domain_id": domainID, "concept": "advanced", "activity_type": "DIAGNOSTIC_ASSESSMENT",
		"success": true, "response_time_seconds": 4.0, "confidence": 0.9,
		"session_id": sessionID, "attempt_id": attemptID,
		"evaluator_id": "host-test-v1", "evaluation_method": "host_llm",
		"rubric_score": json.RawMessage(`{"criteria_scores":[{"id":"correctness","score":1}]}`),
	})
	if recorded.IsError {
		t.Fatalf("record_interaction: %s", resultText(recorded))
	}

	after, err := store.GetConceptStatesByDomain(ctx, "L_owner", domainID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cs := range after {
		if cs.Concept != "base" {
			continue
		}
		if cs.PMastery <= priorBase {
			t.Fatalf("prerequisite estimate was not raised by the successful diagnostic: before=%v after=%v", priorBase, cs.PMastery)
		}
		if cs.CardState != "new" || cs.LastReview != nil || cs.Reps != 0 {
			t.Fatalf("placement created or scheduled an FSRS card on the prerequisite: %+v", cs)
		}
		return
	}
	t.Fatal("prerequisite state missing")
}
