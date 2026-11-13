// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"math"
	"testing"
	"tutor-mcp/algorithms"

	"tutor-mcp/models"
)

func defaultCfg() PhaseConfig {
	return NewDefaultPhaseConfig()
}

// ─── DIAGNOSTIC → INSTRUCTION ──────────────────────────────────────────────

func TestEvaluatePhase_DiagnosticToInstruction_PlacementClassifiedEverything(t *testing.T) {
	obs := PhaseObservables{
		MeanEntropy:           0.45,
		PhaseEntryEntropy:     0.50,
		DiagnosticItemsCount:  3,
		PlacementUnclassified: 0,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, defaultCfg())
	if !got.Transitioned || got.To != models.PhaseInstruction {
		t.Fatalf("expected transition once the placement is complete, got %+v", got)
	}
}

func TestEvaluatePhase_DiagnosticToInstruction_ItemBudgetSpent(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		DiagnosticItemsCount:  cfg.NDiagnosticMax,
		PlacementUnclassified: 4,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, cfg)
	if !got.Transitioned || got.To != models.PhaseInstruction {
		t.Fatalf("expected transition when the item budget is spent, got %+v", got)
	}
}

func TestEvaluatePhase_Diagnostic_StaysWhileUnclassifiedAndBudgetLeft(t *testing.T) {
	obs := PhaseObservables{
		MeanEntropy:           0.05, // large entropy drop is audit-only
		PhaseEntryEntropy:     0.50,
		DiagnosticItemsCount:  3,
		PlacementUnclassified: 2,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, defaultCfg())
	if got.Transitioned {
		t.Fatalf("entropy must not end the placement, got %+v", got)
	}
}

func TestEvaluatePhase_Diagnostic_NaNSnapshot_TreatedAsAbsent(t *testing.T) {
	obs := PhaseObservables{
		MeanEntropy:           0.10,
		PhaseEntryEntropy:     math.NaN(),
		DiagnosticItemsCount:  5,
		PlacementUnclassified: 1,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, defaultCfg())
	if got.Transitioned {
		t.Fatalf("NaN snapshot must not trigger a transition, got %+v", got)
	}
}

func TestBuildObservablesReportsPlacementProgress(t *testing.T) {
	graph := chainGraph(5)
	placement := ReplayPlacement(graph, nil, []models.DiagnosticObservation{{Concept: "c"}}, 12)
	pf := &pipelineFixtures{StatesByConcept: map[string]*models.ConceptState{}, Placement: placement}
	got := buildObservables(&models.Domain{Graph: graph}, pf, defaultCfg())
	if got.DiagnosticItemsCount != 1 || got.PlacementUnclassified != 2 {
		t.Fatalf("items=%d unclassified=%d, want 1 and 2 (a, b)", got.DiagnosticItemsCount, got.PlacementUnclassified)
	}
}

// ─── INSTRUCTION → MAINTENANCE ─────────────────────────────────────────────

func TestEvaluatePhase_InstructionToMaintenance_AllGoalMastered(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		EstimatedGoalRelevant: 5,
		TotalGoalRelevant:     5,
	}
	got := EvaluatePhase(models.PhaseInstruction, obs, cfg)
	if !got.Transitioned || got.To != models.PhaseMaintenance {
		t.Fatalf("expected MAINTENANCE, got %+v", got)
	}
}

func TestEvaluatePhase_InstructionToMaintenance_OneNotMastered_Stays(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		EstimatedGoalRelevant: 4,
		TotalGoalRelevant:     5,
	}
	got := EvaluatePhase(models.PhaseInstruction, obs, cfg)
	if got.Transitioned {
		t.Fatalf("expected stay, got transition: %+v", got)
	}
}

func TestEvaluatePhase_Instruction_NoGoalRelevantConcepts_Stays(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		EstimatedGoalRelevant: 0,
		TotalGoalRelevant:     0, // empty set : no transition (would be 0/0 == 0 trivially true otherwise)
	}
	got := EvaluatePhase(models.PhaseInstruction, obs, cfg)
	if got.Transitioned {
		t.Fatalf("zero goal-relevant must NOT auto-transition, got %+v", got)
	}
}

// ─── MAINTENANCE → INSTRUCTION ─────────────────────────────────────────────

