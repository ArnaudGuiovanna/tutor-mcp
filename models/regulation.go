// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package models

// Phase identifies which regulation phase a learner is currently in.
// Decided by [2] PhaseController (engine/phase_fsm.go) and consumed by
// [4] ConceptSelector to choose the eligible-concept pool and scoring
// formula. See docs/regulation-design/04-concept-selector.md §4.
//
// Callers must pass an explicit phase — there is no implicit default
// at the SelectConcept boundary, by design (an unrecognised phase
// returns an explicit error rather than silently degrading; cf.
// OQ-4.1). Bootstrap differs by entry point:
//   - New domains created via init_domain are written with phase
//     PhaseDiagnostic explicitly (tools/domain.go).
//   - Pre-existing domains with NULL phase (created before the phase
//     column existed) are treated by the orchestrator as
//     PhaseInstruction in-memory (engine/orchestrator.go:75-77) until
//     a transition causes UpdateDomainPhase to persist a value.
type Phase string

const (
	// PhaseInstruction — default learning phase. Eligible pool is the
	// "external fringe": concepts whose prereqs are mastered and whose
	// own mastery is below MasteryBKT(). Score: goal_relevance × (1 -
	// mastery).
	PhaseInstruction Phase = "INSTRUCTION"

	// PhaseDiagnostic — adaptive placement phase. The next cold item
	// targets the median-depth unclassified concept of the prerequisite
	// graph (engine/placement.go); goal_relevance is ignored.
	PhaseDiagnostic Phase = "DIAGNOSTIC"

	// PhaseMaintenance — review phase. Eligible pool is mastered
	// concepts (PMastery >= MasteryBKT()). Score: (1 - retention) ×
	// goal_relevance.
	PhaseMaintenance Phase = "MAINTENANCE"
)
