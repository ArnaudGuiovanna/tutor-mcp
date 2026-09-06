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
//     E2E test using NDiagnosticMax=3 to validate transitions faster);
//  2. logging of the active config at server startup for ops audit;
//  3. extensibility to per-domain configs in a future phase, without
//     refactoring all call sites.
//
// The struct is *not* mutated at runtime in production — call sites
// receive an immutable copy via OrchestratorInput.Config. Tests
// construct ad-hoc PhaseConfig values to drive specific scenarios.
type PhaseConfig struct {
	// DeltaHThreshold is the minimum reduction in mean binary entropy
	// of P(L) (relative to the entropy snapshotted at DIAGNOSTIC
	// entry) required to exit DIAGNOSTIC. OQ-2.2 = relative criterion
	// — an absolute threshold is sensitive to BKT defaults
	// (H(P(L)=0.1) ≈ 0.469) and would short-circuit the phase.
	DeltaHThreshold float64

	// NDiagnosticMax bounds required diagnostic concept coverage. Domains with
	// at most this many concepts cover every concept; larger domains cover this
	// many distinct concepts. Repeated items never consume the bound.
	NDiagnosticMax int

	// RetentionRecallThreshold is the FSRS Retrievability below which
	// a goal-relevant concept receives recall priority in either learning phase.
	// It follows the recall-routing threshold,
	// which is intentionally earlier than user-facing FORGETTING alerts.
	RetentionRecallThreshold float64

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
}

// NewDefaultPhaseConfig returns the canonical Phase 1 configuration.
// These are the values that go to production.
//
// Calibration history:
//   - DeltaHThreshold=0.2 : initial guess; revisit with E2E artifact data
//   - NDiagnosticMax=8    : cadrage utilisateur
//   - RetentionRecallThreshold=RetentionRecallRoutingThreshold : early recall routing
//   - GoalRelevantCutoff=0.0 : strict positive (OQ-2.7 = A)
func NewDefaultPhaseConfig() PhaseConfig {
	return PhaseConfig{
		DeltaHThreshold:          0.2,
		NDiagnosticMax:           8,
		RetentionRecallThreshold: algorithms.RetentionRecallRoutingThreshold,
		GoalRelevantCutoff:       0.0,
		AntiRepeatWindow:         DefaultAntiRepeatWindow,
	}
}
