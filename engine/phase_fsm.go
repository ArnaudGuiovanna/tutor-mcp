// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"math"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// PhaseObservables is the *snapshot* of state the FSM evaluates
// against. Pre-fetched by the orchestrator from the store; the FSM
// itself is pure (no DB, no time, no logging).
type PhaseObservables struct {
	// MeanEntropy is the current mean binary entropy of P(L) across
	// all concepts in the domain graph. Used for DIAGNOSTIC →
	// INSTRUCTION transition (compared against PhaseEntryEntropy).
	MeanEntropy float64

	// PhaseEntryEntropy is the entropy snapshot taken at the moment
	// the phase was last set to DIAGNOSTIC. Zero (or NaN) means "no
	// snapshot available" — the relative criterion is omitted from the
	// rationale. Qualified concept coverage still governs exit.
	PhaseEntryEntropy float64

	// DiagnosticItemsCount is the number of qualified (hint-free,
	// attempt-linked, evaluated) DIAGNOSTIC_ASSESSMENT items applied to the
	// adaptive placement since PhaseChangedAt.
	DiagnosticItemsCount int

	// PlacementUnclassified is the number of domain concepts whose placement
	// is still ambiguous (see PlacementEstimate.Classified).
	PlacementUnclassified int

	// EstimatedGoalRelevant is the count of concepts that are
	// *goal-relevant* (per cfg.GoalRelevantCutoff) AND have a BKT estimate
	// above the routing threshold. This phase signal is not a demonstrated
	// mastery claim; learner-facing progress uses MasteryStatus.
	EstimatedGoalRelevant int

	// TotalGoalRelevant is the total count of goal-relevant concepts
	// (denominator of the phase check). Equality with
	// EstimatedGoalRelevant fires the INSTRUCTION → MAINTENANCE
	// transition.
	TotalGoalRelevant int

	// BelowExitGoalRelevant counts goal-relevant concepts whose estimate is
	// below cfg.MasteryExitThreshold (never-practised concepts included).
	// Any such concept fires MAINTENANCE → INSTRUCTION; concepts between
	// the exit and routing thresholds keep MAINTENANCE (hysteresis).
	BelowExitGoalRelevant int
}

// PhaseEvaluation is the FSM output: from-state, to-state, whether a
// transition occurred, plus a human-readable rationale used in audit
// logs and the E2E artifact.
type PhaseEvaluation struct {
	From         models.Phase
	To           models.Phase
	Transitioned bool
	Rationale    string
}

