// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"time"

	"tutor-mcp/models"
)

// metacogKindToWebhookKind maps an AlertType to the webhook_message_queue
// `kind` slot under which the dispatch enqueues a nudge. The planner now
// chooses the highest-value candidate per learner/tick; the kind remains the
// daily dedup tag for the selected signal.
func metacogKindToWebhookKind(t models.AlertType) string {
	switch t {
	case models.AlertDependencyIncreasing:
		return "metacog_dependency"
	case models.AlertCalibrationDiverging:
		return "metacog_calibration"
	case models.AlertAffectNegative:
		return "metacog_affect"
	case models.AlertTransferBlocked:
		return "metacog_transfer"
	}
	return ""
}

// dispatchMetacognitiveAlerts iterates over every active learner, computes
// metacognitive alerts (DEPENDENCY / CALIBRATION / AFFECT / TRANSFER) from
// their current state, turns them into structured learner-facing candidates,
// and pushes at most the best candidate for the learner on this tick.
// Per-kind daily dedup is enforced by dispatchQueued via scheduled_alerts
// (WasAlertSentToday + CreateScheduledAlert) — the cron cadence only affects
// detection latency, not delivery frequency.
//
// Two-stage flow on each tick:
//
//  1. Compute alerts → build ranked structured candidates.
//  2. Enqueue the first candidate whose kind has not fired today.
//  3. Drain each enqueued kind via dispatchQueued, which posts the embed
//     and stamps scheduled_alerts so the next tick deduplicates.
//
// TRANSFER_BLOCKED is per-concept in the alert payload but is collapsed to
// a single per-day kind here — one nudge per day is the product contract,
// and the embed mentions the most-recent concept that fired.
func (s *Scheduler) dispatchMetacognitiveAlerts() {
	if s.store == nil {
		return
	}
	ctx := context.Background()
	learners, err := s.store.GetActiveLearners(ctx)
	if err != nil {
		s.logger.Error("scheduler: metacog get learners", "err", err)
		return
	}
	now := time.Now().UTC()

	// Track which kinds were enqueued this tick so we know what to drain.
	enqueuedKinds := make(map[string]bool)

	for _, learner := range learners {
		if learner.WebhookURL == "" {
			continue
		}
		avail, _ := s.store.GetAvailability(ctx, learner.ID)
		if avail != nil && avail.DoNotDisturb {
			continue
		}

		// Gather inputs. Errors on any one source are tolerated — the
		// missing input simply skips its branch in
		// ComputeMetacognitiveAlerts (no alert is better than panicking
		// the cron tick on a single learner with corrupt rows).
		states, _ := s.store.GetConceptStatesByLearner(ctx, learner.ID)
		interactions, _ := s.store.GetRecentInteractionsByLearner(ctx, learner.ID, 20)
		affects, _ := s.store.GetRecentAffectStates(ctx, learner.ID, 10)
		var autonomyScores []float64
		for _, a := range affects {
			autonomyScores = append(autonomyScores, a.AutonomyScore)
		}
		calibBias, _ := s.store.GetCalibrationBias(ctx, learner.ID, 20)
		transfers, _ := s.store.GetTransferRecordsByLearner(ctx, learner.ID)

		alerts := ComputeMetacognitiveAlerts(
			autonomyScores,
			calibBias,
			affects,
			interactions,
			WithTransferData(states, transfers),
		)

		domains, _ := s.store.GetDomainsByLearner(ctx, learner.ID, false)
		candidates := BuildMetacognitiveNudgeCandidates(learner, domains, alerts)
		for _, candidate := range candidates {
			// One metacognitive push per tick is intentional: Discord should
			// surface the highest-learning-value next action, not a bundle of
			// weak observations.
			alreadySent, _ := s.store.WasAlertSentToday(ctx, learner.ID, candidate.AlertTag)
			if alreadySent {
				continue
			}
			content, err := models.EncodeWebhookBrief(candidate.Brief)
			if err != nil {
				s.logger.Error("scheduler: metacog brief encode",
					"err", err, "learner", learner.ID, "kind", candidate.Kind)
				continue
			}
			if _, err := s.store.EnqueueWebhookMessage(
				ctx, learner.ID, candidate.Kind, content, now, now.Add(2*time.Hour), candidate.Priority,
			); err != nil {
				s.logger.Error("scheduler: metacog enqueue",
					"err", err, "learner", learner.ID, "kind", candidate.Kind)
				continue
			}
			enqueuedKinds[candidate.Kind] = true
			s.logger.Info("scheduler: metacog enqueued",
				"learner", learner.ID, "kind", candidate.Kind, "priority", candidate.Priority)
			break
		}
	}

	// Drain every kind we enqueued so the webhook actually leaves the
	// process this tick. dispatchQueued handles the WasAlertSentToday
	// dedup + CreateScheduledAlert stamping so the next tick is a no-op.
	for kind := range enqueuedKinds {
		s.dispatchQueued(kind, kind, nil)
	}
}
