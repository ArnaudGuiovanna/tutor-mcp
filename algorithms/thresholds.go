// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Package algorithms exposes the mastery threshold accessors. A single
// unified threshold applies to the BKT routing estimate, KST prerequisite
// satisfaction and the intermediate hint-independence check: BKT=KST=Mid=0.85.
// It is a compile-time constant, never read from the environment.

package algorithms

// MasteryThreshold is the unified mastery threshold shared by MasteryBKT,
// MasteryKST and MasteryMid.
const MasteryThreshold = 0.85

const (
	// RetentionAlertWarningThreshold is the FSRS retrievability cutoff
	// below which ComputeAlerts emits a FORGETTING warning.
	RetentionAlertWarningThreshold = 0.40

	// RetentionAlertCriticalThreshold is the stricter FORGETTING band.
	// Critical forgetting arbitrates away same-concept mastery/ZPD/plateau
	// nudges because retrieval recovery must be handled first.
	RetentionAlertCriticalThreshold = 0.30

	// RetentionRecallRoutingThreshold is intentionally higher than the
	// alert thresholds. Once a concept has been selected for work, the action
	// selector turns the activity into a recall below this retention. It does
	// not decide which concepts are due: the orchestrator schedules recall
	// from FSRS NextReview (desired retention FSRSDesiredRetention = 0.9), see
	// engine.RecallDue. The 0.40/0.30 bands are the user-facing alerts.
	RetentionRecallRoutingThreshold = 0.50
)

// MasteryBKT returns the routing threshold on the BKT estimate P(L). Reaching
// it means "estimated", never "mastered": it excludes a concept from the
// INSTRUCTION fringe, enters MAINTENANCE, opens the high-mastery rotation of
// the action selector and is one of the conditions of MASTERY_READY and of a
// demonstration. A demonstration additionally requires a passed trusted
// MASTERY_CHALLENGE or TRANSFER_PROBE (engine.AssessMasteryStatus).
func MasteryBKT() float64 {
	return MasteryThreshold
}

// MasteryKST returns the threshold for "prerequisite considered satisfied
// to unlock a successor in the KST frontier". Used by ComputeFrontier,
// ConceptStatus, OLM cluster classification and dashboard aggregations.
func MasteryKST() float64 {
	return MasteryThreshold
}

// MasteryMid returns the intermediate threshold used by hint-independence
// checks (engine/metacognition.go) and learning_negotiation prereq sanity
// (tools/negotiation.go): a concept solid enough that the system expects
// independent solving. It equals MasteryBKT.
func MasteryMid() float64 {
	return MasteryThreshold
}
