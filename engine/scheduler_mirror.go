// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"time"

	"tutor-mcp/models"
)

// sendMirrorMessages dispatches any pending metacognitive mirror nudges
// queued during sessions (engine.EnqueueMirrorWebhook). Mirror content is
// JSON (pattern + message + open question), decoded into a readable embed.
// Dedup at the alert layer (MirrorAlertKind) so an in-session enqueue won't
// race with the scheduler tick on the same day.
func (s *Scheduler) sendMirrorMessages() scheduledJobResult {
	now := time.Now().UTC()
	return s.processWebhookTargets(
		"mirror", "mirror_list_learners_failed", "mirror_partial_failure",
		func(ctx context.Context, target models.WebhookDispatchTarget) bool {
			return s.sendMirrorTarget(ctx, target, now)
		},
	)
}

func (s *Scheduler) sendMirrorTarget(ctx context.Context, target models.WebhookDispatchTarget, now time.Time) bool {
	return s.dispatchWebhookTarget(ctx, func(txCtx context.Context, child *Scheduler) (*webhookDispatch, bool) {
		return child.prepareMirrorTarget(txCtx, target, now)
	})
}

func (s *Scheduler) prepareMirrorTarget(ctx context.Context, target models.WebhookDispatchTarget, now time.Time) (*webhookDispatch, bool) {
	learner, avail := &models.Learner{ID: target.LearnerID}, target.Availability
	if learner.ID == "" || avail == nil {
		return nil, true
	}
	allowed, err := avail.AllowsNotificationAt(now)
	if err != nil {
		s.logger.Error("scheduler: mirror invalid notification policy", "err", err, "learner", learner.ID)
		return nil, true
	}
	if !allowed {
		return nil, false
	}
	highStakesAllowed, err := allHighStakesDomainsHumanReviewed(ctx, s.store, learner.ID)
	if err != nil {
		s.logger.Error("scheduler: mirror high-stakes review", "err", err, "learner", learner.ID)
		return nil, true
	}
	if !highStakesAllowed {
		return nil, false
	}

	item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, models.WebhookKindMirror, now, 30*time.Minute)
	if err != nil {
		s.logger.Error("scheduler: mirror claim", "err", err, "learner", learner.ID)
		return nil, true
	}
	if item == nil || item.Content == "" {
		return nil, false
	}

	embed := mirrorEmbedFromContent(item.Content)
	accessibility, err := avail.Accessibility()
	if err != nil {
		if releaseErr := s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID); releaseErr != nil {
			s.logger.Error("scheduler: mirror release invalid accessibility", "error_type", fmt.Sprintf("%T", releaseErr), "learner", learner.ID)
		}
		s.logger.Error("scheduler: mirror invalid accessibility policy", "err", err, "learner", learner.ID)
		return nil, true
	}
	payload := applyAccessibilityPreferences(discordPayload{Embeds: []discordEmbed{embed}}, accessibility)
	active, activeErr := s.store.IsWebhookClaimActive(ctx, item.ID, learner.ID)
	if activeErr != nil {
		if releaseErr := s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID); releaseErr != nil {
			s.logger.Error("scheduler: mirror release after revalidation error", "error_type", fmt.Sprintf("%T", releaseErr), "learner", learner.ID)
		}
		s.logger.Error("scheduler: mirror revalidate claim", "err", activeErr, "learner", learner.ID, "queue_id", item.ID)
		return nil, true
	}
	if !active {
		return nil, false
	}
	return &webhookDispatch{learnerID: learner.ID, alertType: MirrorAlertKind, intrusive: true,
		queueIDs: []int64{item.ID}, payload: payload, now: now}, false
}
