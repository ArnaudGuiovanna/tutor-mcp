// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
)

func (s *Scheduler) sendDailyMotivation() scheduledJobResult {
	return s.dispatchQueued("daily_motivation", "DAILY_MOTIVATION", fallbackDailyMotivation)
}

func (s *Scheduler) sendDailyRecap() scheduledJobResult {
	return s.dispatchQueued("daily_recap", "DAILY_RECAP", fallbackDailyRecap)
}

// dispatchQueued pulls the best pending webhook message for each active learner
// (kind + ±30min scheduling window) and posts it. Falls back to a sober template
// when the queue is empty.
func (s *Scheduler) dispatchQueued(kind, alertTag string, fallback func(*models.Learner) discordPayload) scheduledJobResult {
	now := time.Now().UTC()
	return s.processWebhookTargets(
		"dispatch_"+kind, "dispatch_list_learners_failed", "dispatch_partial_failure",
		func(ctx context.Context, target models.WebhookDispatchTarget) bool {
			return s.dispatchQueuedTarget(ctx, target, kind, alertTag, fallback, now)
		},
	)
}

func (s *Scheduler) dispatchQueuedTarget(
	ctx context.Context,
	target models.WebhookDispatchTarget,
	kind, alertTag string,
	fallback func(*models.Learner) discordPayload,
	now time.Time,
) bool {
	return s.dispatchWebhookTarget(ctx, func(txCtx context.Context, child *Scheduler) (*webhookDispatch, bool) {
		return child.prepareQueuedTarget(txCtx, target, kind, alertTag, fallback, now)
	})
}

func (s *Scheduler) prepareQueuedTarget(
	ctx context.Context,
	target models.WebhookDispatchTarget,
	kind, alertTag string,
	fallback func(*models.Learner) discordPayload,
	now time.Time,
) (*webhookDispatch, bool) {
	learner, avail := &models.Learner{ID: target.LearnerID}, target.Availability
	if learner.ID == "" || avail == nil {
		return nil, true
	}
	allowed, err := avail.AllowsNotificationAt(now)
	if err != nil {
		s.logger.Error("scheduler: invalid notification policy", "err", err, "learner", learner.ID, "kind", kind)
		return nil, true
	}
	if !allowed {
		return nil, false
	}

	item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, kind, now, 30*time.Minute)
	if err != nil {
		s.logger.Error("scheduler: claim webhook", "err", err, "learner", learner.ID, "kind", kind)
		return nil, true
	}

	var payload discordPayload
	var briefForLog *models.WebhookBrief
	domainID := ""
	payloadAlreadyAccessible := false
	if item != nil && item.Content != "" {
		if item.ContentFormat == models.WebhookContentFormatDiscordPayload {
			if err := json.Unmarshal([]byte(item.Content), &payload); err != nil || len(payload.Embeds) == 0 {
				if _, recordErr := s.store.RecordWebhookFailure(ctx, item.ID, learner.ID, "payload_invalid", now); recordErr != nil {
					s.logger.Error("scheduler: persist invalid fallback payload", "error_type", fmt.Sprintf("%T", recordErr), "learner", learner.ID, "queue_id", item.ID)
				}
				s.logger.Error("scheduler: invalid durable fallback payload", "learner", learner.ID, "queue_id", item.ID)
				return nil, true
			}
			domainID = item.DomainID
			payloadAlreadyAccessible = true
		} else {
			embed, brief := embedFromWebhookContent(kind, item.Content, models.UrgencyInfo)
			payload = discordPayload{Embeds: []discordEmbed{embed}}
			briefForLog = brief
			if brief != nil {
				domainID = brief.DomainID
			}
		}
		if domainID == "" && strings.HasPrefix(kind, "olm:") {
			domainID = strings.TrimPrefix(kind, "olm:")
		}
	} else if fallback != nil {
		live, liveErr := s.store.HasLiveWebhookMessageKind(ctx, learner.ID, kind, now)
		if liveErr != nil {
			s.logger.Error("scheduler: check live webhook before fallback", "error_type", fmt.Sprintf("%T", liveErr), "learner", learner.ID, "kind", kind)
			return nil, true
		}
		if live {
			return nil, false
		}
		payload = fallback(learner)
	} else {
		return nil, false
	}

	accessibility, err := avail.Accessibility()
	if err != nil {
		if item != nil {
			if releaseErr := s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID); releaseErr != nil {
				s.logger.Error("scheduler: release webhook after invalid accessibility", "error_type", fmt.Sprintf("%T", releaseErr), "learner", learner.ID)
			}
		}
		s.logger.Error("scheduler: invalid accessibility policy", "err", err, "learner", learner.ID)
		return nil, true
	}
	if !payloadAlreadyAccessible {
		payload = applyAccessibilityPreferences(payload, accessibility)
	}
	if item == nil {
		encoded, encodeErr := json.Marshal(payload)
		if encodeErr != nil {
			s.logger.Error("scheduler: encode fallback payload", "error_type", fmt.Sprintf("%T", encodeErr), "learner", learner.ID, "kind", kind)
			return nil, true
		}
		var enqueued bool
		item, enqueued, err = s.store.EnqueueClaimedWebhookPayloadOncePerDay(
			ctx, learner.ID, kind, alertTag, domainID, string(encoded),
			models.IsIntrusiveWebhookKind(kind), now, now.Add(12*time.Hour), 0,
		)
		if err != nil {
			s.logger.Error("scheduler: enqueue durable fallback", "error_type", fmt.Sprintf("%T", err), "learner", learner.ID, "kind", kind)
			return nil, true
		}
		if !enqueued || item == nil {
			return nil, false
		}
	}
	delivery := &webhookDispatch{learnerID: learner.ID, alertType: alertTag, domainID: domainID,
		intrusive: models.IsIntrusiveWebhookKind(kind), queueIDs: []int64{item.ID}, payload: payload, now: now}
	if briefForLog != nil {
		delivery.pushes = append(delivery.pushes, webhookPushRecord{queueID: item.ID, brief: briefForLog})
	}
	return delivery, false
}

func fallbackDailyMotivation(_ *models.Learner) discordPayload {
	return discordPayload{Embeds: []discordEmbed{{
		Title:       "☀️ Good morning",
		Description: "Even 5 minutes today keeps the trajectory. Come back whenever you want.",
		Color:       0x5865F2,
	}}}
}

func fallbackDailyRecap(_ *models.Learner) discordPayload {
	return discordPayload{Embeds: []discordEmbed{{
		Title:       "🌙 Tonight",
		Description: "If you stop by, we keep going. Otherwise, see you tomorrow.",
		Color:       0x57F287,
	}}}
}
