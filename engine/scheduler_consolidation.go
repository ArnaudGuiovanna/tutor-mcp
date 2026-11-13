// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"time"

	"tutor-mcp/memory"
)

func (s *Scheduler) runConsolidationCycle() scheduledJobResult {
	return s.runConsolidationCycleAt(time.Now().UTC())
}

func (s *Scheduler) runConsolidationCycleAt(now time.Time) scheduledJobResult {
	if !memory.Enabled() {
		return scheduledJobSucceeded()
	}
	return s.processConsolidationLearners(func(ctx context.Context, learnerID string) bool {
		jobs, err := memory.PrepareJobsContext(ctx, learnerID, now)
		if err != nil {
			s.logger.Warn("scheduler: consolidation prepare", "err", err, "learner", learnerID)
			return true
		}
		failed := false
		for _, job := range jobs {
			if ctx.Err() != nil {
				return true
			}
			if err := s.store.UpsertPendingConsolidation(ctx, learnerID, string(job.Period), job.PeriodKey, now); err != nil {
				failed = true
				s.logger.Warn("scheduler: consolidation enqueue failed", "err", err, "learner", learnerID, "period", job.PeriodKey)
			} else {
				s.logger.Info("scheduler: consolidation pending", "learner", learnerID, "period_type", job.Period, "period", job.PeriodKey)
			}
		}
		return failed
	})
}

func (s *Scheduler) requeueStaleConsolidations() scheduledJobResult {
	if !memory.Enabled() {
		return scheduledJobSucceeded()
	}
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(-s.consolidationDeliveryTimeout())
	count, err := s.store.RequeueStaleDeliveredConsolidations(ctx, cutoff)
	if err != nil {
		s.logger.Warn("scheduler: consolidation timeout requeue failed", "err", err)
		return scheduledJobFailed("consolidation_requeue_failed")
	}
	if count > 0 {
		s.logger.Info("scheduler: consolidation timeout requeued", "count", count)
	}
	return scheduledJobSucceeded()
}

func (s *Scheduler) consolidationDeliveryTimeout() time.Duration {
	if s.options.ConsolidationDeliveryTimeout > 0 {
		return s.options.ConsolidationDeliveryTimeout
	}
	return DefaultConsolidationDeliveryTimeout
}
