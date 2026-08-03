// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Package engine — [3] Gate Controller.
//
// ApplyGate is component [3] of the regulation pipeline (see
// docs/regulation-design/03-gate-controller.md). It runs *before*
// [4] ConceptSelector and [5] ActionSelector at execution time:
// filters the candidate pool, restricts allowed actions per concept
// (e.g. misconception lock), and may short-circuit the pipeline with
// an escape action (OVERLOAD → close session).
//
// Pure function — no store access, no time, no network. Logging is
// limited to: slog.Error on phase corruption (cf. [4] OQ-4.1) and
// slog.Info on the misconception+unsatisfied-prereqs pathological case
// (cf. OQ-3.5 sub-b — surfaced for statistical analysis, not blocked).
//
// Wired into the runtime by engine.Orchestrate (see orchestrator.go).
// The standalone REGULATION_GATE flag only toggles the system-prompt
// documentation appendix in tools/prompt.go; the gate itself runs as
// part of the orchestrator regardless.
package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// ErrGateUnknownPhase is returned by ApplyGate when the caller passes
// a Phase value that is not one of {Instruction, Diagnostic,
// Maintenance}. Consistent with [4] OQ-4.1: explicit error, no silent
// fallback — version-skew bugs must surface immediately.
var ErrGateUnknownPhase = errors.New("gate: unknown phase")

// ErrInvalidGateResult is returned by GateResult.Validate when the
// multi-mode exclusivity invariant is violated (zero modes set, or
// more than one). Production callers don't need to call Validate —
// ApplyGate's mode constructors enforce the invariant by construction.
// Tests use Validate as a safety net.
var ErrInvalidGateResult = errors.New("gate result invariant violation")

// DefaultAntiRepeatWindow is the default value for
// GateInput.AntiRepeatWindow (OQ-3.4 = A): the N most-recent concepts
// are excluded from the candidate pool. The caller may override.
//
// N = 0 disables the anti-rep filter entirely (documented escape for
// tests and special modes).
//
// At runtime, the effective window is capped at len(input.Concepts) - 1
// to guarantee anti-rep never excludes more than (domain_size - 1)
// candidates — preventing NoCandidate on small domains (OQ-3.4 cap).
const DefaultAntiRepeatWindow = 3

// GateInput is the read-only context for one ApplyGate call. The
// caller (PR [2] orchestrator, eventually) is responsible for
// pre-deriving the lookups: ActiveMisconceptions via
// db.GetActiveMisconceptions, RecentConcepts from
// db.GetRecentInteractionsByLearner, Alerts from engine/alert.go.
type GateInput struct {
	Phase                models.Phase
	Concepts             []string
	States               map[string]*models.ConceptState
	Graph                models.KnowledgeSpace
	ActiveMisconceptions map[string]bool // concept → has-active misconception
	RecentConcepts       []string        // DESC, last-first
	Alerts               []models.Alert
	AntiRepeatWindow     int // 0 disables; default DefaultAntiRepeatWindow
}

// GateResult is the structured output of ApplyGate. It has three
// mutually exclusive modes:
//
//  1. EscapeAction != nil : pipeline short-circuit (OVERLOAD).
//  2. NoCandidate == true : signal for [2] to consider a phase switch.
//  3. len(AllowedConcepts) > 0 : standard case, [4] picks among them
//     and [5] honours ActionRestriction[concept] if non-nil.
//
// Direct field access is fine for *reading*. To *construct* a
// GateResult, callers should use newEscapeResult / newNoCandidateResult
// / newAllowResult (unexported — internal to this package). Hand-rolled
// multi-mode states are caught by Validate (OQ-3.1 garde-fou).
type GateResult struct {
	EscapeAction      *EscapeAction
	NoCandidate       bool
	AllowedConcepts   []string
	ActionRestriction map[string][]models.ActivityType
	Rationale         string
}

// EscapeAction describes a forced activity emitted by the Gate when
// rule 3 (OVERLOAD) fires. The orchestrator emits this directly,
// bypassing [4] and [5].
type EscapeAction struct {
	Type      models.ActivityType
	Format    string
	Rationale string
}

// Validate enforces the multi-mode exclusivity invariant: exactly one
// of {EscapeAction != nil, NoCandidate, len(AllowedConcepts) > 0}
// must be true.
//
// Production callers don't need to call this — ApplyGate's mode
// constructors below are the only documented entry points to
// GateResult, and each constructs exactly one valid mode. The
// Validate hook exists so tests can pin the invariant against
// hand-rolled regressions (cf. OQ-3.1 garde-fou).
func (g GateResult) Validate() error {
	modes := 0
	if g.EscapeAction != nil {
		modes++
	}
	if g.NoCandidate {
		modes++
	}
	if len(g.AllowedConcepts) > 0 {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("%w: %d modes set, expected exactly 1", ErrInvalidGateResult, modes)
	}
	return nil
}

