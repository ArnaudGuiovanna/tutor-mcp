// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Historical standalone FadeController API, retained for compatibility.
// Decide is pure and deterministic, with no I/O or clock. Its output is not
// consumed by activity selection, motivation or the webhook scheduler.
// REGULATION_FADE exposes descriptive diagnostics only; autonomy scores do not
// withdraw help or alter delivery cadence.
//
// docs/regulation-design/06-fade-controller.md records the historical proposal;
// docs/runtime-pedagogique-2026-09.md describes the current runtime.
package engine

// AutonomyTrend mirrors the values produced by
// computeAutonomyTrend (engine/metacognition.go): "improving",
// "stable", "declining". Any other value is treated as "stable".
type AutonomyTrend string

const (
	AutonomyTrendImproving AutonomyTrend = "improving"
	AutonomyTrendStable    AutonomyTrend = "stable"
	AutonomyTrendDeclining AutonomyTrend = "declining"
)

// HintLevel describes the historical scaffolding setting returned by Decide.
// The runtime does not use it to suppress hints or change motivation briefs.
type HintLevel string

const (
	HintLevelFull    HintLevel = "full"
	HintLevelPartial HintLevel = "partial"
	HintLevelNone    HintLevel = "none"
)

// WebhookFrequency describes the historical cadence proposal returned by
// Decide. It does not control the scheduler's notification policy.
type WebhookFrequency string

const (
	WebhookFrequencyDaily  WebhookFrequency = "daily"
	WebhookFrequencyWeekly WebhookFrequency = "weekly"
	WebhookFrequencyOff    WebhookFrequency = "off"
)

// ZPDAggressiveness retains the historical targeting labels returned by Decide.
// They are neither a calibrated pCorrect target nor an input to live selection.
type ZPDAggressiveness string

const (
	ZPDAggressivenessGentle ZPDAggressiveness = "gentle"
	ZPDAggressivenessNormal ZPDAggressiveness = "normal"
	ZPDAggressivenessPush   ZPDAggressiveness = "push"
)

// FadeParams is the standalone bundle returned by Decide. Its public fields
// and JSON names remain compatible with historical callers/responses. Current
// get_next_activity calls do not generate this bundle from descriptive scores.
type FadeParams struct {
	HintLevel              HintLevel         `json:"hint_level"`
	WebhookFrequency       WebhookFrequency  `json:"webhook_frequency"`
	ZPDAggressiveness      ZPDAggressiveness `json:"zpd_aggressiveness"`
	ProactiveReviewEnabled bool              `json:"proactive_review_enabled"`
}

// fadeTier is the internal coarse classification driving the four
// output fields jointly. We map (score, trend) to a tier, then a tier
// to FadeParams. This keeps the table small (3 tiers) and easy to
// audit.
type fadeTier int

const (
	tierLow fadeTier = iota
	tierMid
	tierHigh
)

// Decide is the pure mapping from autonomy state to fade parameters.
//
// Mapping (see docs/regulation-design/06-fade-controller.md §4):
//
//	tier base:     score < 0.3 -> Low, 0.3 <= score < 0.7 -> Mid,
//	               score >= 0.7 -> High.
//	trend modifier: improving -> +1 tier, stable -> 0, declining -> -1
//	               tier (clamped to [Low, High]).
//
// Tier defaults (HintLevel, WebhookFrequency, ZPDAggressiveness,
// ProactiveReviewEnabled):
//
//	Low  -> full,    daily,   gentle, true
//	Mid  -> partial, weekly,  normal, true
//	High -> none,    off,     push,   false
//
// Edge cases:
//   - score < 0 or score > 1: clamped (Low / High respectively).
//   - score = NaN: treated as Mid (NaN comparisons fall through both
//     branches).
//   - trend not in {"improving","stable","declining"}: treated as
//     "stable".
func Decide(score float64, trend AutonomyTrend) FadeParams {
	base := tierFromScore(score)
	final := applyTrend(base, trend)
	return paramsForTier(final)
}

// tierFromScore is the score-only classification, before trend
// modulation. Boundaries are inclusive at the lower bound (>= 0.3
// is Mid, >= 0.7 is High). NaN inputs fall through both checks and
// land in Mid.
func tierFromScore(score float64) fadeTier {
	// Defensive clamp first to avoid surprises on NaN+arithmetic
	// downstream consumers might do.
	if score < 0.3 {
		// Includes negatives; explicit ordering means NaN does
		// NOT enter this branch (NaN < 0.3 is false), so NaN
		// continues to Mid below.
		return tierLow
	}
	if score >= 0.7 {
		return tierHigh
	}
	return tierMid
}

// applyTrend slides the base tier up/down by one cran depending on
// the trend, clamping at the ends. Unknown trend strings degrade to
// "stable" (no movement) by virtue of the switch's default branch.
func applyTrend(base fadeTier, trend AutonomyTrend) fadeTier {
	switch trend {
	case AutonomyTrendImproving:
		if base == tierHigh {
			return tierHigh
		}
		return base + 1
	case AutonomyTrendDeclining:
		if base == tierLow {
			return tierLow
		}
		return base - 1
	default:
		// Stable, "", or any other string: no movement.
		return base
	}
}

// paramsForTier maps the final tier to the bundle of four parameters.
// The mapping is intentionally one-shot per tier — see OQ-6.3 in the
// design doc for the rationale (vs per-field modulation).
func paramsForTier(t fadeTier) FadeParams {
	switch t {
	case tierLow:
		return FadeParams{
			HintLevel:              HintLevelFull,
			WebhookFrequency:       WebhookFrequencyDaily,
			ZPDAggressiveness:      ZPDAggressivenessGentle,
			ProactiveReviewEnabled: true,
		}
	case tierHigh:
		return FadeParams{
			HintLevel:              HintLevelNone,
			WebhookFrequency:       WebhookFrequencyOff,
			ZPDAggressiveness:      ZPDAggressivenessPush,
			ProactiveReviewEnabled: false,
		}
	default:
		// tierMid (and any unexpected fadeTier value, which the type
		// system should prevent — but we degrade safely to Mid).
		return FadeParams{
			HintLevel:              HintLevelPartial,
			WebhookFrequency:       WebhookFrequencyWeekly,
			ZPDAggressiveness:      ZPDAggressivenessNormal,
			ProactiveReviewEnabled: true,
		}
	}
}