// EvaluatePhase computes the next phase given the current phase and
// a snapshot of observables. Pure function — same input always
// yields same output.
//
// Transitions (cf. docs/regulation-design/02-phase-controller.md §3) :
//
//   - DIAGNOSTIC → INSTRUCTION: the adaptive placement classified every
//     concept, or its item budget cfg.NDiagnosticMax is spent. The entropy
//     reduction since phase entry is reported for audit only.
//
//   - INSTRUCTION → MAINTENANCE :
//     all goal-relevant concepts have an estimated PMastery above the routing
//     threshold (encoded via EstimatedGoalRelevant == TotalGoalRelevant > 0)
//
//   - MAINTENANCE → INSTRUCTION :
//     at least one goal-relevant estimate is below the acquisition routing
//     threshold. Retention alone is handled by recall selection in either phase.
//
// Unknown current phase → no transition, returns the unknown phase
// echoed back with Transitioned=false. The caller (Orchestrate) is
// responsible for the *legacy* fallback (NULL phase → INSTRUCTION) —
// the FSM itself only processes recognised phases.
func EvaluatePhase(current models.Phase, obs PhaseObservables, cfg PhaseConfig) PhaseEvaluation {
	switch current {
	case models.PhaseDiagnostic:
		// The entropy delta is an audit signal only: it is reported when a
		// valid entry snapshot exists but never decides the transition.
		entropyNote := "no entropy snapshot"
		if obs.PhaseEntryEntropy > 0 && !math.IsNaN(obs.PhaseEntryEntropy) {
			entropyNote = fmt.Sprintf("entropy delta %.3f bits", obs.PhaseEntryEntropy-obs.MeanEntropy)
		}
		budget := cfg.NDiagnosticMax
		if obs.PlacementUnclassified == 0 {
			return PhaseEvaluation{
				From: current, To: models.PhaseInstruction, Transitioned: true,
				Rationale: fmt.Sprintf(
					"DIAGNOSTIC→INSTRUCTION: adaptive placement classified every concept after %d items (%s)",
					obs.DiagnosticItemsCount, entropyNote),
			}
		}
		if budget > 0 && obs.DiagnosticItemsCount >= budget {
			return PhaseEvaluation{
				From: current, To: models.PhaseInstruction, Transitioned: true,
				Rationale: fmt.Sprintf(
					"DIAGNOSTIC→INSTRUCTION: placement item budget spent (%d/%d), %d concept(s) left unclassified (%s)",
					obs.DiagnosticItemsCount, budget, obs.PlacementUnclassified, entropyNote),
			}
		}
		return PhaseEvaluation{
			From: current, To: current, Transitioned: false,
			Rationale: fmt.Sprintf(
				"DIAGNOSTIC: %d concept(s) unclassified, items %d/%d (%s) — stay",
				obs.PlacementUnclassified, obs.DiagnosticItemsCount, budget, entropyNote),
		}

	case models.PhaseInstruction:
		// Shift into spaced maintenance once instruction has raised every
		// goal-relevant estimate. This is routing, not a mastery declaration.
		if obs.TotalGoalRelevant > 0 && obs.EstimatedGoalRelevant == obs.TotalGoalRelevant {
			return PhaseEvaluation{
				From: current, To: models.PhaseMaintenance, Transitioned: true,
				Rationale: fmt.Sprintf(
					"INSTRUCTION→MAINTENANCE: %d/%d goal-relevant concepts estimated above routing threshold",
					obs.EstimatedGoalRelevant, obs.TotalGoalRelevant),
			}
		}
		return PhaseEvaluation{
			From: current, To: current, Transitioned: false,
			Rationale: fmt.Sprintf(
				"INSTRUCTION: %d/%d goal-relevant concepts estimated above routing threshold — stay",
				obs.EstimatedGoalRelevant, obs.TotalGoalRelevant),
		}

	case models.PhaseMaintenance:
		if obs.TotalGoalRelevant > 0 && obs.BelowExitGoalRelevant > 0 {
			return PhaseEvaluation{
				From: current, To: models.PhaseInstruction, Transitioned: true,
				Rationale: fmt.Sprintf(
					"MAINTENANCE→INSTRUCTION: %d goal-relevant concept(s) estimated below the exit threshold %.2f",
					obs.BelowExitGoalRelevant, cfg.EffectiveMasteryExitThreshold()),
			}
		}
		return PhaseEvaluation{
			From: current, To: current, Transitioned: false,
			Rationale: fmt.Sprintf(
				"MAINTENANCE: %d/%d goal-relevant concepts above routing threshold, none below exit threshold %.2f (hysteresis); recall needs are selected within this phase",
				obs.EstimatedGoalRelevant, obs.TotalGoalRelevant, cfg.EffectiveMasteryExitThreshold()),
		}

	default:
		// Unrecognised phase: no transition. The orchestrator decides
		// the fallback (typically INSTRUCTION for rows pre-flagged with
		// phase NULL — already mapped to an empty string).
		return PhaseEvaluation{
			From: current, To: current, Transitioned: false,
			Rationale: fmt.Sprintf("unrecognised phase %q — no transition", string(current)),
		}
	}
}

// MeanBinaryEntropyOverGraph computes the mean of H(P(L_c)) over all
// concepts in graph. Concepts absent from the states map are treated
// as having P(L) = 0 → H = 0 (uninformative — they contribute zero
// entropy to the mean, biasing it down). NaN P(L) values are skipped
// (defensive). Returns 0 for an empty graph.
//
// Used by the orchestrator to feed PhaseObservables.MeanEntropy and
// to snapshot PhaseEntryEntropy on transition INTO DIAGNOSTIC.
func MeanBinaryEntropyOverGraph(graph models.KnowledgeSpace, states map[string]*models.ConceptState) float64 {
	if len(graph.Concepts) == 0 {
		return 0
	}
	var sum float64
	var n int
	for _, c := range graph.Concepts {
		cs := states[c]
		var p float64
		if cs != nil && !math.IsNaN(cs.PMastery) {
			p = cs.PMastery
		}
		sum += algorithms.BinaryEntropy(p)
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}
