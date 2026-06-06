// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"time"
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

// PurgeJobRunsBefore deletes lease rows older than cutoff (housekeeping).
func (s *Store) PurgeJobRunsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM scheduled_job_runs WHERE claimed_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge job runs: %w", err)
	}
	return res.RowsAffected()
}
