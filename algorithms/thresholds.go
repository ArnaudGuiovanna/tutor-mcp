// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Package algorithms exposes mastery threshold accessors with a runtime
// bascule between the legacy multi-threshold profile and a unified single
// threshold profile.
//
// Profiles
//
//   - "unified" (default, REGULATION_THRESHOLD!="off"): BKT=KST=Mid=0.85
//   - "legacy"  (REGULATION_THRESHOLD=off):             BKT=0.85, KST=0.70, Mid=0.80
//
// The bascule is the runtime expression of audit findings F-1.8, F-2.3
// and F-3.10 (multiple incompatible mastery thresholds — see
// docs/audit-report.md and docs/regulation-design/07-threshold-resolver.md).
//
// Promoted to default after eval/VERDICT_THRESHOLD_2026-05-04.md showed
// V3 and V4 cross their pass bars under unified (V3 +0.0544 vs +0.05
// bar, V4 0.7011 vs 0.70 bar) without degrading V1 or V1'. Operator
// opt-out remains available via REGULATION_THRESHOLD=off for rollback.

package algorithms

import "os"

const (
	// RetentionAlertWarningThreshold is the FSRS retrievability cutoff
	// below which ComputeAlerts emits a FORGETTING warning.
	RetentionAlertWarningThreshold = 0.40

	// RetentionAlertCriticalThreshold is the stricter FORGETTING band.
	// Critical forgetting arbitrates away same-concept mastery/ZPD/plateau
	// nudges because retrieval recovery must be handled first.
	RetentionAlertCriticalThreshold = 0.30

	// RetentionRecallRoutingThreshold is intentionally higher than the
	// alert thresholds. It is used after a concept has already been selected
	// for work, to route the activity toward recall before the user-facing
	// FORGETTING alert bands (<0.40 warning, <0.30 critical) are reached.
	RetentionRecallRoutingThreshold = 0.50
)

// MasteryBKT returns the threshold for "concept mastered per BKT P(L)".
// Used by the MASTERY_READY alert, mastery_challenge eligibility,
// transfer_challenge gating and the mastered-concept count in
// ComputeMetacognitiveAlerts.
func MasteryBKT() float64 {
	// Both profiles set BKT mastery to 0.85: the unified profile makes
	// BKT/KST/Mid share 0.85, and the legacy profile already pinned BKT at
	// 0.85, so the isUnifiedThreshold() branch was vestigial.
	return 0.85
}

// MasteryKST returns the threshold for "prerequisite considered satisfied
// to unlock a successor in the KST frontier". Used by ComputeFrontier,
// ConceptStatus, OLM cluster classification and dashboard aggregations.
func MasteryKST() float64 {
	if isUnifiedThreshold() {
		return 0.85
	}
	return 0.70
}

// MasteryMid returns the intermediate threshold used by hint-independence
// checks (engine/metacognition.go) and learning_negotiation prereq sanity
// (tools/negotiation.go). Both share the semantic intent "concept solid
// enough that the system expects independent solving" — splitting them is
// YAGNI today (cf docs/regulation-design/07-threshold-resolver.md OQ-7.4).
// In the unified profile this collapses to MasteryBKT.
func MasteryMid() float64 {
	if isUnifiedThreshold() {
		return 0.85
	}
	return 0.80
}

// isUnifiedThreshold reads the REGULATION_THRESHOLD env at every call.
// Default is unified; only the strict literal "off" opts out to legacy.
// Typos like "Off", "OFF", " off" are treated as unified — operators
// must type the canonical opt-out exactly, which prevents accidental
// rollback while preserving an explicit escape hatch.
//
// Lookup-each-call keeps the API testable via t.Setenv with no global
// reset helper. Cost is a single os.Getenv per accessor invocation,
// negligible at the call rate of get_next_activity. Replace with
// sync.OnceValue if profiling shows hot.
func isUnifiedThreshold() bool {
	return os.Getenv("REGULATION_THRESHOLD") != "off"
}
