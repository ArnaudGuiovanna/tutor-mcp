// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// AutonomyInput holds the data needed to compute autonomy metrics.
type AutonomyInput struct {
	Interactions    []*models.Interaction
	ConceptStates   []*models.ConceptState
	CalibrationBias float64
	SessionGap      time.Duration // default: 2h
}

// ComputeAutonomyMetrics computes the four autonomy components.
// Each component is 25% of the final score.
func ComputeAutonomyMetrics(input AutonomyInput) models.AutonomyMetrics {
	now := time.Now().UTC()

	// 1. Initiative rate: % of sessions self-initiated
	initiativeRate := 0.0
	sessions := groupIntoSessions(input.Interactions, input.SessionGap)
	if len(sessions) > 0 {
		selfInitCount := 0
		for _, s := range sessions {
			if len(s) > 0 && s[0].SelfInitiated {
				selfInitCount++
			}
		}
		initiativeRate = float64(selfInitCount) / float64(len(sessions))
	}

	// 2. Calibration accuracy: 1 - |bias| clamped to [0, 1]
	calibrationAccuracy := 1.0 - math.Min(math.Abs(input.CalibrationBias), 1.0)

	// 3. Hint independence: 1 - (hints on mastered / total on mastered)
	hintIndependence := 1.0
	if len(input.ConceptStates) > 0 {
		masteryMid := algorithms.MasteryMid()
		masteredConcepts := make(map[string]bool)
		for _, cs := range input.ConceptStates {
			if cs.PMastery >= masteryMid {
				masteredConcepts[cs.Concept] = true
			}
		}
		if len(masteredConcepts) > 0 {
			totalOnMastered := 0
			hintsOnMastered := 0
			for _, i := range input.Interactions {
				if masteredConcepts[i.Concept] {
					totalOnMastered++
					hintsOnMastered += i.HintsRequested
				}
			}
			if totalOnMastered > 0 {
				hintRatio := float64(hintsOnMastered) / float64(totalOnMastered)
				hintIndependence = 1.0 - math.Min(hintRatio, 1.0)
			}
		}
	}

	// 4. Proactive review rate: % of review interactions that were proactive
	proactiveRate := 0.0
	reviewCount := 0
	proactiveCount := 0
	for _, i := range input.Interactions {
		if i.ActivityType != "NEW_CONCEPT" && i.ActivityType != "REST" && i.ActivityType != "SETUP_DOMAIN" {
			reviewCount++
			if i.IsProactiveReview {
				proactiveCount++
			}
		}
	}
	if reviewCount > 0 {
		proactiveRate = float64(proactiveCount) / float64(reviewCount)
	}

	score := (initiativeRate + calibrationAccuracy + hintIndependence + proactiveRate) / 4.0

	return models.AutonomyMetrics{
		Score:               score,
		InitiativeRate:      initiativeRate,
		CalibrationAccuracy: calibrationAccuracy,
		HintIndependence:    hintIndependence,
		ProactiveReviewRate: proactiveRate,
		ComputedAt:          now,
	}
}

// groupIntoSessions groups durable session IDs exactly. The time-gap heuristic
// is retained only for rows written before explicit sessions existed.
// Interactions are sorted oldest-first internally.
func groupIntoSessions(interactions []*models.Interaction, gap time.Duration) [][]*models.Interaction {
	if len(interactions) == 0 {
		return nil
	}

	sorted := make([]*models.Interaction, len(interactions))
	copy(sorted, interactions)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})

	var sessions [][]*models.Interaction
	var current []*models.Interaction

	for _, i := range sorted {
		if len(current) > 0 {
			previous := current[len(current)-1]
			boundary := false
			switch {
			case previous.SessionID != "" || i.SessionID != "":
				// Explicit and legacy events never merge. Two explicit events
				// share a session only when their durable IDs match, regardless
				// of elapsed wall time.
				boundary = previous.SessionID == "" || i.SessionID == "" || previous.SessionID != i.SessionID
			default:
				boundary = i.CreatedAt.Sub(previous.CreatedAt) > gap
			}
			if boundary {
				sessions = append(sessions, current)
				current = nil
			}
		}
		current = append(current, i)
	}
	if len(current) > 0 {
		sessions = append(sessions, current)
	}
	return sessions
}

// GroupIntoSessionsExported is the exported version of groupIntoSessions.
func GroupIntoSessionsExported(interactions []*models.Interaction, gap time.Duration) [][]*models.Interaction {
	return groupIntoSessions(interactions, gap)
}