func TestEvaluatePhase_MaintenanceToInstruction_AcquisitionGap(t *testing.T) {
	got := EvaluatePhase(models.PhaseMaintenance, PhaseObservables{
		EstimatedGoalRelevant: 1,
		TotalGoalRelevant:     2,
		BelowExitGoalRelevant: 1,
	}, defaultCfg())
	if !got.Transitioned || got.To != models.PhaseInstruction {
		t.Fatalf("a new acquisition gap should return to instruction, got %+v", got)
	}
}

func TestEvaluatePhase_Maintenance_SlipBetweenThresholdsStays(t *testing.T) {
	// One concept dropped below the routing threshold but not below the
	// exit floor: hysteresis keeps MAINTENANCE, where it receives practice.
	got := EvaluatePhase(models.PhaseMaintenance, PhaseObservables{
		EstimatedGoalRelevant: 1,
		TotalGoalRelevant:     2,
		BelowExitGoalRelevant: 0,
	}, defaultCfg())
	if got.Transitioned || got.To != models.PhaseMaintenance {
		t.Fatalf("a slip inside the hysteresis band must not leave MAINTENANCE, got %+v", got)
	}
}

func TestEvaluatePhase_Maintenance_NoHysteresisConfigFallsBackToRoutingThreshold(t *testing.T) {
	cfg := PhaseConfig{} // ad-hoc config without a floor
	if cfg.EffectiveMasteryExitThreshold() != algorithms.MasteryBKT() {
		t.Fatalf("unset floor must fall back to the routing threshold, got %v", cfg.EffectiveMasteryExitThreshold())
	}
	if defaultCfg().EffectiveMasteryExitThreshold() != DefaultMasteryExitThreshold {
		t.Fatalf("default config must carry the hysteresis floor")
	}
}

func TestEvaluatePhase_MaintenanceToInstruction_RetentionOK_Stays(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{EstimatedGoalRelevant: 1, TotalGoalRelevant: 1}
	got := EvaluatePhase(models.PhaseMaintenance, obs, cfg)
	if got.Transitioned {
		t.Fatalf("expected stay, got %+v", got)
	}
}

// ─── Phase non reconnue ────────────────────────────────────────────────────

func TestEvaluatePhase_UnknownPhase_NoTransition(t *testing.T) {
	cfg := defaultCfg()
	got := EvaluatePhase(models.Phase("BOGUS"), PhaseObservables{}, cfg)
	if got.Transitioned {
		t.Fatalf("unknown phase must not transition, got %+v", got)
	}
}

// ─── MeanBinaryEntropyOverGraph ────────────────────────────────────────────

func TestMeanEntropy_EmptyGraph_Zero(t *testing.T) {
	got := MeanBinaryEntropyOverGraph(models.KnowledgeSpace{}, nil)
	if got != 0 {
		t.Errorf("expected 0 on empty graph, got %f", got)
	}
}

func TestMeanEntropy_AllAtBKTDefault_Approximately047(t *testing.T) {
	// With P(L)=0.1 default, H ≈ 0.469 bits.
	graph := models.KnowledgeSpace{Concepts: []string{"A", "B", "C"}}
	states := map[string]*models.ConceptState{}
	for _, c := range graph.Concepts {
		cs := models.NewConceptState("L1", c)
		// PMastery=0.1 by default
		states[c] = cs
	}
	got := MeanBinaryEntropyOverGraph(graph, states)
	if math.Abs(got-0.469) > 0.005 {
		t.Errorf("expected mean ≈ 0.469, got %f", got)
	}
}

func TestMeanEntropy_AllSaturated_Zero(t *testing.T) {
	graph := models.KnowledgeSpace{Concepts: []string{"A", "B"}}
	states := map[string]*models.ConceptState{
		"A": {PMastery: 0.99},
		"B": {PMastery: 0.01},
	}
	got := MeanBinaryEntropyOverGraph(graph, states)
	// H(0.99) + H(0.01) ≈ 2 * 0.0808 ≈ 0.0808 mean
	if got > 0.15 {
		t.Errorf("expected mean near 0 at saturation, got %f", got)
	}
}

func TestMeanEntropy_NaN_Skipped(t *testing.T) {
	graph := models.KnowledgeSpace{Concepts: []string{"A"}}
	states := map[string]*models.ConceptState{
		"A": {PMastery: math.NaN()},
	}
	// NaN treated as 0 → H(0) = 0 (per BinaryEntropy at saturation).
	got := MeanBinaryEntropyOverGraph(graph, states)
	if math.IsNaN(got) || got != 0 {
		t.Errorf("expected 0 (NaN handled defensively), got %f", got)
	}
}
