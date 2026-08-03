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

	// DiagnosticItemsCount is retained as a compatibility name, but carries the
	// number of distinct concepts covered by hint-free, attempt-linked,
	// evaluated DIAGNOSTIC_ASSESSMENT interactions since PhaseChangedAt.
	DiagnosticItemsCount int

	// DiagnosticCoverageTarget is min(domain concept count, NDiagnosticMax).
	// Coverage is mandatory: entropy derived from repeated or unverified
	// observations cannot end the diagnostic early.
	DiagnosticCoverageTarget int

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

	// GoalRelevantBelowRetention is true when at least one
	// goal-relevant concept has FSRS retrievability strictly below
	// cfg.RetentionRecallThreshold. Triggers MAINTENANCE →
	// INSTRUCTION.
	GoalRelevantBelowRetention bool
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
//   - DIAGNOSTIC → INSTRUCTION: qualified distinct-concept coverage reaches
//     min(domain concepts, cfg.NDiagnosticMax). Entropy remains an audit signal
//     but cannot bypass coverage.
//
//   - INSTRUCTION → MAINTENANCE :
//     all goal-relevant concepts have an estimated PMastery above the routing
//     threshold (encoded via EstimatedGoalRelevant == TotalGoalRelevant > 0)
//
//   - MAINTENANCE → INSTRUCTION :
//     at least one goal-relevant concept has retention <
//     cfg.RetentionRecallThreshold
//
// Unknown current phase → no transition, returns the unknown phase
// echoed back with Transitioned=false. The caller (Orchestrate) is
// responsible for the *legacy* fallback (NULL phase → INSTRUCTION) —
// the FSM itself only processes recognised phases.
func EvaluatePhase(current models.Phase, obs PhaseObservables, cfg PhaseConfig) PhaseEvaluation {
	switch current {
	case models.PhaseDiagnostic:
		// Relative entropy criterion: only fire when we have a valid
		// snapshot AND the reduction reaches the threshold.
		hasSnapshot := obs.PhaseEntryEntropy > 0 && !math.IsNaN(obs.PhaseEntryEntropy)
		entropyDelta := obs.PhaseEntryEntropy - obs.MeanEntropy
		entropyExit := hasSnapshot && entropyDelta >= cfg.DeltaHThreshold
		coverageTarget := obs.DiagnosticCoverageTarget
		if coverageTarget <= 0 {
			coverageTarget = cfg.NDiagnosticMax
		}
		coverageReached := coverageTarget > 0 && obs.DiagnosticItemsCount >= coverageTarget

		if entropyExit && coverageReached {
			return PhaseEvaluation{
				From: current, To: models.PhaseInstruction, Transitioned: true,
				Rationale: fmt.Sprintf(
					"DIAGNOSTIC→INSTRUCTION: qualified concept coverage reached (%d/%d) and entropy reduction reached (%.3f >= %.3f bits)",
					obs.DiagnosticItemsCount, coverageTarget, entropyDelta, cfg.DeltaHThreshold),
			}
		}
		if coverageReached {
			return PhaseEvaluation{
				From: current, To: models.PhaseInstruction, Transitioned: true,
				Rationale: fmt.Sprintf(
					"DIAGNOSTIC→INSTRUCTION: qualified concept coverage reached (%d/%d distinct concepts)",
					obs.DiagnosticItemsCount, coverageTarget),
			}
		}
		return PhaseEvaluation{
			From: current, To: current, Transitioned: false,
			Rationale: fmt.Sprintf(
				"DIAGNOSTIC: delta=%.3f bits (threshold %.3f), qualified coverage=%d/%d concepts — stay",
				entropyDelta, cfg.DeltaHThreshold,
				obs.DiagnosticItemsCount, coverageTarget),
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
		if obs.GoalRelevantBelowRetention {
			return PhaseEvaluation{
				From: current, To: models.PhaseInstruction, Transitioned: true,
				Rationale: "MAINTENANCE→INSTRUCTION: one goal-relevant concept below the retention threshold",
			}
		}
		return PhaseEvaluation{
			From: current, To: current, Transitioned: false,
			Rationale: "MAINTENANCE: retention OK on all goal-relevants — stay",
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