// computeAutonomyTrend compares the last 5 scores to the 5 before that.
// scores are in newest-first order.
func computeAutonomyTrend(scores []float64) string {
	if len(scores) < 6 {
		return "stable"
	}

	recentN := 5
	if len(scores) < 10 {
		recentN = len(scores) / 2
	}

	recentSum := 0.0
	for i := 0; i < recentN; i++ {
		recentSum += scores[i]
	}
	recentAvg := recentSum / float64(recentN)

	previousSum := 0.0
	previousN := recentN
	if len(scores) < 2*recentN {
		previousN = len(scores) - recentN
	}
	for i := recentN; i < recentN+previousN; i++ {
		previousSum += scores[i]
	}
	previousAvg := previousSum / float64(previousN)

	diff := recentAvg - previousAvg
	if diff > 0.05 {
		return "improving"
	}
	if diff < -0.05 {
		return "declining"
	}
	return "stable"
}

// ComputeAutonomyTrendExported is the exported version of computeAutonomyTrend.
func ComputeAutonomyTrendExported(scores []float64) string {
	return computeAutonomyTrend(scores)
}

// ─── Metacognitive Mirror ───────────────────────────────────────────────────

// MirrorInput holds data for metacognitive mirror pattern detection.
type MirrorInput struct {
	Interactions       []*models.Interaction
	ConceptStates      []*models.ConceptState
	AutonomyScores     []float64 // newest-first from affect_states.autonomy_score
	CalibrationBias    float64
	CalibrationSamples int
	SessionCount       int
}

// DetectMirrorPattern returns a mirror message if a dependency pattern is consolidated
// over 3+ sessions. Returns nil if no pattern detected.
func DetectMirrorPattern(input MirrorInput) *models.MirrorMessage {
	if input.SessionCount < 3 {
		return nil
	}

	// Priority 1: dependency_increasing — autonomy declining over 3 consecutive sessions
	if len(input.AutonomyScores) >= 3 {
		declining := true
		for i := 0; i < len(input.AutonomyScores)-1 && i < 2; i++ {
			if input.AutonomyScores[i] >= input.AutonomyScores[i+1] {
				declining = false
				break
			}
		}
		if declining {
			return &models.MirrorMessage{
				Pattern:      "dependency_increasing",
				Message:      "Your autonomy score has dropped over the last 3 sessions.",
				OpenQuestion: "Do you feel you need more guidance right now, or would you like to try working more independently?",
			}
		}
	}

	// Priority 2: hint_overuse — hints on high-estimate concepts (PMastery >= MasteryMid)
	masteryMid := algorithms.MasteryMid()
	masteredConcepts := make(map[string]bool)
	for _, cs := range input.ConceptStates {
		if cs.PMastery >= masteryMid {
			masteredConcepts[cs.Concept] = true
		}
	}
	if len(masteredConcepts) > 0 {
		hintsOnMastered := 0
		totalOnMastered := 0
		for _, i := range input.Interactions {
			if masteredConcepts[i.Concept] {
				totalOnMastered++
				hintsOnMastered += i.HintsRequested
			}
		}
		if totalOnMastered >= 5 && float64(hintsOnMastered)/float64(totalOnMastered) > 0.5 {
			return &models.MirrorMessage{
				Pattern:      "hint_overuse",
				Message:      "You often ask for hints on concepts that your recent results make look familiar.",
				OpenQuestion: "Is it by reflex, or is there an aspect of these concepts that still feels unclear to you?",
			}
		}
	}

	// Priority 3: no_initiative — all sessions started after alert (no self_initiated)
	if input.SessionCount >= 3 {
		hasSelfInitiated := false
		for _, i := range input.Interactions {
			if i.SelfInitiated {
				hasSelfInitiated = true
				break
			}
		}
		if !hasSelfInitiated {
			return &models.MirrorMessage{
				Pattern:      "no_initiative",
				Message:      "All recent sessions were triggered by a notification.",
				OpenQuestion: "Do you prefer reminders from the system, or would you like to define your own learning moments?",
			}
		}
	}

	// Priority 4: calibration_drift. Calibration deltas are normalized to
	// [-1,1]; use the same evidence threshold as alerts and the OLM rather than
	// an unreachable >1.0 comparison.
	if calibrationBiasIsActionable(input.CalibrationBias, input.CalibrationSamples) {
		direction := "overestimate"
		if input.CalibrationBias < 0 {
			direction = "underestimate"
		}
		return &models.MirrorMessage{
			Pattern:      "calibration_drift",
			Message:      fmt.Sprintf("You tend to %s your level repeatedly.", direction),
			OpenQuestion: "Do you want to work with more frequent self-ratings to refine your perception?",
		}
	}

	return nil
}

