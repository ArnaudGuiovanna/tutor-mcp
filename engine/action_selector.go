// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Package engine — [5] ActionSelector.
//
// SelectAction is component [5] of the regulation pipeline (see
// docs/regulation-design/05-action-selector.md). It is a pure function
// that, given a concept already chosen by the caller, decides *what to
// do on it*: which ActivityType, with which DifficultyTarget.
//
// It does not consume goal_relevance, phase, autonomy or session
// history — those are upstream concerns belonging to [1], [2], [4]
// and [3] respectively. This isolation is contractual: SelectConcept
// can change without affecting SelectAction.
//
// Wired into the runtime by engine.Orchestrate (see orchestrator.go).
// The standalone REGULATION_ACTION flag only toggles the system-prompt
// documentation appendix in tools/prompt.go — the selector itself runs
// as part of the orchestrator regardless.
package engine

import (
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// Action is the structured output of SelectAction. It carries enough
// information for the caller to wrap into a models.Activity (which
// requires Concept and PromptForLLM, supplied by the caller) and for
// the dashboard / audit trail to surface the rationale.
type Action struct {
	Type             models.ActivityType
	DifficultyTarget float64
	Format           string
	EstimatedMinutes int
	Rationale        string
}

// ActionHistory captures the per-concept counts that drive the
// high-mastery rotation (OQ-5.2) and the stability window (OQ-5.5).
// The caller derives this from the persisted interactions so that
// SelectAction itself stays pure.
//
// InteractionsAboveBKT: number of consecutive successful interactions on
// the concept since PMastery last crossed MasteryBKT(). Gates entry
// into the high-mastery rotation (HighMasteryStabilityWindow).
//
// MasteryChallengeCount, FeynmanCount, TransferCount: lifetime counts
// of activities of each type already emitted on this concept. Drive
// the gated cascade rotation (see selectHighMasteryAction).
type ActionHistory struct {
	InteractionsAboveBKT  int
	MasteryChallengeCount int
	FeynmanCount          int
	TransferCount         int
}

const (
	// ZPDOffset is the IRT 2PL latent-difficulty offset that targets a
	// pCorrect of 0.70 — centre of the ZPD per algorithms.IRTIsInZPD
	// (which accepts pCorrect in [0.55, 0.80]).
	//
	// Derivation. Solving for b in the 2PL with discrimination a=1:
	//
	//     0.70 = 1 / (1 + exp(-(θ - b)))
	//     exp(-(θ - b)) = 3/7
	//     b = θ - ln(7/3) = θ - 0.847
	//
	// See docs/regulation-design/05-action-selector.md §5 for the full
	// derivation and the rationale behind a=1.
	ZPDOffset = 0.847

	// HighMasteryStabilityWindow is the number of consecutive interactions
	// above MasteryBKT() required before the high-mastery rotation
	// engages (OQ-5.5 = B). Without this window, ping-pong oscillation
	// around the threshold (0.86 → MasteryChallenge → fail → 0.84 →
	// Practice → success → 0.86 → re-MasteryChallenge → ...) becomes
	// pathological.
	HighMasteryStabilityWindow = 3
)

// nanFallbackCount tracks how often SelectAction encountered a NaN
// (or nil ConceptState) and fell back to REST. Internal observability
// per OQ-5.6 = B — exposed via NaNFallbackCount() so tests and ops
// can measure the frequency of upstream corruption (e.g. F-1.3 BKT
// division by zero in the audit dette).
var nanFallbackCount atomic.Int64

// NaNFallbackCount returns the number of NaN-triggered REST fallbacks
// since process start. Used by tests; suitable for ops sampling.
func NaNFallbackCount() int64 {
	return nanFallbackCount.Load()
}

// SelectAction maps (concept, ConceptState, active misconception,
// history) → Action. Pure function: no store access, no network, no
// time.Now(). Logging is limited to the NaN-fallback path (slog.Error)
// because that is a corruption signal, not a normal control flow.
//
// Precedence cascade (each rule documented inline):
//
//  1. nil state or NaN in PMastery/Theta → REST (defence vs F-1.3).
//  2. Active misconception → DEBUG_MISCONCEPTION.
//     OQ-5.4 = A: misconception beats retention because recalling a
//     concept while a faulty belief is active just re-anchors the
//     error; fix the misconception first.
//  3. FSRS retention < algorithms.RetentionRecallRoutingThreshold (and card
//     not "new") → RECALL_EXERCISE. This routing cutoff is intentionally
//     higher than the user-facing FORGETTING alert bands: once a concept is
//     already selected for work, recall is preferred before retention reaches
//     warning/critical alert urgency.
//  4. Mastery brackets:
//     - p < 0.30                     → NEW_CONCEPT
//     - p < 0.70                     → PRACTICE (standard, diff=0.55)
//     - p < MasteryBKT()             → PRACTICE (ZPD via IRT theta)
//     - p ≥ MasteryBKT() unstable    → PRACTICE (ZPD), avoids ping-pong
//     - p ≥ MasteryBKT() stable (N=3)→ high-mastery rotation
//
// The threshold is read through algorithms.MasteryBKT() so the legacy
// vs unified profile (REGULATION_THRESHOLD) is honoured — no literal
// 0.85 in this file (drift test of [7] guards that).
func SelectAction(concept string, cs *models.ConceptState, mc *models.MisconceptionGroup, history ActionHistory) Action {
	return SelectActionAt(concept, cs, mc, history, time.Now().UTC())
}

// SelectActionAt is the clock-injected action decision used by the runtime.
// Retention is derived from the card's current age (now - LastReview), not
// from FSRS ElapsedDays, which records the interval before the last review.
func SelectActionAt(concept string, cs *models.ConceptState, mc *models.MisconceptionGroup, history ActionHistory, now time.Time) Action {
	// (1) NaN / nil guard — OQ-5.6 = B. Logged at ERROR (not WARN) to
	// surface corruption explicitly; counter incremented for sampling.
	if cs == nil {
		nanFallbackCount.Add(1)
		slog.Error("action_selector: nil concept state, falling back to REST",
			"concept", concept)
		return restFallback("concept_state nil — fallback REST")
	}
	if math.IsNaN(cs.PMastery) || math.IsNaN(cs.Theta) {
		nanFallbackCount.Add(1)
		slog.Error("action_selector: NaN in concept state, falling back to REST",
			"concept", concept,
			"p_mastery", cs.PMastery,
			"theta", cs.Theta)
		return restFallback("concept_state corrupted (NaN) — fallback REST")
	}

	// (2) Misconception override — OQ-5.4 = A: misconception > retention.
	// A live faulty belief on the concept means a recall would just
	// re-anchor the error; we confront the misconception directly via
	// DEBUG_MISCONCEPTION instead.
	if mc != nil {
		return Action{
			Type:             models.ActivityDebugMisconception,
			DifficultyTarget: 0.55,
			Format:           "misconception_targeted",
			EstimatedMinutes: 12,
			Rationale:        fmt.Sprintf("misconception active : %s", mc.MisconceptionType),
		}
	}

	// (3) FSRS retention override — but only if the card has been
	// reviewed at least once. A "new" card has no meaningful Stability
	// so Retrievability is uninformative (cf. engine/alert.go which
	// excludes new cards from FORGETTING).
	if cs.CardState != "new" {
		retention := algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
		if retention < algorithms.RetentionRecallRoutingThreshold {
			return Action{
				Type:             models.ActivityRecall,
				DifficultyTarget: 0.65,
				Format:           "code_completion",
				EstimatedMinutes: 8,
				Rationale:        fmt.Sprintf("retention FSRS basse (%.0f%%)", retention*100),
			}
		}
	}

	// (4) Mastery brackets.
	p := cs.PMastery
	switch {
	case p < 0.30:
		return Action{
			Type:             models.ActivityNewConcept,
			DifficultyTarget: 0.55,
			Format:           "introduction",
			EstimatedMinutes: 15,
			Rationale:        fmt.Sprintf("introduction : mastery %.2f < 0.30", p),
		}
	case p < 0.70:
		return Action{
			Type:             models.ActivityPractice,
			DifficultyTarget: 0.55,
			Format:           "practice_standard",
			EstimatedMinutes: 10,
			Rationale:        fmt.Sprintf("practice standard : mastery %.2f", p),
		}
	case p < algorithms.MasteryBKT():
		d := zpdDifficultyFromTheta(cs.Theta)
		return Action{
			Type:             models.ActivityPractice,
			DifficultyTarget: d,
			Format:           "practice_zpd",
			EstimatedMinutes: 12,
			Rationale:        fmt.Sprintf("ZPD via IRT θ=%.2f → diff=%.2f", cs.Theta, d),
		}
	default:
		// p ≥ MasteryBKT() — but require N=3 stable interactions before
		// engaging the high-mastery rotation (OQ-5.5 = B). Below the
		// window, stay in PRACTICE ZPD: the learner is plausibly above
		// threshold but not yet *consolidated* there.
		if history.InteractionsAboveBKT < HighMasteryStabilityWindow {
			d := zpdDifficultyFromTheta(cs.Theta)
			return Action{
				Type:             models.ActivityPractice,
				DifficultyTarget: d,
				Format:           "practice_zpd",
				EstimatedMinutes: 12,
				Rationale: fmt.Sprintf(
					"mastery %.2f >= seuil mais stabilite insuffisante (%d/%d)",
					p, history.InteractionsAboveBKT, HighMasteryStabilityWindow),
			}
		}
		return selectHighMasteryAction(history)
	}
}

// selectHighMasteryAction implements the gated cascade rotation
// (OQ-5.2 = A). Transition table — read top-down, first match wins:
//
//	┌─────────────────────────────────────────────┬──────────────────────┐
//	│ Condition                                   │ Emit                 │
//	├─────────────────────────────────────────────┼──────────────────────┤
//	│ MasteryChallengeCount == 0                  │ MasteryChallenge     │
//	│ FeynmanCount         < MasteryChallengeCount│ Feynman              │
//	│ TransferCount        < FeynmanCount         │ TransferChallenge    │
//	│ otherwise (cycle complete)                  │ MasteryChallenge ↻   │
//	└─────────────────────────────────────────────┴──────────────────────┘
//
// Trace: starting from (0,0,0), the rotation produces the sequence
// MC → F → T → MC → F → T → ... — i.e. MasteryChallenge → Feynman →
// TransferChallenge → MasteryChallenge as specified.
//
// Pedagogical rationale: build (MasteryChallenge) → explain (Feynman)
// → apply elsewhere (Transfer). The cycle restart on the default
// branch ensures the rotation never stalls indefinitely on Transfer:
// once each bucket has caught up, the next call routes back to a
// fresh MasteryChallenge under the *current* mastery snapshot.
func selectHighMasteryAction(history ActionHistory) Action {
	switch {
	case history.MasteryChallengeCount == 0:
		return Action{
			Type:             models.ActivityMasteryChallenge,
			DifficultyTarget: 0.75,
			Format:           "build_challenge",
			EstimatedMinutes: 45,
			Rationale:        "mastery >= seuil stable : premier mastery challenge",
		}
	case history.FeynmanCount < history.MasteryChallengeCount:
		return Action{
			Type:             models.ActivityFeynmanPrompt,
			DifficultyTarget: 0.50,
			Format:           "feynman_explanation",
			EstimatedMinutes: 15,
			Rationale:        "consolidation post-mastery via Feynman",
		}
	case history.TransferCount < history.FeynmanCount:
		return Action{
			Type:             models.ActivityTransferProbe,
			DifficultyTarget: 0.65,
			Format:           "transfer_novel_context",
			EstimatedMinutes: 20,
			Rationale:        "transfert hors contexte initial",
		}
	default:
		// Cycle complete — restart at MasteryChallenge.
		return Action{
			Type:             models.ActivityMasteryChallenge,
			DifficultyTarget: 0.75,
			Format:           "build_challenge",
			EstimatedMinutes: 45,
			Rationale:        "mastery stable: new challenge cycle",
		}
	}
}

// zpdDifficultyFromTheta maps IRT theta to a DifficultyTarget that
// targets pCorrect ≈ 0.70 in a 2PL model with discrimination a=1, then
// maps the latent difficulty to [0,1] via the logistic and clamps to
// the [0.30, 0.85] envelope.
//
// The clamp prevents two failure modes:
//   - very low θ producing a near-zero target (boredom / floor effect)
//   - very high θ producing a near-one target (frustration / ceiling)
//
// See ZPDOffset for the derivation of the constant.
func zpdDifficultyFromTheta(theta float64) float64 {
	if math.IsNaN(theta) {
		// Defensive — SelectAction already guards on NaN before calling
		// this helper. Returns a benign middle so callers can still
		// produce a valid Action even in pathological tests.
		return 0.55
	}
	b := theta - ZPDOffset
	d := 1.0 / (1.0 + math.Exp(-b))
	return clampActionDifficulty(d)
}

func clampActionDifficulty(d float64) float64 {
	if d < 0.30 {
		return 0.30
	}
	if d > 0.85 {
		return 0.85
	}
	return d
}

func restFallback(rationale string) Action {
	return Action{
		Type:             models.ActivityRest,
		DifficultyTarget: 0,
		Format:           "concept_state_corrupted",
		EstimatedMinutes: 0,
		Rationale:        rationale,
	}
}
