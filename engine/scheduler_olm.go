// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// sendOLM dispatches the daily Open Learner Model webhook at 13h UTC for each
// active learner with at least one actionable domain. Per-domain dispatch
// (cap 3 embeds) lets a learner with multiple domains see all relevant states
// in one Discord message. Falls back to BuildOLMNudgeBrief when no LLM-authored
// message is queued.
func (s *Scheduler) sendOLM() scheduledJobResult {
	now := time.Now().UTC()
	return s.processWebhookTargets(
		"olm", "olm_list_learners_failed", "olm_partial_failure",
		func(ctx context.Context, target models.WebhookDispatchTarget) bool {
			return s.sendOLMTarget(ctx, target, now)
		},
	)
}

func (s *Scheduler) sendOLMTarget(ctx context.Context, target models.WebhookDispatchTarget, now time.Time) bool {
	return s.dispatchWebhookTarget(ctx, func(txCtx context.Context, child *Scheduler) (*webhookDispatch, bool) {
		return child.prepareOLMTarget(txCtx, target, now)
	})
}

func (s *Scheduler) prepareOLMTarget(ctx context.Context, target models.WebhookDispatchTarget, now time.Time) (*webhookDispatch, bool) {
	learner, avail := &models.Learner{ID: target.LearnerID}, target.Availability
	if learner.ID == "" || avail == nil {
		return nil, true
	}
	failed := false
	allowed, err := avail.AllowsNotificationAt(now)
	if err != nil {
		s.logger.Error("scheduler: olm invalid notification policy", "err", err, "learner", learner.ID)
		return nil, true
	}
	if !allowed {
		return nil, false
	}

	domains, err := s.store.GetDomainsByLearner(ctx, learner.ID, false /*includeArchived*/)
	if err != nil {
		s.logger.Error("scheduler: olm list domains", "err", err, "learner", learner.ID)
		return nil, true
	}

	var prepared []preparedWebhookEmbed
	for _, d := range domains {
		if len(prepared) >= 3 {
			break
		}
		if d.HighStakes {
			reviewed, err := s.store.HasHumanReviewedEvaluationInDomain(ctx, learner.ID, d.ID)
			if err != nil {
				failed = true
				s.logger.Error("scheduler: olm high-stakes review", "err", err, "learner", learner.ID, "domain", d.ID)
				continue
			}
			if !reviewed {
				continue
			}
		}
		snap, err := BuildOLMSnapshotAt(ctx, s.store, learner.ID, d.ID, now)
		if err != nil {
			failed = true
			s.logger.Warn("scheduler: olm build", "err", err, "learner", learner.ID, "domain", d.ID)
			continue
		}
		if !snap.HasActionable {
			continue
		}
		var memCtx *memory.EpisodicContext
		if memory.Enabled() && snap.FocusReason == "next frontier" {
			if ctx, err := s.loadOLMNarrativeContext(ctx, learner.ID, d.ID, snap.FocusConcept); err == nil {
				memCtx = ctx
			} else {
				failed = true
				s.logger.Warn("scheduler: olm memory context", "err", err, "learner", learner.ID, "domain", d.ID)
			}
		}
		if !shouldPushDiscord(snap, memCtx) {
			continue
		}

		kind := "olm:" + d.ID
		item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, kind, now, 30*time.Minute)
		if err != nil {
			failed = true
			s.logger.Error("scheduler: olm claim", "err", err, "learner", learner.ID, "domain", d.ID)
			continue
		}
		if item != nil && item.Content != "" {
			if item.ContentFormat == models.WebhookContentFormatDiscordPayload {
				var stored discordPayload
				if err := json.Unmarshal([]byte(item.Content), &stored); err != nil || len(stored.Embeds) == 0 {
					failed = true
					if _, recordErr := s.store.RecordWebhookFailure(ctx, item.ID, learner.ID, "payload_invalid", now); recordErr != nil {
						s.logger.Error("scheduler: persist invalid olm payload", "error_type", fmt.Sprintf("%T", recordErr), "learner", learner.ID, "queue_id", item.ID)
					}
					s.logger.Error("scheduler: invalid durable olm fallback", "learner", learner.ID, "queue_id", item.ID)
					continue
				}
				for index, embed := range stored.Embeds {
					entry := preparedWebhookEmbed{DomainID: item.DomainID, Embed: embed}
					if index == 0 {
						entry.Item = item
					}
					prepared = append(prepared, entry)
				}
			} else {
				embed, brief := embedFromWebhookContent(kind, item.Content, snap.FocusUrgency)
				prepared = append(prepared, preparedWebhookEmbed{DomainID: d.ID, Embed: embed, Item: item, Brief: brief})
			}
		} else {
			brief := BuildOLMNudgeBrief(snap)
			prepared = append(prepared, preparedWebhookEmbed{
				DomainID: d.ID, Embed: discordEmbedFromBrief(brief, snap.FocusUrgency),
				Brief: &brief, NeedsPersistence: true,
			})
		}
	}

	if len(prepared) == 0 {
		return nil, failed
	}
	embeds := make([]discordEmbed, 0, len(prepared))
	for _, p := range prepared {
		embeds = append(embeds, p.Embed)
	}
	accessibility, err := avail.Accessibility()
	if err != nil {
		failed = true
		for _, p := range prepared {
			if p.Item != nil {
				_ = s.store.ReleaseWebhookClaim(ctx, p.Item.ID, learner.ID)
			}
		}
		s.logger.Error("scheduler: olm invalid accessibility policy", "err", err, "learner", learner.ID)
		return nil, true
	}
	payload := applyAccessibilityPreferences(discordPayload{Embeds: embeds}, accessibility)
	claimsActive := true
	queueIDs := make([]int64, 0, len(prepared))
	fallbackPayload := discordPayload{}
	fallbackDomainID := ""
	for index, p := range prepared {
		domain, domainErr := s.store.GetDomainByID(ctx, p.DomainID)
		if domainErr != nil || domain.LearnerID != learner.ID || domain.Archived {
			claimsActive = false
			if domainErr != nil {
				failed = true
				s.logger.Warn("scheduler: olm domain became inactive", "error_type", fmt.Sprintf("%T", domainErr), "learner", learner.ID, "domain", p.DomainID)
			}
		}
		if p.Item == nil {
			if p.NeedsPersistence {
				fallbackPayload.Embeds = append(fallbackPayload.Embeds, payload.Embeds[index])
				if fallbackDomainID == "" {
					fallbackDomainID = p.DomainID
				}
			}
			continue
		}
		queueIDs = append(queueIDs, p.Item.ID)
		active, activeErr := s.store.IsWebhookClaimActive(ctx, p.Item.ID, learner.ID)
		if activeErr != nil || !active {
			claimsActive = false
			if activeErr != nil {
				failed = true
				s.logger.Error("scheduler: olm revalidate claim", "err", activeErr, "learner", learner.ID, "queue_id", p.Item.ID)
			}
		}
	}
	if !claimsActive {
		s.releaseWebhookClaims(ctx, learner.ID, queueIDs)
		return nil, failed
	}

	fallbackQueueID := int64(0)
	if len(fallbackPayload.Embeds) > 0 {
		encoded, encodeErr := json.Marshal(fallbackPayload)
		if encodeErr != nil {
			failed = true
			s.releaseWebhookClaims(ctx, learner.ID, queueIDs)
			s.logger.Error("scheduler: encode olm fallback", "error_type", fmt.Sprintf("%T", encodeErr), "learner", learner.ID)
			return nil, true
		}
		fallbackItem, enqueued, enqueueErr := s.store.EnqueueClaimedWebhookPayloadOncePerDay(
			ctx, learner.ID, "olm:"+fallbackDomainID, alertKindOLM, fallbackDomainID,
			string(encoded), true, now, now.Add(24*time.Hour), 0,
		)
		if enqueueErr != nil || !enqueued || fallbackItem == nil {
			s.releaseWebhookClaims(ctx, learner.ID, queueIDs)
			if enqueueErr != nil {
				failed = true
				s.logger.Error("scheduler: enqueue durable olm fallback", "error_type", fmt.Sprintf("%T", enqueueErr), "learner", learner.ID)
			}
			return nil, failed
		}
		fallbackQueueID = fallbackItem.ID
		queueIDs = append(queueIDs, fallbackItem.ID)
	}
	delivery := &webhookDispatch{learnerID: learner.ID, alertType: alertKindOLM, domainID: prepared[0].DomainID,
		intrusive: true, queueIDs: queueIDs, payload: payload, now: now}
	for _, p := range prepared {
		queueID := int64(0)
		if p.Item != nil {
			queueID = p.Item.ID
		} else if p.NeedsPersistence {
			queueID = fallbackQueueID
		}
		if p.Brief != nil {
			delivery.pushes = append(delivery.pushes, webhookPushRecord{queueID: queueID, brief: p.Brief})
		}
	}
	return delivery, failed
}