// ─── Mirror webhook persistence ─────────────────────────────────────────────

// MirrorAlertKind is the daily reservation tag used by EnqueueMirrorWebhook.
// The scheduler still dispatches the claimed queue row carrying that
// reservation; it must not interpret reservation as successful delivery.
const MirrorAlertKind = "MIRROR_MESSAGE"

// mirrorWebhookStore is the narrow surface EnqueueMirrorWebhook needs from
// *db.Store. Keeping it as an interface lets the call sites (and tests) wire
// up a real or mock store without dragging the whole Store API into engine.
type mirrorWebhookStore interface {
	EnqueueWebhookMessageOncePerDay(
		ctx context.Context,
		learnerID, kind, alertType, content string,
		scheduledFor, expiresAt time.Time,
		priority int,
	) (int64, bool, error)
}

// MirrorWebhookContent is the JSON shape persisted into webhook_message_queue.content
// for a mirror nudge. Keeping the structured fields (pattern + open question)
// alongside the human-readable line lets the dispatcher and any downstream
// consumer reconstruct the full mirror without re-running detection.
type MirrorWebhookContent struct {
	Pattern      string `json:"pattern"`
	Message      string `json:"message"`
	OpenQuestion string `json:"open_question"`
}

// EnqueueMirrorWebhook persists an emitted mirror message into the shared
// webhook_message_queue so it can be pushed proactively by the scheduler.
//
// Behaviour:
//   - Returns (0, false, nil) on a no-op (mirror is nil, or one was already
//     reserved for this learner today).
//   - Queue insertion and the daily reservation are one database transaction,
//     so concurrent callers cannot create duplicate official mirror items.
//   - The message content is JSON-encoded so the scheduler can render the
//     full mirror (pattern + open question) when it dispatches.
//
// scheduledFor defaults to `now`; the message expires 24h later (mirrors are
// time-sensitive — a stale dependency-pattern nudge is noise, not signal).
func EnqueueMirrorWebhook(ctx context.Context, store mirrorWebhookStore, learnerID string, mirror *models.MirrorMessage, now time.Time) (int64, bool, error) {
	if store == nil || mirror == nil || learnerID == "" {
		return 0, false, nil
	}

	payload := MirrorWebhookContent{
		Pattern:      mirror.Pattern,
		Message:      mirror.Message,
		OpenQuestion: mirror.OpenQuestion,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, false, fmt.Errorf("marshal mirror payload: %w", err)
	}

	scheduledFor := now.UTC()
	expiresAt := scheduledFor.Add(24 * time.Hour)
	id, enqueued, err := store.EnqueueWebhookMessageOncePerDay(
		ctx,
		learnerID,
		models.WebhookKindMirror,
		MirrorAlertKind,
		string(body),
		scheduledFor,
		expiresAt,
		0, // priority: mirrors share the same lane as other proactive nudges
	)
	if err != nil {
		return 0, false, fmt.Errorf("enqueue mirror webhook: %w", err)
	}
	return id, enqueued, nil
}

// ─── Tutor Mode ─────────────────────────────────────────────────────────────

// ComputeTutorMode determines the tutor communication mode from affect and alerts.
func ComputeTutorMode(affect *models.AffectState, alerts []models.Alert) string {
	if affect == nil {
		return "normal"
	}

	hasAffectNegative := false
	for _, a := range alerts {
		if a.Type == models.AlertAffectNegative {
			hasAffectNegative = true
			break
		}
	}

	// Affect negative (frustration or boredom): low satisfaction → lighter.
	// (The previously distinct "recontextualize" branch for high-energy boredom
	// was removed in #60: it was purely cosmetic — the only side effect was
	// appending a label to Activity.Rationale, with no different prompt,
	// selector input, or persistence. Both sub-cases now map to "lighter".)
	if hasAffectNegative && affect.Satisfaction <= 2 {
		return "lighter"
	}

	// Start-of-session: anxious → scaffolding
	if affect.SubjectConfidence == 1 {
		return "scaffolding"
	}

	// Start-of-session: fatigued → lighter
	if affect.Energy == 1 {
		return "lighter"
	}

	return "normal"
}
