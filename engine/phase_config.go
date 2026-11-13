// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import "tutor-mcp/algorithms"

// PhaseConfig captures the tunable parameters of [2] PhaseController.
// Per OQ-2.6 (validated): exposed as a struct rather than as private
// constants to support
//
//  1. injection of alternative configs in integration tests (e.g. an
//     E2E test using a small NDiagnosticMax to validate transitions faster);
//  2. logging of the active config at server startup for ops audit;
//  3. extensibility to per-domain configs in a future phase, without
//     refactoring all call sites.
//
// The struct is *not* mutated at runtime in production — call sites
// receive an immutable copy via OrchestratorInput.Config. Tests
// construct ad-hoc PhaseConfig values to drive specific scenarios.
type PhaseConfig struct {
	// NDiagnosticMax is the qualified-item budget of one DIAGNOSTIC phase.
	// The adaptive placement stops earlier when every concept is classified.
	// Zero or negative disables the budget.
	NDiagnosticMax int

	// GoalRelevantCutoff defines which concepts count as
	// "goal-relevant" for the INSTRUCTION → MAINTENANCE transition
	// (and its reverse). OQ-2.7 = A: a concept is goal-relevant iff
	// goal_relevance[c] > GoalRelevantCutoff. Default 0 (any strictly
	// positive score qualifies). Concepts uncovered by the
	// goal_relevance vector are excluded by virtue of not being in
	// the map (consistent with [4] OQ-4.3 = B').
	GoalRelevantCutoff float64

	// AntiRepeatWindow is the value the orchestrator forwards into
	// GateInput.AntiRepeatWindow when calling [3] Gate. Default
	// DefaultAntiRepeatWindow=3. Test scenarios with small domains
	// can lower it to avoid excluding the entire eligible pool.
	AntiRepeatWindow int

	// MasteryExitThreshold is the hysteresis floor of MAINTENANCE. Entering
	// MAINTENANCE still requires every goal-relevant estimate to reach
	// algorithms.MasteryBKT(); leaving it requires one estimate to fall
	// below this lower value. With the per-transition forgetting term a
	// single failure drops an estimate from ~0.94 to ~0.68, so a single
	// threshold made the phase oscillate on every slip. Concepts between
	// the two thresholds stay in the MAINTENANCE pool and receive guided
	// practice. Zero or negative means "no hysteresis" (legacy behaviour).
	MasteryExitThreshold float64
}

// DefaultMasteryExitThreshold is the MAINTENANCE hysteresis floor. It reuses
// the historical KST prerequisite value so the two thresholds that already
// existed in the legacy profile (0.70 / 0.85) now describe one hysteresis band.
const DefaultMasteryExitThreshold = 0.70

// EffectiveMasteryExitThreshold resolves the configured floor, falling back
// to the routing threshold when unset so ad-hoc configs keep the strict rule.
func (cfg PhaseConfig) EffectiveMasteryExitThreshold() float64 {
	if cfg.MasteryExitThreshold > 0 {
		return cfg.MasteryExitThreshold
	}
	return algorithms.MasteryBKT()
}

// NewDefaultPhaseConfig returns the canonical Phase 1 configuration.
// These are the values that go to production.
//
// Calibration history:
//   - NDiagnosticMax=DefaultDiagnosticItemBudget : placement item budget
//   - GoalRelevantCutoff=0.0 : strict positive (OQ-2.7 = A)
func NewDefaultPhaseConfig() PhaseConfig {
	return PhaseConfig{
		NDiagnosticMax:       DefaultDiagnosticItemBudget,
		GoalRelevantCutoff:   0.0,
		AntiRepeatWindow:     DefaultAntiRepeatWindow,
		MasteryExitThreshold: DefaultMasteryExitThreshold,
	}
}
