// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"strings"
	"testing"

	"tutor-mcp/models"
)

// TestMetacognitiveNudgePriority pins the priority ordering used to rank
// competing metacognitive nudges: transfer > calibration > dependency >
// affect, with any other alert type scoring 0 (never surfaced).
func TestMetacognitiveNudgePriority(t *testing.T) {
	cases := []struct {
		t    models.AlertType
		want int
	}{
		{models.AlertTransferBlocked, 95},
		{models.AlertCalibrationDiverging, 90},
		{models.AlertDependencyIncreasing, 80},
		{models.AlertAffectNegative, 70},
		{models.AlertForgetting, 0}, // not a metacognitive nudge
		{models.AlertType("UNKNOWN"), 0},
	}
	for _, c := range cases {
		if got := metacognitiveNudgePriority(c.t); got != c.want {
			t.Errorf("metacognitiveNudgePriority(%s) = %d, want %d", c.t, got, c.want)
		}
	}

	// The relative ordering must be strict, so SliceStable in
	// BuildMetacognitiveNudgeCandidates yields the intended ranking.
	order := []models.AlertType{
		models.AlertTransferBlocked,
		models.AlertCalibrationDiverging,
		models.AlertDependencyIncreasing,
		models.AlertAffectNegative,
	}
	for i := 0; i+1 < len(order); i++ {
		if metacognitiveNudgePriority(order[i]) <= metacognitiveNudgePriority(order[i+1]) {
			t.Errorf("priority not strictly descending at %s vs %s", order[i], order[i+1])
		}
	}
}

// TestBuildMetacognitiveNudgeCandidates_OrderingAndFiltering feeds alerts in a
// deliberately scrambled order plus one non-metacognitive alert, and asserts
// the output is sorted by descending priority with the irrelevant alert dropped.
func TestBuildMetacognitiveNudgeCandidates_OrderingAndFiltering(t *testing.T) {
	learner := &models.Learner{ID: "L1"}
	domains := []*models.Domain{
		{ID: "D1", Name: "Algebra", PersonalGoal: "Pass the exam",
			Graph: models.KnowledgeSpace{Concepts: []string{"vectors"}}},
	}
	alerts := []models.Alert{
		{Type: models.AlertAffectNegative, Urgency: models.UrgencyWarning},
		{Type: models.AlertForgetting, Concept: "vectors", Urgency: models.UrgencyCritical}, // not metacognitive → dropped
		{Type: models.AlertTransferBlocked, Concept: "vectors", Urgency: models.UrgencyWarning},
		{Type: models.AlertDependencyIncreasing, Urgency: models.UrgencyWarning},
		{Type: models.AlertCalibrationDiverging, Urgency: models.UrgencyWarning, RecommendedAction: "calibration divergente"},
	}

	out := BuildMetacognitiveNudgeCandidates(learner, domains, alerts)

	if len(out) != 4 {
		t.Fatalf("expected 4 candidates (FORGETTING dropped), got %d", len(out))
	}
	wantKinds := []string{
		"metacog_transfer",    // 95
		"metacog_calibration", // 90
		"metacog_dependency",  // 80
		"metacog_affect",      // 70
	}
	for i, want := range wantKinds {
		if out[i].Kind != want {
			t.Errorf("candidate[%d].Kind = %q, want %q", i, out[i].Kind, want)
		}
		if out[i].AlertTag != want {
			t.Errorf("candidate[%d].AlertTag = %q, want %q", i, out[i].AlertTag, want)
		}
	}
	// Priorities must be non-increasing.
	for i := 0; i+1 < len(out); i++ {
		if out[i].Priority < out[i+1].Priority {
			t.Errorf("candidates not sorted by priority: %d before %d", out[i].Priority, out[i+1].Priority)
		}
	}

	// The transfer brief should carry the concept and a Feynman open loop.
	transfer := out[0].Brief
	if transfer.Concept != "vectors" {
		t.Errorf("transfer brief Concept = %q, want %q", transfer.Concept, "vectors")
	}
	if !strings.Contains(transfer.OpenLoop, "Feynman") {
		t.Errorf("transfer brief OpenLoop = %q, want it to mention Feynman", transfer.OpenLoop)
	}
	if transfer.DomainID != "D1" || transfer.DomainName != "Algebra" {
		t.Errorf("transfer brief domain = %q/%q, want D1/Algebra", transfer.DomainID, transfer.DomainName)
	}
}

