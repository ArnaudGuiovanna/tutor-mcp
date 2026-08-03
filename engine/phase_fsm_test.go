// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"math"
	"testing"
	"time"

	"tutor-mcp/models"
)

func defaultCfg() PhaseConfig {
	return NewDefaultPhaseConfig()
}

// ─── DIAGNOSTIC → INSTRUCTION ──────────────────────────────────────────────

func TestEvaluatePhase_DiagnosticToInstruction_EntropyReductionMet(t *testing.T) {
	cfg := defaultCfg() // DeltaH=0.2, NMax=8
	obs := PhaseObservables{
		MeanEntropy:              0.20,
		PhaseEntryEntropy:        0.50, // delta = 0.30 >= 0.20
		DiagnosticItemsCount:     3,
		DiagnosticCoverageTarget: 3,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, cfg)
	if !got.Transitioned || got.To != models.PhaseInstruction {
		t.Fatalf("expected transition to INSTRUCTION via entropy reduction, got %+v", got)
	}
}

func TestEvaluatePhase_DiagnosticToInstruction_NItemsMaxReached(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		MeanEntropy:              0.45,
		PhaseEntryEntropy:        0.50, // delta = 0.05 < 0.20
		DiagnosticItemsCount:     8,
		DiagnosticCoverageTarget: 8,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, cfg)
	if !got.Transitioned || got.To != models.PhaseInstruction {
		t.Fatalf("expected transition via NMax, got %+v", got)
	}
}

func TestEvaluatePhase_DiagnosticToInstruction_NeitherCondition_Stays(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		MeanEntropy:              0.45,
		PhaseEntryEntropy:        0.50, // delta = 0.05 < 0.20
		DiagnosticItemsCount:     3,
		DiagnosticCoverageTarget: 8,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, cfg)
	if got.Transitioned {
		t.Fatalf("expected stay in DIAGNOSTIC, got transition: %+v", got)
	}
}

func TestEvaluatePhase_DiagnosticEntropyCannotBypassConceptCoverage(t *testing.T) {
	obs := PhaseObservables{
		MeanEntropy:              0.10,
		PhaseEntryEntropy:        0.50,
		DiagnosticItemsCount:     1,
		DiagnosticCoverageTarget: 4,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, defaultCfg())
	if got.Transitioned {
		t.Fatalf("entropy from repeated/unqualified observations bypassed coverage: %+v", got)
	}
}

func TestBuildObservablesDiagnosticCoverageTargetIsAllUpToEight(t *testing.T) {
	cfg := defaultCfg()
	pf := &pipelineFixtures{StatesByConcept: map[string]*models.ConceptState{}}
	for _, tc := range []struct {
		concepts int
		want     int
	}{{concepts: 3, want: 3}, {concepts: 8, want: 8}, {concepts: 12, want: 8}} {
		graph := make([]string, tc.concepts)
		for index := range graph {
			graph[index] = string(rune('A' + index))
		}
		domain := &models.Domain{Graph: models.KnowledgeSpace{Concepts: graph}}
		got := buildObservables(domain, pf, cfg, time.Time{})
		if got.DiagnosticCoverageTarget != tc.want {
			t.Fatalf("concepts=%d target=%d, want %d", tc.concepts, got.DiagnosticCoverageTarget, tc.want)
		}
	}
}

func TestEvaluatePhase_Diagnostic_NoSnapshot_OnlyNMaxApplies(t *testing.T) {
	cfg := defaultCfg()
	// PhaseEntryEntropy=0 → no snapshot (legacy/corrupted). Only NMax.
	obs := PhaseObservables{
		MeanEntropy:              0.10, // very low — would normally trigger entropy criterion
		PhaseEntryEntropy:        0,
		DiagnosticItemsCount:     3,
		DiagnosticCoverageTarget: 8,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, cfg)
	if got.Transitioned {
		t.Fatalf("expected stay (no snapshot, NMax not hit), got %+v", got)
	}

	// Same setup but NMax hit
	obs.DiagnosticItemsCount = 8
	got = EvaluatePhase(models.PhaseDiagnostic, obs, cfg)
	if !got.Transitioned {
		t.Fatalf("expected transition via NMax even without snapshot, got %+v", got)
	}
}

func TestEvaluatePhase_Diagnostic_NaNSnapshot_TreatedAsAbsent(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		MeanEntropy:              0.10,
		PhaseEntryEntropy:        math.NaN(),
		DiagnosticItemsCount:     5,
		DiagnosticCoverageTarget: 8,
	}
	got := EvaluatePhase(models.PhaseDiagnostic, obs, cfg)
	if got.Transitioned {
		t.Fatalf("NaN snapshot should not trigger entropy criterion, got %+v", got)
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

func TestEvaluatePhase_MaintenanceToInstruction_RetentionLow(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		GoalRelevantBelowRetention: true,
	}
	got := EvaluatePhase(models.PhaseMaintenance, obs, cfg)
	if !got.Transitioned || got.To != models.PhaseInstruction {
		t.Fatalf("expected INSTRUCTION on retention drop, got %+v", got)
	}
}

func TestEvaluatePhase_MaintenanceToInstruction_RetentionOK_Stays(t *testing.T) {
	cfg := defaultCfg()
	obs := PhaseObservables{
		GoalRelevantBelowRetention: false,
	}
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
