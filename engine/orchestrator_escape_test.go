// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

// TestOrchestrate_EscapePath_NoFringeAfterRetry covers the
// "pipeline_exhausted: NoFringe persists after retry" branch of
// Orchestrate (engine/orchestrator.go:142-146). The scenario:
//
//   - Phase = INSTRUCTION ; the domain's only concept is mastered, so
//     the external fringe in INSTRUCTION is empty (NoFringe).
//   - Orchestrate retries with MAINTENANCE (noFringeFallbackPhase).
//   - In MAINTENANCE, the mastered concept is NOT covered by
//     goal_relevance — eligible-pool empty, NoFringe again.
//   - Loop exits with the fallback Activity{Rest, "pipeline_exhausted…"}.
func TestOrchestrate_EscapePath_NoFringeAfterRetry(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.PhaseInstruction)

	// Set a goal_relevance vector that does NOT cover "A". This forces:
	//   - INSTRUCTION → resolveRelevance returns eligible=false → NoFringe
	//     (rationale: "aucun concept couvert par goal_relevance"), or
	//     fringe empty because mastered.
	//   - MAINTENANCE → mastered but not eligible → NoFringe.
	setGoalRelevance(t, store, domainID, map[string]float64{"DUMMY": 1.0})

	// Master "A" so external fringe is empty in INSTRUCTION.
	setMastery(t, store, "A", 0.95)

	activity, err := Orchestrate(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activity.Type != models.ActivityRest {
		t.Errorf("type: want REST, got %q", activity.Type)
	}
	if !strings.HasPrefix(activity.Rationale, "pipeline_exhausted") {
		t.Errorf("rationale prefix: want 'pipeline_exhausted...', got %q", activity.Rationale)
	}
	if activity.PromptForLLM == "" {
		t.Errorf("expected non-empty PromptForLLM on escape, got empty")
	}
}

// TestOrchestrate_EscapePath_MaintenanceFallbackToInstruction_BothNoFringe
// is the symmetric scenario: starting from MAINTENANCE, the fallback is
// INSTRUCTION, and the same "no eligible concept" outcome triggers the
// pipeline_exhausted branch. Pinning the symmetry guarantees the fallback
// table (noFringeFallbackPhase) is exercised in both directions.
func TestOrchestrate_EscapePath_MaintenanceFallbackToInstruction_BothNoFringe(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.PhaseMaintenance)

	// "A" is in goal_relevance but never mastered — so:
	//   - MAINTENANCE: needs PMastery >= MasteryBKT() → none mastered → NoFringe.
	//   - INSTRUCTION (fallback): A is in fringe BUT we strip it out via
	//     anti-rep with a recent interaction below, AND prereq blocks…
	// Easier: drop A out of goal_relevance entirely so both phases return NoFringe.
	setGoalRelevance(t, store, domainID, map[string]float64{"DUMMY": 1.0})

	activity, err := Orchestrate(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activity.Type != models.ActivityRest {
		t.Errorf("type: want REST, got %q", activity.Type)
	}
	if !strings.HasPrefix(activity.Rationale, "pipeline_exhausted") {
		t.Errorf("rationale: want pipeline_exhausted prefix, got %q", activity.Rationale)
	}
}

// TestOrchestrate_GateEscape_OVERLOAD_ComposesCloseSession exercises the
// OTHER escape path: Gate emits an EscapeAction (OVERLOAD). The
// orchestrator routes it through composeEscapeActivity
// (engine/orchestrator.go:445), which is at 0% coverage today.
//
// We force OVERLOAD by passing SessionStart far in the past. This
// pins the contract that Now is the current clock while SessionStart
// is the active-session boundary used by ComputeAlerts.
func TestOrchestrate_GateEscape_OVERLOAD_ComposesCloseSession(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 0.9, "B": 0.5})

	input := defaultInput(domainID)
	// OVERLOAD threshold is 45 min ; pass an "older than 45 min" timestamp.
	input.SessionStart = time.Now().UTC().Add(-1 * time.Hour)

	activity, err := Orchestrate(context.Background(), store, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activity.Type != models.ActivityCloseSession {
		t.Errorf("type: want CLOSE_SESSION, got %q", activity.Type)
	}
	if activity.Format != "session_overload" {
		t.Errorf("format: want session_overload, got %q", activity.Format)
	}
	if !strings.Contains(activity.Rationale, "OVERLOAD") {
		t.Errorf("rationale: want to mention OVERLOAD, got %q", activity.Rationale)
	}
	if activity.PromptForLLM == "" {
		t.Errorf("expected composeEscapeActivity to set a non-empty PromptForLLM")
	}
}