// ─── Mode constructors (sole entry points, OQ-3.1) ─────────────────────────

func newEscapeResult(action EscapeAction, rationale string) GateResult {
	return GateResult{
		EscapeAction: &action,
		Rationale:    rationale,
	}
}

func newNoCandidateResult(rationale string) GateResult {
	return GateResult{
		NoCandidate: true,
		Rationale:   rationale,
	}
}

func newAllowResult(concepts []string, restrictions map[string][]models.ActivityType, rationale string) GateResult {
	if restrictions == nil {
		restrictions = map[string][]models.ActivityType{}
	}
	return GateResult{
		AllowedConcepts:   concepts,
		ActionRestriction: restrictions,
		Rationale:         rationale,
	}
}

// ─── ApplyGate ─────────────────────────────────────────────────────────────

// ApplyGate composes the four gate rules in priority order:
//
//	OVERLOAD escape > misconception (action lock) > prereq filter >
//	  anti-repetition filter
//
// With two key bypasses validated in OQ-3.5:
//
//   - misconception bypasses anti-repetition (a concept with an
//     active misconception is reintroduced if it had been filtered
//     by the recent buffer — fixing the error trumps diversity);
//   - misconception does NOT bypass prereq (sub-b): a concept whose
//     prereqs are unsatisfied stays excluded even when a misconception
//     is active. The pathological case is logged at INFO for
//     statistical analysis (likely a domain misconfig).
//
// Phase-specific behaviour (OQ-3.6):
//
//   - DIAGNOSTIC bypasses *only* the prereq filter — the diagnostic
//     phase is allowed to test concepts with formally-missing prereqs
//     to calibrate IRT theta. OVERLOAD, misconception, and anti-rep
//     remain active in DIAGNOSTIC.
//
//     Why misconception stays in DIAGNOSTIC : a live misconception is
//     a strong pedagogical signal we don't leave dangling. UX caveat:
//     the LLM sees a DEBUG_MISCONCEPTION emerge mid-diagnostic, which
//     can feel like a context switch. The system-prompt appendix
//     surfaces this so the LLM can frame it gracefully.
//
// Returns ErrGateUnknownPhase on phase invalid (no fallback).
func ApplyGate(input GateInput) (GateResult, error) {
	switch input.Phase {
	case models.PhaseInstruction, models.PhaseDiagnostic, models.PhaseMaintenance:
		// ok
	default:
		slog.Error("gate: unknown phase", "phase", string(input.Phase))
		return GateResult{}, fmt.Errorf("%w: %q", ErrGateUnknownPhase, string(input.Phase))
	}

	// ── Rule 3 — OVERLOAD escape (priority 1, OQ-3.5) ──────────────────
	for _, alert := range input.Alerts {
		if alert.Type == models.AlertOverload {
			return newEscapeResult(EscapeAction{
				Type:      models.ActivityCloseSession,
				Format:    "session_overload",
				Rationale: "session >= 45 min (OVERLOAD)",
			}, "OVERLOAD escape : close session"), nil
		}
	}

	kstThreshold := algorithms.MasteryKST()

	// ── Rule 2 — prereq filter (bypassed in DIAGNOSTIC) ────────────────
	prereqOK := make(map[string]bool, len(input.Concepts))
	if input.Phase == models.PhaseDiagnostic {
		for _, c := range input.Concepts {
			prereqOK[c] = true
		}
	} else {
		for _, c := range input.Concepts {
			prereqOK[c] = prereqsSatisfied(c, input.Graph, input.States, kstThreshold)
		}
	}

	// Pathological log : misconception on concept whose prereqs are
	// unsatisfied. OQ-3.5 sub-b confirmed: misconception does NOT
	// bypass prereq. We log at INFO (not Error / Warn) — the case is
	// pathological at the domain level (likely misconfig), worth a
	// statistical signal but not blocking. Phase=DIAGNOSTIC bypasses
	// prereq, so this only fires in INSTRUCTION / MAINTENANCE.
	if input.Phase != models.PhaseDiagnostic {
		for _, c := range input.Concepts {
			if !input.ActiveMisconceptions[c] {
				continue
			}
			if !prereqOK[c] {
				missing := missingPrereqs(c, input.Graph, input.States, kstThreshold)
				slog.Info("gate: misconception on concept with unsatisfied prereqs (pathological — possible domain misconfig)",
					"concept", c,
					"missing_prereq_count", len(missing),
					"phase", string(input.Phase))
			}
		}
	}

	// Build post-prereq pool (preserves input order for determinism).
	var afterPrereq []string
	for _, c := range input.Concepts {
		if prereqOK[c] {
			afterPrereq = append(afterPrereq, c)
		}
	}

	// ── Rule 4 — anti-rep with effective_N protection (OQ-3.4) ─────────
	//
	// Cap effective N at domain_size - 1 so the filter never excludes
	// every candidate. N = 0 disables the filter entirely.
	n := max(0, input.AntiRepeatWindow)
	maxExclusion := max(0, len(input.Concepts)-1)
	n = min(n, maxExclusion)
	var recentSet map[string]bool
	if n > 0 && len(input.RecentConcepts) > 0 {
		recentSet = make(map[string]bool, n)
		end := min(n, len(input.RecentConcepts))
		for _, c := range input.RecentConcepts[:end] {
			recentSet[c] = true
		}
	}

	forgettingSet := alertConceptSet(input.Alerts, models.AlertForgetting)

	// Apply anti-rep: keep concept iff (not in recent) OR (forgetting bypass)
	// OR (misconception bypass — OQ-3.5 sub-a).
	var finalCandidates []string
	for _, c := range afterPrereq {
		inRecent := recentSet[c]
		hasForgetting := forgettingSet[c]
		hasMisconception := input.ActiveMisconceptions[c]
		if !inRecent || hasForgetting || hasMisconception {
			finalCandidates = append(finalCandidates, c)
		}
	}

	if len(finalCandidates) == 0 {
		return newNoCandidateResult("tous candidats filtres par prereq/anti-rep"), nil
	}

	// ── Rule 5 — FORGETTING-Critical priority filter (issue #16) ───────
	//
	// If at least one AlertForgetting with Urgency=Critical references
	// a concept that survived prereq+anti-rep, restrict the pool to
	// those critical-forgetting concepts only. Mirrors the legacy
	// router's priority-1 RECALL bypass at the pool level: [4] and [5]
	// keep their normal contracts ([5]'s retention branch will produce
	// RECALL_EXERCISE naturally for these concepts).
	//
	// Conceptual priority: between OVERLOAD (rule 3) and the per-concept
	// misconception action lock (rule 1). Executed here because the
	// "survives" condition needs the post-prereq+anti-rep pool.
	//
	// If no critical-forgetting concept survived (all were filtered by
	// prereq or none was alerted Critical), the rule is a no-op and the
	// normal pool semantics apply.
	criticalForgettingSet := map[string]bool{}
	for _, a := range input.Alerts {
		if a.Type == models.AlertForgetting && a.Urgency == models.UrgencyCritical {
			criticalForgettingSet[a.Concept] = true
		}
	}
	forgettingRestrictionFired := false
	if len(criticalForgettingSet) > 0 {
		var restricted []string
		for _, c := range finalCandidates {
			if criticalForgettingSet[c] {
				restricted = append(restricted, c)
			}
		}
		if len(restricted) > 0 {
			finalCandidates = restricted
			forgettingRestrictionFired = true
		}
	}

	// ── Rule 1 — misconception action lock ─────────────────────────────
	//
	// Per concept with an active misconception in the final pool,
	// restrict the allowed ActivityTypes to {DEBUG_MISCONCEPTION}.
	// [5] ActionSelector already prioritises misconception — this
	// restriction makes the constraint *explicit* in the gate output
	// so the orchestrator (and the audit trail) records it.
	restrictions := map[string][]models.ActivityType{}
	for _, c := range finalCandidates {
		if input.ActiveMisconceptions[c] {
			restrictions[c] = []models.ActivityType{models.ActivityDebugMisconception}
		}
	}

	rationale := fmt.Sprintf(
		"%d candidats apres prereq+anti-rep (window effective=%d) ; %d sous restriction misconception",
		len(finalCandidates), n, len(restrictions))
	if forgettingRestrictionFired {
		rationale += " ; pool restreint aux concepts FORGETTING-Critical (issue #16)"
	}
	return newAllowResult(finalCandidates, restrictions, rationale), nil
}

