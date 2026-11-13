// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"errors"
	"time"
)

// cleanupSQLState exposes only the bounded SQLSTATE code. Never log query text,
// credential values or the database error detail from a maintenance failure.
func cleanupSQLState(err error) string {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		if len(code) == 5 {
			for _, ch := range code {
				if !(ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z') {
					return "unknown"
				}
			}
			return code
		}
	}
	return "unknown"
}

func (s *Scheduler) cleanupExpiredData() scheduledJobResult {
	result := scheduledJobSucceeded()
	ctx, cancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
	defer cancel()
	now := time.Now().UTC()
	codes, err := s.store.CleanupExpiredCodes(ctx)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup codes", "sqlstate", cleanupSQLState(err))
		return result
	} else if codes > 0 {
		s.logger.Info("scheduler: cleaned expired codes", "count", codes)
	}

	tokens, err := s.store.CleanupExpiredRefreshTokens(ctx)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup tokens", "sqlstate", cleanupSQLState(err))
		return result
	} else if tokens > 0 {
		s.logger.Info("scheduler: cleaned expired refresh tokens", "count", tokens)
	}

	accountTokens, err := s.store.CleanupExpiredAccountTokens(ctx)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup account tokens", "sqlstate", cleanupSQLState(err))
		return result
	} else if accountTokens > 0 {
		s.logger.Info("scheduler: cleaned account tokens", "count", accountTokens)
	}

	oauthClients, err := s.store.CleanupExpiredOAuthClients(ctx)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup OAuth clients", "sqlstate", cleanupSQLState(err))
		return result
	} else if oauthClients > 0 {
		s.logger.Info("scheduler: cleaned OAuth clients", "count", oauthClients)
	}

	rateBuckets, loginFailures, err := s.store.CleanupRateLimitState(
		ctx, now.Add(-24*time.Hour), now.Add(-24*time.Hour),
	)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup shared auth limits", "sqlstate", cleanupSQLState(err))
		return result
	} else if rateBuckets > 0 || loginFailures > 0 {
		s.logger.Info("scheduler: cleaned shared auth limits", "rate_buckets", rateBuckets, "login_failures", loginFailures)
	}

	requeued, err := s.store.RequeueStaleWebhookClaims(ctx, now.Add(-15*time.Minute), now)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: requeue stale webhook claims", "sqlstate", cleanupSQLState(err))
		return result
	} else if requeued > 0 {
		s.logger.Info("scheduler: requeued stale webhook claims", "count", requeued)
	}

	expired, err := s.store.ExpirePastWebhookMessages(ctx, now)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: expire webhook queue", "sqlstate", cleanupSQLState(err))
		return result
	} else if expired > 0 {
		s.logger.Info("scheduler: expired webhook messages", "count", expired)
	}

	// Housekeeping for the distributed-scheduler lease table so it doesn't
	// grow unbounded. Best-effort: a no-op on single-node deployments (the
	// table is simply empty) and never fails the cleanup tick.
	if purged, err := s.store.PurgeJobRunsBefore(ctx, time.Now().Add(-7*24*time.Hour)); err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: purge job runs", "sqlstate", cleanupSQLState(err))
		return result
	} else if purged > 0 {
		s.logger.Info("scheduler: purged old job runs", "count", purged)
	}
	return result
}