func TestOrchestrate_GateEscape_FreshSessionDoesNotCloseSession(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 0.9, "B": 0.5})

	input := defaultInput(domainID)
	input.SessionStart = time.Now().UTC()

	activity, err := Orchestrate(context.Background(), store, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activity.Type == models.ActivityCloseSession {
		t.Fatalf("fresh session must not close, got %+v", activity)
	}
}

// TestContainsActivityType_Direct exercises the slices.Contains wrapper
// directly. It is at 0% in the runtime path because the production path
// happens to always pass an action whose Type IS in the gate's
// ActionRestriction set (the gate restricts to {DEBUG_MISCONCEPTION} and
// [5] ActionSelector also selects DEBUG_MISCONCEPTION when a misconception
// is active). The function is still load-bearing — pin its behaviour
// independently.
func TestContainsActivityType_Direct(t *testing.T) {
	tests := []struct {
		name string
		set  []models.ActivityType
		t    models.ActivityType
		want bool
	}{
		{"empty set returns false", nil, models.ActivityRecall, false},
		{"single match", []models.ActivityType{models.ActivityDebugMisconception}, models.ActivityDebugMisconception, true},
		{"single non-match", []models.ActivityType{models.ActivityDebugMisconception}, models.ActivityRecall, false},
		{"multi-element first hit", []models.ActivityType{models.ActivityDebugMisconception, models.ActivityRecall}, models.ActivityDebugMisconception, true},
		{"multi-element last hit", []models.ActivityType{models.ActivityDebugMisconception, models.ActivityRecall}, models.ActivityRecall, true},
		{"multi-element no hit", []models.ActivityType{models.ActivityDebugMisconception, models.ActivityRecall}, models.ActivityFeynmanPrompt, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := containsActivityType(tc.set, tc.t)
			if got != tc.want {
				t.Errorf("containsActivityType(%v, %q) = %v, want %v", tc.set, tc.t, got, tc.want)
			}
		})
	}
}

// TestComposeEscapeActivity_Direct unit-tests the pure composer so the
// shape of its output is pinned even if the runtime path through the
// gate changes. This is the function that turns a Gate.EscapeAction into
// a models.Activity (engine/orchestrator.go:445).
func TestComposeEscapeActivity_Direct(t *testing.T) {
	esc := EscapeAction{
		Type:      models.ActivityCloseSession,
		Format:    "session_overload",
		Rationale: "OVERLOAD escape : close session",
	}
	got := composeEscapeActivity(esc)
	if got.Type != esc.Type {
		t.Errorf("Type: want %q, got %q", esc.Type, got.Type)
	}
	if got.Format != esc.Format {
		t.Errorf("Format: want %q, got %q", esc.Format, got.Format)
	}
	if got.Rationale != esc.Rationale {
		t.Errorf("Rationale: want %q, got %q", esc.Rationale, got.Rationale)
	}
	if got.PromptForLLM == "" {
		t.Errorf("PromptForLLM: want non-empty (canned LLM instruction)")
	}
	// Concept and DifficultyTarget are zero-value by construction — the
	// escape doesn't pick a concept.
	if got.Concept != "" {
		t.Errorf("Concept: want empty (escape has no concept), got %q", got.Concept)
	}
}
