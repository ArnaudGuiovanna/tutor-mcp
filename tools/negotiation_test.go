// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestLearningNegotiation_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerLearningNegotiation, "", "learning_negotiation", map[string]any{
		"session_id": "s1",
	})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestLearningNegotiation_NoDomain(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", map[string]any{
		"session_id": "s1",
	})
	// Issue #33: a missing domain (no DomainID supplied) returns the
	// uniform needs_domain_setup jsonResult — not an errorResult — so the
	// LLM can branch consistently across all chat-side tools.
	out := decodeResult(t, res)
	if got, _ := out["needs_domain_setup"].(bool); !got {
		t.Fatalf("expected needs_domain_setup=true, got %v", out)
	}
}

func TestLearningNegotiation_SystemPlanOnly(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", map[string]any{
		"session_id": "s1",
		"domain_id":  d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if _, ok := out["system_plan"]; !ok {
		t.Fatalf("expected system_plan, got %v", out)
	}
	if _, ok := out["learner_proposal"]; ok {
		t.Fatalf("learner_proposal should not be present, got %v", out["learner_proposal"])
	}
}

// TestLearningNegotiation_UnknownConceptRejected guards issue #92: a learner
// (or hallucinating LLM) can pass a concept name that does not exist in the
// active domain's Graph.Concepts. Without the validateConceptInDomain guard
// the negotiation tool silently builds a plan around the non-existent concept
// and returns accepted=true even though no prereqs exist. Mirror the
// record_interaction / transfer_challenge guard pattern.
func TestLearningNegotiation_UnknownConceptRejected(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math") // domain has {a, b}

	res := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", map[string]any{
		"session_id":      "s1",
		"learner_concept": "ghost",
		"domain_id":       d.ID,
	})
	if !res.IsError {
		t.Fatalf("expected error for unknown concept, got %q", resultText(res))
	}
	msg := resultText(res)
	if !strings.Contains(msg, "ghost") || !strings.Contains(msg, "not part of domain") {
		t.Fatalf("expected error mentioning unknown concept and domain, got %q", msg)
	}
}

func TestLearningNegotiation_LearnerProposalAccepted(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	// Seed concept state for a (learner's choice).
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.5
	cs.Difficulty = 5.0
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	res := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", map[string]any{
		"session_id":        "s1",
		"learner_concept":   "a",
		"learner_rationale": "envie",
		"domain_id":         d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["learner_proposal"] != "a" {
		t.Fatalf("expected learner_proposal=a, got %v", out["learner_proposal"])
	}
	if _, ok := out["accepted"]; !ok {
		t.Fatalf("expected accepted key, got %v", out)
	}
	if _, ok := out["accepted_plan"]; !ok {
		t.Fatalf("expected accepted_plan key, got %v", out)
	}
	if _, ok := out["counts_as_self_initiated"]; !ok {
		t.Fatalf("expected counts_as_self_initiated key, got %v", out)
	}
	if _, ok := out["override_persistence"]; !ok {
		t.Fatalf("expected persisted override receipt, got %v", out)
	}
}