// ─── Helpers (private) ─────────────────────────────────────────────────────

func prereqsSatisfied(concept string, graph models.KnowledgeSpace, states map[string]*models.ConceptState, kstThreshold float64) bool {
	for _, prereq := range graph.Prerequisites[concept] {
		cs := states[prereq]
		if cs == nil {
			// No state on a prereq ≡ never practiced ≡ mastery=0 by
			// convention ≡ not satisfied.
			return false
		}
		if math.IsNaN(cs.PMastery) || cs.PMastery < kstThreshold {
			return false
		}
	}
	return true
}

func missingPrereqs(concept string, graph models.KnowledgeSpace, states map[string]*models.ConceptState, kstThreshold float64) []string {
	var missing []string
	for _, prereq := range graph.Prerequisites[concept] {
		cs := states[prereq]
		if cs == nil {
			missing = append(missing, prereq)
			continue
		}
		if math.IsNaN(cs.PMastery) || cs.PMastery < kstThreshold {
			missing = append(missing, prereq)
		}
	}
	sort.Strings(missing) // deterministic for tests/logs
	return missing
}

func alertConceptSet(alerts []models.Alert, t models.AlertType) map[string]bool {
	s := map[string]bool{}
	for _, a := range alerts {
		if a.Type == t {
			s[a.Concept] = true
		}
	}
	return s
}
