// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"sync"
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
func (s *Scheduler) dispatchMetacognitiveAlerts() scheduledJobResult {
	if s.store == nil {
		return scheduledJobSucceeded()
	}
	now := time.Now().UTC()
	var enqueuedKinds sync.Map
	result := s.processWebhookTargets(
		"metacog", "metacog_list_learners_failed", "metacog_partial_failure",
		func(ctx context.Context, target models.WebhookDispatchTarget) bool {
			return s.dispatchMetacognitiveTarget(ctx, target, now, &enqueuedKinds)
		},
	)

	// Drain every bounded kind enqueued by the workers. dispatchQueued uses the
	// same paginated pool and durable daily reservation, so concurrent candidates
	// converge without an unbounded learner list or duplicate delivery.
	enqueuedKinds.Range(func(key, _ any) bool {
		kind, ok := key.(string)
		if !ok {
			result.recordFailure("metacog_dispatch_failed")
			return true
		}
		if dispatchResult := s.dispatchQueued(kind, kind, nil); dispatchResult.failed() {
			result.recordFailure("metacog_dispatch_failed")
		}
		return true
	})
	return result
}

func (s *Scheduler) dispatchMetacognitiveTarget(
	ctx context.Context,
	target models.WebhookDispatchTarget,
	now time.Time,
	enqueuedKinds *sync.Map,
) bool {
	learner, avail := &models.Learner{ID: target.LearnerID}, target.Availability
	if learner.ID == "" || avail == nil {
		return true
	}
	failed := false
	allowed, err := avail.AllowsNotificationAt(now)
	if err != nil {
		s.logger.Error("scheduler: metacog invalid notification policy", "err", err, "learner", learner.ID)
		return true
	}
	if !allowed {
		return false
	}

	// A partial input set can suppress a real alert. Skip this learner for
	// the tick and log the failed source instead of producing a misleading
	// "all clear" decision.
	states, err := s.store.GetConceptStatesByLearner(ctx, learner.ID)
	if err != nil {
		s.logger.Error("scheduler: metacog states", "err", err, "learner", learner.ID)
		return true
	}
	interactions, err := s.store.GetRecentInteractionsByLearner(ctx, learner.ID, 20)
	if err != nil {
		s.logger.Error("scheduler: metacog interactions", "err", err, "learner", learner.ID)
		return true
	}
	affects, err := s.store.GetRecentAffectStates(ctx, learner.ID, 10)
	if err != nil {
		s.logger.Error("scheduler: metacog affects", "err", err, "learner", learner.ID)
		return true
	}
	var autonomyScores []float64
	for _, a := range affects {
		autonomyScores = append(autonomyScores, a.AutonomyScore)
	}
	calibBias, err := s.store.GetCalibrationBias(ctx, learner.ID, 20)
	if err != nil {
		s.logger.Error("scheduler: metacog calibration", "err", err, "learner", learner.ID)
		return true
	}
	calibHistory, err := s.store.GetCalibrationBiasHistory(ctx, learner.ID, 20)
	if err != nil {
		s.logger.Error("scheduler: metacog calibration evidence", "err", err, "learner", learner.ID)
		return true
	}
	transfers, err := s.store.GetTransferRecordsByLearner(ctx, learner.ID)
	if err != nil {
		s.logger.Error("scheduler: metacog transfers", "err", err, "learner", learner.ID)
		return true
	}

	alerts := ComputeMetacognitiveAlerts(
		autonomyScores,
		calibBias,
		affects,
		interactions,
		WithTransferData(states, transfers),
		WithCalibrationEvidence(len(calibHistory)),
	)

	domains, err := s.store.GetDomainsByLearner(ctx, learner.ID, false)
	if err != nil {
		s.logger.Error("scheduler: metacog domains", "err", err, "learner", learner.ID)
		return true
	}
	candidates := BuildMetacognitiveNudgeCandidates(learner, domains, alerts)
	for _, candidate := range candidates {
		highStakesAllowed, highStakesErr := metacognitiveHighStakesAllowed(ctx, s, learner.ID, domains, candidate.Brief.DomainID)
		if highStakesErr != nil {
			failed = true
			continue
		}
		if !highStakesAllowed {
			continue
		}
		// One metacognitive push per tick is intentional: Discord should
		// surface the highest-learning-value next action, not a bundle of
		// weak observations.
		alreadySent, err := s.store.WasAlertSentToday(ctx, learner.ID, candidate.AlertTag)
		if err != nil {
			failed = true
			s.logger.Error("scheduler: metacog dedup", "err", err, "learner", learner.ID, "kind", candidate.Kind)
			continue
		}
		if alreadySent {
			continue
		}
		content, err := models.EncodeWebhookBrief(candidate.Brief)
		if err != nil {
			failed = true
			s.logger.Error("scheduler: metacog brief encode",
				"err", err, "learner", learner.ID, "kind", candidate.Kind)
			continue
		}
		if _, err := s.store.EnqueueWebhookMessage(
			ctx, learner.ID, candidate.Kind, content, now, now.Add(2*time.Hour), candidate.Priority,
		); err != nil {
			failed = true
			s.logger.Error("scheduler: metacog enqueue",
				"err", err, "learner", learner.ID, "kind", candidate.Kind)
			continue
		}
		enqueuedKinds.Store(candidate.Kind, true)
		s.logger.Info("scheduler: metacog enqueued",
			"learner", learner.ID, "kind", candidate.Kind, "priority", candidate.Priority)
		break
	}
	return failed
}

func metacognitiveHighStakesAllowed(ctx context.Context, scheduler *Scheduler, learnerID string, domains []*models.Domain, domainID string) (bool, error) {
	if domainID == "" {
		allowed, err := allHighStakesDomainsHumanReviewed(ctx, scheduler.store, learnerID)
		if err != nil {
			scheduler.logger.Error("scheduler: metacog high-stakes review", "err", err, "learner", learnerID)
			return false, err
		}
		return allowed, nil
	}
	for _, domain := range domains {
		if domain == nil || domain.ID != domainID {
			continue
		}
		if !domain.HighStakes {
			return true, nil
		}
		reviewed, err := scheduler.store.HasHumanReviewedEvaluationInDomain(ctx, learnerID, domainID)
		if err != nil {
			scheduler.logger.Error("scheduler: metacog high-stakes review", "err", err, "learner", learnerID, "domain", domainID)
			return false, err
		}
		return reviewed, nil
	}
	return false, nil
}