func TestLearningNegotiation_StructuredOverrideAcceptedAndConsumedOnce(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.5
	cs.Difficulty = 5.0
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", map[string]any{
		"session_id":        "s1",
		"concept":           "a",
		"format":            "worked_example",
		"activity_type":     string(models.ActivityPractice),
		"scaffold":          true,
		"micro_diagnostic":  true,
		"learner_rationale": "need a smaller first step",
		"domain_id":         d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if got, _ := out["accepted"].(bool); !got {
		t.Fatalf("expected accepted structured override, got %v", out)
	}
	if _, ok := out["override"].(map[string]any); !ok {
		t.Fatalf("expected structured override in response, got %v", out["override"])
	}

	systemActivity := models.Activity{
		Type:             models.ActivityRecall,
		Concept:          "a",
		DifficultyTarget: 0.6,
		Format:           "mixed",
		EstimatedMinutes: 10,
	}
	gotActivity, consume, err := ConsumeLearningNegotiationOverride(context.Background(),
		store, "L_owner", d, systemActivity, nil, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if consume.Status != LearningNegotiationOverrideConsumeConsumed {
		t.Fatalf("expected consumed override, got %+v", consume)
	}
	if gotActivity.Type != models.ActivityPractice {
		t.Fatalf("expected PRACTICE, got %s", gotActivity.Type)
	}
	if gotActivity.Concept != "a" || gotActivity.Format != "worked_example" {
		t.Fatalf("unexpected consumed activity: %+v", gotActivity)
	}
	if gotActivity.EstimatedMinutes != 5 {
		t.Fatalf("expected micro diagnostic to cap duration at 5 minutes, got %d", gotActivity.EstimatedMinutes)
	}

	secondActivity, second, err := ConsumeLearningNegotiationOverride(context.Background(),
		store, "L_owner", d, systemActivity, nil, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != LearningNegotiationOverrideConsumeNone {
		t.Fatalf("expected one-shot override to be gone, got %+v", second)
	}
	if secondActivity != systemActivity {
		t.Fatalf("expected original system activity after one-shot consume, got %+v", secondActivity)
	}
}

func TestLearningNegotiation_InvalidActivityTypeRejected(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", map[string]any{
		"session_id":    "s1",
		"concept":       "a",
		"activity_type": "QUIZ",
		"domain_id":     d.ID,
	})
	if !res.IsError {
		t.Fatalf("expected invalid activity_type error, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "activity_type") {
		t.Fatalf("expected activity_type in error, got %q", resultText(res))
	}
}

func TestLearningNegotiation_RejectedProposalDoesNotPersistOverride(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math") // b requires a

	res := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", map[string]any{
		"session_id":        "s1",
		"concept":           "b",
		"learner_rationale": "want to skip ahead",
		"domain_id":         d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if got, _ := out["accepted"].(bool); got {
		t.Fatalf("expected rejected proposal, got %v", out)
	}
	if _, ok := out["override_persistence"]; ok {
		t.Fatalf("rejected proposal should not persist override, got %v", out["override_persistence"])
	}
	tradeoffs, _ := out["tradeoffs"].([]any)
	if len(tradeoffs) == 0 {
		t.Fatalf("expected rejection tradeoffs, got %v", out)
	}

	systemActivity := models.Activity{Type: models.ActivityRecall, Concept: "a", Format: "mixed", EstimatedMinutes: 10}
	_, consume, err := ConsumeLearningNegotiationOverride(context.Background(), store, "L_owner", d, systemActivity, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if consume.Status != LearningNegotiationOverrideConsumeNone {
		t.Fatalf("expected no persisted override after rejection, got %+v", consume)
	}
}

func TestConsumeLearningNegotiationOverride_ExpiredOverride(t *testing.T) {
	store, _ := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	now := time.Now().UTC()
	override := &LearningNegotiationOverride{
		DomainID:  d.ID,
		Concept:   "a",
		ExpiresAt: now.Add(-time.Minute),
		Activity: models.Activity{
			Type:             models.ActivityPractice,
			Concept:          "a",
			DifficultyTarget: 0.55,
			Format:           "practice_standard",
			EstimatedMinutes: 10,
		},
	}
	if _, err := PersistLearningNegotiationOverride(context.Background(), store, "L_owner", override, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	systemActivity := models.Activity{Type: models.ActivityRecall, Concept: "a", Format: "mixed", EstimatedMinutes: 10}
	gotActivity, consume, err := ConsumeLearningNegotiationOverride(context.Background(), store, "L_owner", d, systemActivity, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if consume.Status != LearningNegotiationOverrideConsumeExpired {
		t.Fatalf("expected expired override, got %+v", consume)
	}
	if gotActivity != systemActivity {
		t.Fatalf("expired override should return system activity, got %+v", gotActivity)
	}
}

func TestConsumeLearningNegotiationOverride_RejectsHardPrerequisiteBypass(t *testing.T) {
	store, _ := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math") // b requires a
	now := time.Now().UTC()
	override := &LearningNegotiationOverride{
		DomainID:  d.ID,
		Concept:   "b",
		ExpiresAt: now.Add(time.Hour),
		Activity: models.Activity{
			Type:             models.ActivityPractice,
			Concept:          "b",
			DifficultyTarget: 0.55,
			Format:           "practice_standard",
			EstimatedMinutes: 10,
		},
	}
	if _, err := PersistLearningNegotiationOverride(context.Background(), store, "L_owner", override, now); err != nil {
		t.Fatal(err)
	}

	systemActivity := models.Activity{Type: models.ActivityRecall, Concept: "a", Format: "mixed", EstimatedMinutes: 10}
	gotActivity, consume, err := ConsumeLearningNegotiationOverride(context.Background(), store, "L_owner", d, systemActivity, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if consume.Status != LearningNegotiationOverrideConsumeRejectedHardConstraint {
		t.Fatalf("expected hard-constraint rejection, got %+v", consume)
	}
	if !strings.Contains(consume.Reason, "prerequisites") {
		t.Fatalf("expected prerequisite reason, got %q", consume.Reason)
	}
	if gotActivity != systemActivity {
		t.Fatalf("hard-constraint rejection should return system activity, got %+v", gotActivity)
	}
}