func (s *Scheduler) loadOLMNarrativeContext(ctx context.Context, learnerID, domainID, concept string) (*memory.EpisodicContext, error) {
	load := func(loadCtx context.Context) (*memory.EpisodicContext, error) {
		return memory.LoadContextForDomainContext(loadCtx, learnerID, domainID, concept, &memory.OLMView{FocusConcept: concept}, nil)
	}
	_, alreadyScoped := memory.NarrativeTenantScope(ctx)
	if alreadyScoped || !s.legacyNarrativeScope || !memory.UsingSharedNarrativeStore() {
		return load(ctx)
	}
	// Only the legacy constructors may supply this known tenant identity.
	// Wrap the complete loader so root-backend calls retain RLS on PostgreSQL.
	var result *memory.EpisodicContext
	err := s.store.WithTenantTx(ctx, models.LegacyPrincipal(learnerID).TenantScope(), func(txCtx context.Context, _ storeport.Store) error {
		var err error
		result, err = load(txCtx)
		return err
	})
	return result, err
}

type preparedWebhookEmbed struct {
	DomainID         string
	Embed            discordEmbed
	Item             *models.WebhookQueueItem
	Brief            *models.WebhookBrief
	NeedsPersistence bool
}

func shouldPushDiscord(olm *OLMSnapshot, memCtx *memory.EpisodicContext) bool {
	if olm == nil || !olm.HasActionable {
		return false
	}
	if !memory.Enabled() {
		return true
	}
	if olm.FocusReason != "next frontier" {
		return true
	}
	return memCtx != nil && memCtx.HasRecentNarrativeSignal()
}

func allHighStakesDomainsHumanReviewed(ctx context.Context, persistence interface {
	storeport.DomainStore
	storeport.AssessmentStore
}, learnerID string) (bool, error) {
	domains, err := persistence.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return false, err
	}
	for _, domain := range domains {
		if domain == nil || !domain.HighStakes {
			continue
		}
		reviewed, err := persistence.HasHumanReviewedEvaluationInDomain(ctx, learnerID, domain.ID)
		if err != nil {
			return false, err
		}
		if !reviewed {
			return false, nil
		}
	}
	return true, nil
}

// alertKindOLM is the alert tag used by sendOLM for daily-dedup checks
// (WasAlertSentToday + CreateScheduledAlert). Single source of truth so the
// two call sites cannot drift apart.
const alertKindOLM = "OLM"
