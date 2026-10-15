// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// withTenantStore opens only the database phase of an independent tenant job.
// Its callback must never cross a network boundary. The child cannot reopen
// the root pool accidentally: its store is bound to this exact RLS transaction.
func (s *Scheduler) withTenantStore(ctx context.Context, fn func(context.Context, *Scheduler) error) error {
	if s.currentTenantScope == nil {
		return fn(ctx, s)
	}
	return s.store.WithTenantTx(ctx, *s.currentTenantScope, func(txCtx context.Context, scoped storeport.Store) error {
		child := *s
		child.store = scoped
		child.currentTenantScope = nil
		child.tenantDBPhase = true
		return fn(txCtx, &child)
	})
}

type webhookPushRecord struct {
	queueID int64
	brief   *models.WebhookBrief
}

// webhookDispatch contains no credential. Preparation commits queue claims
// before this value leaves its transaction; the HTTP boundary is entered by
// dispatchDurableWebhook in a separate, committed transaction.
type webhookDispatch struct {
	learnerID string
	alertType string
	domainID  string
	intrusive bool
	queueIDs  []int64
	payload   discordPayload
	pushes    []webhookPushRecord
	now       time.Time
}

func (s *Scheduler) dispatchWebhookTarget(ctx context.Context, prepare func(context.Context, *Scheduler) (*webhookDispatch, bool)) bool {
	var delivery *webhookDispatch
	var failed bool
	if err := s.withTenantStore(ctx, func(txCtx context.Context, child *Scheduler) error {
		delivery, failed = prepare(txCtx, child)
		// Preparation can persist failures or release claims. Commit those
		// transitions even when one learner could not be prepared.
		return nil
	}); err != nil {
		s.logger.Error("scheduler: webhook preparation failed", "error_type", fmt.Sprintf("%T", err))
		return true
	}
	if delivery == nil {
		return failed
	}
	if err := s.dispatchDurableWebhook(ctx, delivery.learnerID, delivery.alertType, delivery.domainID,
		delivery.intrusive, delivery.queueIDs, delivery.payload, delivery.now); err != nil {
		s.logger.Error("scheduler: durable webhook delivery", "error_type", fmt.Sprintf("%T", err), "learner", delivery.learnerID)
		return true
	}
	if err := s.withTenantStore(ctx, func(txCtx context.Context, child *Scheduler) error {
		for _, push := range delivery.pushes {
			if _, err := child.store.CreateWebhookPushLog(txCtx, delivery.learnerID, push.queueID, push.brief, delivery.now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		s.logger.Warn("scheduler: webhook push log failed", "error_type", fmt.Sprintf("%T", err), "learner", delivery.learnerID)
		return true
	}
	s.logger.Info("scheduler: webhook dispatched", "learner", delivery.learnerID, "kind", delivery.alertType)
	return failed
}
