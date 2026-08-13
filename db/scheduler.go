// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	storeport "tutor-mcp/store"
)

// ClaimJobRun atomically claims the (name, windowKey) lease. Returns true if
// THIS caller won the claim (the run should execute here), false if another
// instance already claimed it. Dialect-neutral via INSERT ... ON CONFLICT DO
// NOTHING + RowsAffected.
func (s *Store) ClaimJobRun(ctx context.Context, name, windowKey string) (bool, error) {
	res, err := s.exec(ctx,
		`INSERT INTO scheduled_job_runs (name, window_key) VALUES (?, ?) ON CONFLICT (name, window_key) DO NOTHING`,
		name, windowKey)
	if err != nil {
		return false, fmt.Errorf("claim job run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim job run rows: %w", err)
	}
	return n == 1, nil
}

// AcquireJobRunLease creates a processing row for a new cron window or takes
// over a retry/stale processing row whose lease is due. The whole transition is
// serialized so a fleet has exactly one current owner. A stale final attempt is
// terminalized instead of being stranded forever in processing.
func (s *Store) AcquireJobRunLease(
	ctx context.Context,
	name, windowKey, owner string,
	now time.Time,
	leaseDuration time.Duration,
	maxAttempts int,
) (bool, error) {
	if err := validateJobLeaseInput(name, windowKey, owner, leaseDuration, maxAttempts); err != nil {
		return false, err
	}
	now = now.UTC()
	leasedUntil := now.Add(leaseDuration)
	acquired := false
	err := s.inTx(ctx, nil, func(txs *Store) error {
		// A process may die during its final allowed attempt. Turn that stale row
		// into an explicit DLQ state before trying the normal acquisition.
		if _, err := txs.exec(ctx,
			`UPDATE scheduled_job_runs
			 SET status = 'dead', owner = '', leased_until = NULL,
			     next_attempt_at = NULL,
			     last_error = CASE WHEN last_error = '' THEN 'lease_expired_after_max_attempts' ELSE last_error END,
			     completed_at = ?, updated_at = ?
			 WHERE name = ? AND window_key = ?
			   AND attempts >= ?
			   AND ((status = 'processing' AND leased_until IS NOT NULL AND leased_until <= ?)
			     OR (status = 'retry' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)))`,
			now, now, name, windowKey, maxAttempts, now, now); err != nil {
			return fmt.Errorf("terminalize exhausted job run: %w", err)
		}

		res, err := txs.exec(ctx,
			`INSERT INTO scheduled_job_runs
			 (name, window_key, claimed_at, status, owner, leased_until, attempts,
			  max_attempts, next_attempt_at, last_error, completed_at, updated_at)
			 VALUES (?, ?, ?, 'processing', ?, ?, 1, ?, NULL, '', NULL, ?)
			 ON CONFLICT (name, window_key) DO UPDATE SET
			   status = 'processing',
			   owner = excluded.owner,
			   leased_until = excluded.leased_until,
			   attempts = scheduled_job_runs.attempts + 1,
			   max_attempts = excluded.max_attempts,
			   next_attempt_at = NULL,
			   completed_at = NULL,
			   updated_at = excluded.updated_at
			 WHERE scheduled_job_runs.attempts < excluded.max_attempts
			   AND ((scheduled_job_runs.status = 'processing'
			         AND scheduled_job_runs.leased_until IS NOT NULL
			         AND scheduled_job_runs.leased_until <= excluded.updated_at)
			     OR (scheduled_job_runs.status = 'retry'
			         AND (scheduled_job_runs.next_attempt_at IS NULL
			              OR scheduled_job_runs.next_attempt_at <= excluded.updated_at)))`,
			name, windowKey, now, owner, leasedUntil, maxAttempts, now)
		if err != nil {
			return fmt.Errorf("acquire job run lease: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("acquire job run lease rows: %w", err)
		}
		acquired = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return acquired, nil
}

// RenewJobRunLease extends a live lease only for its current owner. Once a
// lease has expired, the old process cannot resurrect it after another worker
// may have started the same idempotent window.
func (s *Store) RenewJobRunLease(
	ctx context.Context,
	name, windowKey, owner string,
	now time.Time,
	leaseDuration time.Duration,
) (bool, error) {
	if err := validateJobLeaseInput(name, windowKey, owner, leaseDuration, 1); err != nil {
		return false, err
	}
	now = now.UTC()
	res, err := s.exec(ctx,
		`UPDATE scheduled_job_runs
		 SET leased_until = ?, updated_at = ?
		 WHERE name = ? AND window_key = ? AND status = 'processing'
		   AND owner = ? AND leased_until IS NOT NULL AND leased_until > ?`,
		now.Add(leaseDuration), now, name, windowKey, owner, now)
	if err != nil {
		return false, fmt.Errorf("renew job run lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renew job run lease rows: %w", err)
	}
	return n == 1, nil
}

// CompleteJobRun records the terminal success tombstone. The unexpired-lease
// predicate prevents a late worker from declaring success after ownership may
// already have become available to another process.
func (s *Store) CompleteJobRun(
	ctx context.Context,
	name, windowKey, owner string,
	completedAt time.Time,
) (bool, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(windowKey) == "" || strings.TrimSpace(owner) == "" {
		return false, fmt.Errorf("complete job run: name, window key, and owner are required")
	}
	completedAt = completedAt.UTC()
	res, err := s.exec(ctx,
		`UPDATE scheduled_job_runs
		 SET status = 'succeeded', owner = '', leased_until = NULL,
		     next_attempt_at = NULL, last_error = '', completed_at = ?, updated_at = ?
		 WHERE name = ? AND window_key = ? AND status = 'processing'
		   AND owner = ? AND leased_until IS NOT NULL AND leased_until > ?`,
		completedAt, completedAt, name, windowKey, owner, completedAt)
	if err != nil {
		return false, fmt.Errorf("complete job run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("complete job run rows: %w", err)
	}
	return n == 1, nil
}

// FailJobRun releases the current owner into a delayed retry, or moves the row
// to the dead-letter state once the configured attempt budget is exhausted.
// Only a stable error class is stored; raw errors may contain learner data,
// credentials, SQL, or filesystem paths.
func (s *Store) FailJobRun(
	ctx context.Context,
	name, windowKey, owner string,
	failedAt, nextAttemptAt time.Time,
	errorClass string,
) (bool, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(windowKey) == "" || strings.TrimSpace(owner) == "" {
		return false, fmt.Errorf("fail job run: name, window key, and owner are required")
	}
	failedAt = failedAt.UTC()
	if nextAttemptAt.Before(failedAt) {
		nextAttemptAt = failedAt
	}
	nextAttemptAt = nextAttemptAt.UTC()
	errorClass = normalizeJobErrorClass(errorClass)
	accepted := false
	err := s.inTx(ctx, nil, func(txs *Store) error {
		query := `SELECT attempts, max_attempts
			 FROM scheduled_job_runs
			 WHERE name = ? AND window_key = ? AND status = 'processing' AND owner = ?`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE`
		}
		var attempts, maxAttempts int
		if err := txs.queryRow(ctx, query, name, windowKey, owner).Scan(&attempts, &maxAttempts); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return fmt.Errorf("read owned job run before failure: %w", err)
		}

		var (
			res sql.Result
			err error
		)
		if attempts >= maxAttempts {
			res, err = txs.exec(ctx,
				`UPDATE scheduled_job_runs
				 SET status = 'dead', owner = '', leased_until = NULL,
				     next_attempt_at = NULL, last_error = ?, completed_at = ?, updated_at = ?
				 WHERE name = ? AND window_key = ? AND status = 'processing' AND owner = ?`,
				errorClass, failedAt, failedAt, name, windowKey, owner)
		} else {
			res, err = txs.exec(ctx,
				`UPDATE scheduled_job_runs
				 SET status = 'retry', owner = '', leased_until = NULL,
				     next_attempt_at = ?, last_error = ?, completed_at = NULL, updated_at = ?
				 WHERE name = ? AND window_key = ? AND status = 'processing' AND owner = ?`,
				nextAttemptAt, errorClass, failedAt, name, windowKey, owner)
		}
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("fail job run rows: %w", err)
		}
		accepted = n == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("fail job run: %w", err)
	}
	return accepted, nil
}

// ListRunnableJobRuns returns a bounded retry/stale-lease page. Acquisition is
// still authoritative; multiple replicas may list the same row, then exactly
// one wins AcquireJobRunLease.
func (s *Store) ListRunnableJobRuns(ctx context.Context, now time.Time, limit int) ([]storeport.ScheduledJobRunRef, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	now = now.UTC()
	if _, err := s.exec(ctx,
		`UPDATE scheduled_job_runs
		 SET status = 'dead', owner = '', leased_until = NULL,
		     next_attempt_at = NULL,
		     last_error = CASE WHEN last_error = '' THEN 'lease_expired_after_max_attempts' ELSE last_error END,
		     completed_at = ?, updated_at = ?
		 WHERE attempts >= max_attempts
		   AND ((status = 'processing' AND leased_until IS NOT NULL AND leased_until <= ?)
		     OR (status = 'retry' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)))`,
		now, now, now, now); err != nil {
		return nil, fmt.Errorf("terminalize exhausted runnable job runs: %w", err)
	}
	rows, err := s.query(ctx,
		`SELECT name, window_key
		 FROM scheduled_job_runs
		 WHERE attempts < max_attempts
		   AND ((status = 'processing' AND leased_until IS NOT NULL AND leased_until <= ?)
		     OR (status = 'retry' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)))
		 ORDER BY CASE WHEN status = 'retry' THEN next_attempt_at ELSE leased_until END ASC,
		          name ASC, window_key ASC
		 LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list runnable job runs: %w", err)
	}
	defer rows.Close()
	out := make([]storeport.ScheduledJobRunRef, 0)
	for rows.Next() {
		var ref storeport.ScheduledJobRunRef
		if err := rows.Scan(&ref.Name, &ref.WindowKey); err != nil {
			return nil, fmt.Errorf("scan runnable job run: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runnable job runs: %w", err)
	}
	return out, nil
}

// PurgeJobRunsBefore deletes only terminal rows. Active and delayed work is
// never discarded by retention, regardless of age.
func (s *Store) PurgeJobRunsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.exec(ctx,
		`DELETE FROM scheduled_job_runs
		 WHERE status IN ('succeeded', 'dead') AND claimed_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge job runs: %w", err)
	}
	return res.RowsAffected()
}

func validateJobLeaseInput(name, windowKey, owner string, leaseDuration time.Duration, maxAttempts int) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(windowKey) == "" || strings.TrimSpace(owner) == "" {
		return fmt.Errorf("job lease: name, window key, and owner are required")
	}
	if leaseDuration <= 0 {
		return fmt.Errorf("job lease: duration must be positive")
	}
	if maxAttempts < 1 || maxAttempts > 100 {
		return fmt.Errorf("job lease: max attempts must be between 1 and 100")
	}
	return nil
}

func normalizeJobErrorClass(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return "job_failed"
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '.' || r == ':' || r == '-' {
			continue
		}
		return "job_failed"
	}
	return value
}
