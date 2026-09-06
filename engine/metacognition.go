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
	"strings"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// AutonomyInput holds the data needed to compute autonomy metrics.
type AutonomyInput struct {
	Interactions      []*models.Interaction
	ConceptStates     []*models.ConceptState
	CalibrationDeltas []float64 // completed predictions minus outcomes, each in [-1,1]
	// Deprecated: signed mean bias cannot identify prediction accuracy.
	// Ignored; callers must provide CalibrationDeltas instead.
	CalibrationBias float64
	SessionGap      time.Duration // default: 2h
}

// ComputeAutonomyMetrics computes descriptive rates, not a validated autonomy
// scale. Score is a compatibility summary over the observed components only;
// ScoreStatus and the component sample counts expose missing evidence. Neither
// this summary nor changes in its coverage should determine pedagogical support.
func ComputeAutonomyMetrics(input AutonomyInput) models.AutonomyMetrics {
	now := time.Now().UTC()
	interactions := make([]*models.Interaction, 0, len(input.Interactions))
	for _, interaction := range input.Interactions {
		if interaction != nil {
			interactions = append(interactions, interaction)
		}
	}
	if input.SessionGap <= 0 {
		input.SessionGap = 2 * time.Hour
	}

	// 1. Initiative rate: % of sessions self-initiated
	initiativeRate := 0.0
	sessions := groupIntoSessions(interactions, input.SessionGap)
	if len(sessions) > 0 {
		selfInitCount := 0
		for _, s := range sessions {
			if len(s) > 0 && s[0].SelfInitiated {
				selfInitCount++
			}
		}
		initiativeRate = float64(selfInitCount) / float64(len(sessions))
	}

	// 2. Prediction accuracy: 1 - mean absolute prediction error. Taking the
	// absolute value after averaging would incorrectly make +1/-1 perfect.
	calibrationAccuracy := 0.0
	calibrationSamples := 0
	absErrorSum := 0.0
	for _, delta := range input.CalibrationDeltas {
		if math.IsNaN(delta) || math.IsInf(delta, 0) {
			continue
		}
		absErrorSum += math.Min(math.Abs(delta), 1)
		calibrationSamples++
	}
	if calibrationSamples > 0 {
		calibrationAccuracy = 1 - absErrorSum/float64(calibrationSamples)
	}

	// 3. Share of observed responses without hints on high-estimate concepts.
	// Absence of observations is unknown, not perfect independence. Count
	// assisted responses, not individual hints, so one response cannot count
	// against several independent responses.
	hintIndependence := 0.0
	hintObservations := 0
	if len(input.ConceptStates) > 0 {
		masteryMid := algorithms.MasteryMid()
		type conceptKey struct{ domainID, concept string }
		masteredConcepts := make(map[conceptKey]bool)
		for _, cs := range input.ConceptStates {
			if cs != nil && cs.PMastery >= masteryMid {
				masteredConcepts[conceptKey{cs.DomainID, cs.Concept}] = true
			}
		}
		if len(masteredConcepts) > 0 {
			totalOnMastered := 0
			hintsOnMastered := 0
			for _, i := range interactions {
				if masteredConcepts[conceptKey{i.DomainID, i.Concept}] {
					totalOnMastered++
					if i.HintsRequested > 0 {
						hintsOnMastered++
					}
				}
			}
			if totalOnMastered > 0 {
				hintObservations = totalOnMastered
				hintRatio := float64(hintsOnMastered) / float64(totalOnMastered)
				hintIndependence = 1.0 - math.Min(hintRatio, 1.0)
			}
		}
	}

	// 4. Proactive review rate: % of review interactions that were proactive
	proactiveRate := 0.0
	reviewCount := 0
	proactiveCount := 0
	for _, i := range interactions {
		if i.ActivityType == string(models.ActivityRecall) {
			reviewCount++
			if i.IsProactiveReview {
				proactiveCount++
			}
		}
	}
	if reviewCount > 0 {
		proactiveRate = float64(proactiveCount) / float64(reviewCount)
	}

	score := 0.0
	observedComponents := 0
	for _, component := range []struct {
		value float64
		count int
	}{
		{initiativeRate, len(sessions)},
		{calibrationAccuracy, calibrationSamples},
		{hintIndependence, hintObservations},
		{proactiveRate, reviewCount},
	} {
		if component.count > 0 {
			score += component.value
			observedComponents++
		}
	}
	scoreStatus := "unavailable"
	if observedComponents > 0 {
		score /= float64(observedComponents)
		scoreStatus = "partial"
		if observedComponents == 4 {
			scoreStatus = "descriptive"
		}
	}

	return models.AutonomyMetrics{
		Score:               score,
		ScoreStatus:         scoreStatus,
		ObservedComponents:  observedComponents,
		SessionCount:        len(sessions),
		InitiativeRate:      initiativeRate,
		CalibrationAccuracy: calibrationAccuracy,
		CalibrationSamples:  calibrationSamples,
		HintIndependence:    hintIndependence,
		HintObservations:    hintObservations,
		ProactiveReviewRate: proactiveRate,
		ReviewObservations:  reviewCount,
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
	AutonomyScores     []float64 // legacy input; composite scores no longer establish dependency
	CalibrationBias    float64
	CalibrationSamples int
	SessionCount       int
}

// DetectMirrorPattern returns descriptive observations for a generative dialogue
// after at least three sessions. It does not diagnose dependency, assume why a
// session started, or author the learner-facing message.
func DetectMirrorPattern(input MirrorInput) *models.MirrorMessage {
	if input.SessionCount < 3 {
		return nil
	}

	interactionCount := 0
	for _, interaction := range input.Interactions {
		if interaction != nil {
			interactionCount++
		}
	}
	observation := func(pattern, intent string, facts map[string]any) *models.MirrorMessage {
		return &models.MirrorMessage{
			Pattern: pattern,
			Facts:   facts,
			Window:  &models.MirrorWindow{SessionCount: input.SessionCount, InteractionCount: interactionCount},
			// This is an evidence description, not a calibrated probability
			// or a claim about the learner's psychological state.
			Confidence:     "descriptive_only",
			DialogueIntent: intent,
		}
	}

	// Hint observations use current model estimates to describe the selected
	// cohort. They do not imply the concept was mastered when a hint was used,
	// or that requesting assistance was inappropriate.
	masteryMid := algorithms.MasteryMid()
	masteredConcepts := make(map[string]bool)
	for _, cs := range input.ConceptStates {
		if cs != nil && cs.PMastery >= masteryMid {
			masteredConcepts[cs.Concept] = true
		}
	}
	if len(masteredConcepts) > 0 {
		hintsOnMastered := 0
		totalOnMastered := 0
		for _, i := range input.Interactions {
			if i != nil && masteredConcepts[i.Concept] {
				totalOnMastered++
				hintsOnMastered += i.HintsRequested
			}
		}
		if totalOnMastered >= 5 && float64(hintsOnMastered)/float64(totalOnMastered) > 0.5 {
			return observation("hint_use_observed", "explore_help_use_and_remaining_questions", map[string]any{
				"hints_requested": hintsOnMastered,
				"interactions_on_currently_high_estimate_concepts": totalOnMastered,
				"estimate_threshold": masteryMid,
				"estimate_scope":     "current_state_not_state_when_hint_was_requested",
			})
		}
	}

	// Missing initiative flags do not establish that notifications caused
	// sessions. Surface the recorded absence and let the tutor clarify it.
	if interactionCount > 0 {
		hasSelfInitiated := false
		for _, i := range input.Interactions {
			if i != nil && i.SelfInitiated {
				hasSelfInitiated = true
				break
			}
		}
		if !hasSelfInitiated {
			return observation("no_recorded_initiative", "clarify_session_initiation_preference", map[string]any{
				"self_initiated_interactions":  0,
				"notification_origin_verified": false,
			})
		}
	}

	// Calibration deltas are normalized to
	// [-1,1]; use the same evidence threshold as alerts and the OLM rather than
	// an unreachable >1.0 comparison.
	if calibrationBiasIsActionable(input.CalibrationBias, input.CalibrationSamples) {
		direction := "predictions_above_outcomes"
		if input.CalibrationBias < 0 {
			direction = "predictions_below_outcomes"
		}
		return observation("calibration_drift", "compare_predictions_with_observed_outcomes", map[string]any{
			"mean_signed_error":     input.CalibrationBias,
			"completed_predictions": input.CalibrationSamples,
			"direction":             direction,
		})
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

// EnqueueMirrorWebhook preserves delivery of legacy authored mirror messages.
// New observation-only mirrors need tutor-generated wording and are never
// automatically put on the delivery queue. The tutor can explicitly queue its
// generated message using the normal consent-aware webhook workflow.
//
// Behaviour:
//   - Returns (0, false, nil) on a no-op (no authored message, or one was already
//     reserved for this learner today).
//   - Queue insertion and the daily reservation are one database transaction,
//     so concurrent callers cannot create duplicate official mirror items.
//   - The message content is JSON-encoded so the scheduler can render the
//     full mirror (pattern + open question) when it dispatches.
//
// scheduledFor defaults to `now`; the message expires 24h later (mirrors are
// time-sensitive — a stale dependency-pattern nudge is noise, not signal).
func EnqueueMirrorWebhook(ctx context.Context, store mirrorWebhookStore, learnerID string, mirror *models.MirrorMessage, now time.Time) (int64, bool, error) {
	if store == nil || mirror == nil || learnerID == "" || strings.TrimSpace(mirror.Message) == "" {
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