// TestBuildMetacognitiveNudgeCandidates_EmptyInput covers the empty-input path:
// no alerts → no candidates, and a nil alert slice must not panic.
func TestBuildMetacognitiveNudgeCandidates_EmptyInput(t *testing.T) {
	if out := BuildMetacognitiveNudgeCandidates(nil, nil, nil); len(out) != 0 {
		t.Errorf("expected no candidates for nil input, got %d", len(out))
	}
	if out := BuildMetacognitiveNudgeCandidates(&models.Learner{ID: "L1"}, nil, []models.Alert{}); len(out) != 0 {
		t.Errorf("expected no candidates for empty alert slice, got %d", len(out))
	}
	// A purely non-metacognitive alert must produce nothing.
	out := BuildMetacognitiveNudgeCandidates(&models.Learner{ID: "L1"}, nil,
		[]models.Alert{{Type: models.AlertForgetting, Concept: "x"}})
	if len(out) != 0 {
		t.Errorf("expected non-metacognitive alert to be dropped, got %d candidates", len(out))
	}
}

// TestBuildOLMNudgeBrief_NilSnapshot covers the empty-input guard: a nil
// snapshot yields a zero-value brief, not a panic.
func TestBuildOLMNudgeBrief_NilSnapshot(t *testing.T) {
	brief := BuildOLMNudgeBrief(nil)
	if brief.Kind != "" || brief.Trigger != "" || brief.Concept != "" ||
		brief.WhyNow != "" || brief.NextAction != "" || brief.Version != 0 ||
		len(brief.Evidence) != 0 {
		t.Errorf("expected zero-value brief for nil snapshot, got %+v", brief)
	}
}

// TestBuildOLMNudgeBrief_UrgencyBranches exercises each FocusUrgency branch and
// the no-concept fallback, asserting the trigger/why-now copy and the
// concept-specific open loop / next action.
func TestBuildOLMNudgeBrief_UrgencyBranches(t *testing.T) {
	cases := []struct {
		name        string
		urgency     models.AlertUrgency
		concept     string
		wantTrigger string
	}{
		{"critical", models.UrgencyCritical, "recursion", "Priority review"},
		{"warning", models.UrgencyWarning, "recursion", "Current focus"},
		{"info default", models.UrgencyInfo, "recursion", "Next frontier"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := &OLMSnapshot{
				DomainID:     "D1",
				DomainName:   "CS",
				FocusConcept: c.concept,
				FocusReason:  "low retention",
				FocusUrgency: c.urgency,
				PersonalGoal: "Build a compiler",
				KSTProgress:  0.4,
			}
			brief := BuildOLMNudgeBrief(snap)

			if brief.Kind != "olm" {
				t.Errorf("Kind = %q, want olm", brief.Kind)
			}
			if brief.Trigger != c.wantTrigger {
				t.Errorf("Trigger = %q, want %q", brief.Trigger, c.wantTrigger)
			}
			if brief.Concept != c.concept {
				t.Errorf("Concept = %q, want %q", brief.Concept, c.concept)
			}
			if !strings.Contains(brief.OpenLoop, c.concept) {
				t.Errorf("OpenLoop = %q, want it to mention concept %q", brief.OpenLoop, c.concept)
			}
			if !strings.Contains(brief.NextAction, c.concept) {
				t.Errorf("NextAction = %q, want it to mention concept %q", brief.NextAction, c.concept)
			}
			if brief.GoalLink == "" {
				t.Error("expected non-empty GoalLink when PersonalGoal is set")
			}
		})
	}
}

// TestBuildOLMNudgeBrief_NoConcept covers the empty-concept fallback path,
// where trigger/why-now/open-loop use the generic copy.
func TestBuildOLMNudgeBrief_NoConcept(t *testing.T) {
	snap := &OLMSnapshot{DomainID: "D1", DomainName: "CS"}
	brief := BuildOLMNudgeBrief(snap)

	if brief.Trigger != "Learning state" {
		t.Errorf("Trigger = %q, want generic 'Learning state'", brief.Trigger)
	}
	if brief.Concept != "" {
		t.Errorf("Concept = %q, want empty", brief.Concept)
	}
	if strings.TrimSpace(brief.NextAction) == "" {
		t.Error("expected a non-empty generic NextAction")
	}
	if brief.GoalLink != "" {
		t.Errorf("expected empty GoalLink without a PersonalGoal, got %q", brief.GoalLink)
	}
}
