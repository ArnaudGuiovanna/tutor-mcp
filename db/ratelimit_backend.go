// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	storeport "tutor-mcp/store"
)

// Shared, database-backed implementations of the rate-limit and login-failure
// backend ports (store.RateLimitBackend / store.LoginFailureBackend). They let a
// multi-instance fleet enforce one combined view of throttling / lockout state
// instead of independent per-process counters. Opt-in via
// RATELIMIT_BACKEND=postgres; the in-memory default path in package auth is
// untouched when no backend is installed.
//
// The ports live in the neutral store package, which both auth (consumer) and
// db (implementation) depend on — neither imports the other, so the hexagonal
// boundary holds with no import cycle.

// Compile-time proof the adapters satisfy the backend ports.
var (
	_ storeport.RateLimitBackend    = (*rateLimitBackend)(nil)
	_ storeport.LoginFailureBackend = (*loginFailureBackend)(nil)
)

// rateLimitBackend adapts *Store to store.RateLimitBackend.
type rateLimitBackend struct{ s *Store }

// NewRateLimitBackend returns a shared, DB-backed token-bucket rate-limit
// backend wrapping the given Store.
func NewRateLimitBackend(s *Store) storeport.RateLimitBackend { return &rateLimitBackend{s: s} }

// Allow atomically refills and consumes one token for key under a
// (rate tokens/sec, burst) token bucket. Correct under concurrent fleet access:
// the bucket row is read FOR UPDATE inside a transaction (on SQLite the
// BEGIN IMMEDIATE writer lock provides the same serialization), so two
// instances cannot both observe a stale token count and double-spend.
func (b *rateLimitBackend) Allow(ctx context.Context, key string, rate float64, burst int, now time.Time) (bool, error) {
	return b.s.rateLimitAllow(ctx, key, rate, burst, now)
}

// rateLimitAllow runs the token-bucket read-modify-write in its own
// transaction. Mirrors the in-memory RateLimiter.Allow arithmetic so behaviour
// matches across the memory and shared backends.
func (s *Store) rateLimitAllow(ctx context.Context, key string, rate float64, burst int, now time.Time) (bool, error) {
	now = now.UTC()
	allowed := false
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		// Materialize the row before locking it. ON CONFLICT DO NOTHING is the
		// first-key concurrency boundary on PostgreSQL: a concurrent inserter
		// waits for the winner and then observes its spent token count instead
		// of both callers resetting the bucket to burst-1.
		if _, err := txs.exec(ctx,
			`INSERT INTO rate_limit_buckets (bucket_key, tokens, updated_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT (bucket_key) DO NOTHING`,
			key, float64(burst), now,
		); err != nil {
			return fmt.Errorf("rate limit allow: materialize: %w", err)
		}

		query := `SELECT tokens, updated_at FROM rate_limit_buckets WHERE bucket_key = ?`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE`
		}
		var tokens float64
		var updatedAt flexTime
		err := txs.queryRow(ctx, query, key).Scan(&tokens, &updatedAt)
		if err != nil {
			return fmt.Errorf("rate limit allow: select: %w", err)
		}

		// Refill based on elapsed time, capped at burst.
		elapsed := now.Sub(updatedAt.Time).Seconds()
		if elapsed < 0 {
			elapsed = 0
		}
		tokens += elapsed * rate
		if tokens > float64(burst) {
			tokens = float64(burst)
		}

		if tokens >= 1 {
			tokens--
			allowed = true
		}

		if _, err := txs.exec(ctx,
			`UPDATE rate_limit_buckets SET tokens = ?, updated_at = ? WHERE bucket_key = ?`,
			tokens, now, key,
		); err != nil {
			return fmt.Errorf("rate limit allow: update: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return allowed, nil
}

// loginFailureBackend adapts *Store to auth.LoginFailureBackend.
type loginFailureBackend struct{ s *Store }

// NewLoginFailureBackend returns a shared, DB-backed login-failure backend
// wrapping the given Store.
func NewLoginFailureBackend(s *Store) storeport.LoginFailureBackend {
	return &loginFailureBackend{s: s}
}

// Record atomically increments the account's bounded failure window and
// returns its capped count. The port keeps Record window-free, so the shared
// backend uses the same ten-minute window as the production tracker.
func (b *loginFailureBackend) Record(ctx context.Context, key string, now time.Time) (int, error) {
	return b.s.recordLoginFailure(ctx, key, now)
}

func (b *loginFailureBackend) CountInWindow(ctx context.Context, key string, window time.Duration, now time.Time) (int, error) {
	return b.s.countLoginFailuresInWindow(ctx, key, window, now)
}

func (b *loginFailureBackend) Reset(ctx context.Context, key string) error {
	return b.s.resetLoginFailures(ctx, key)
}

const (
	defaultLoginWindow        = 10 * time.Minute
	maxLoginFailuresPerWindow = 100
)

func (s *Store) recordLoginFailure(ctx context.Context, key string, now time.Time) (int, error) {
	now = now.UTC()
	cutoff := now.Add(-defaultLoginWindow)
	var count int
	err := s.queryRow(ctx,
		`INSERT INTO login_failure_windows
		    (account_key, window_started_at, last_attempt_at, failure_count, updated_at)
		 VALUES (?, ?, ?, 1, ?)
		 ON CONFLICT (account_key) DO UPDATE SET
		    window_started_at = CASE
		        WHEN login_failure_windows.window_started_at <= ? THEN excluded.window_started_at
		        ELSE login_failure_windows.window_started_at END,
		    last_attempt_at = CASE
		        WHEN login_failure_windows.last_attempt_at < excluded.last_attempt_at THEN excluded.last_attempt_at
		        ELSE login_failure_windows.last_attempt_at END,
		    failure_count = CASE
		        WHEN login_failure_windows.window_started_at <= ? THEN 1
		        WHEN login_failure_windows.failure_count >= ? THEN ?
		        ELSE login_failure_windows.failure_count + 1 END,
		    updated_at = CASE
		        WHEN login_failure_windows.updated_at < excluded.updated_at THEN excluded.updated_at
		        ELSE login_failure_windows.updated_at END
		 RETURNING failure_count`,
		key, now, now, now, cutoff, cutoff,
		maxLoginFailuresPerWindow, maxLoginFailuresPerWindow,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("record login failure: %w", err)
	}
	return count, nil
}

func (s *Store) countLoginFailuresInWindow(ctx context.Context, key string, window time.Duration, now time.Time) (int, error) {
	if window <= 0 {
		return 0, nil
	}
	cutoff := now.UTC().Add(-window)
	var count int
	err := s.queryRow(ctx,
		`SELECT failure_count
		 FROM login_failure_windows
		 WHERE account_key = ? AND window_started_at > ?`,
		key, cutoff,
	).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("count login failures: %w", err)
	}
	return count, nil
}

func (s *Store) resetLoginFailures(ctx context.Context, key string) error {
	if _, err := s.exec(ctx, `DELETE FROM login_failure_windows WHERE account_key = ?`, key); err != nil {
		return fmt.Errorf("reset login failures: %w", err)
	}
	return nil
}

// CleanupRateLimitState deletes stale shared buckets and login-failure windows.
// It is intentionally a DB method rather than a scheduler dependency so an
// operator or future maintenance loop can invoke it without coupling auth
// state to webhook scheduling.
func (s *Store) CleanupRateLimitState(ctx context.Context, bucketCutoff, failureCutoff time.Time) (int64, int64, error) {
	var bucketsDeleted, failuresDeleted int64
	err := s.inTx(ctx, nil, func(txs *Store) error {
		bucketResult, err := txs.exec(ctx,
			`DELETE FROM rate_limit_buckets WHERE updated_at < ?`,
			bucketCutoff.UTC(),
		)
		if err != nil {
			return fmt.Errorf("cleanup rate-limit buckets: %w", err)
		}
		bucketsDeleted, err = bucketResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("cleanup rate-limit buckets count: %w", err)
		}

		failureResult, err := txs.exec(ctx,
			`DELETE FROM login_failure_windows WHERE last_attempt_at < ?`,
			failureCutoff.UTC(),
		)
		if err != nil {
			return fmt.Errorf("cleanup login failures: %w", err)
		}
		failuresDeleted, err = failureResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("cleanup login failures count: %w", err)
		}
		// Legacy rows are no longer written after migration. Clean any row
		// manually restored from an older backup so the retired journal cannot
		// grow or unexpectedly influence a future rollback.
		legacyResult, err := txs.exec(ctx,
			`DELETE FROM login_failures WHERE attempted_at < ?`, failureCutoff.UTC(),
		)
		if err != nil {
			return fmt.Errorf("cleanup legacy login failures: %w", err)
		}
		legacyDeleted, err := legacyResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("cleanup legacy login failures count: %w", err)
		}
		failuresDeleted += legacyDeleted
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return bucketsDeleted, failuresDeleted, nil
}

// txOptionsForDialect mirrors WithTx's isolation choice: SERIALIZABLE
// (BEGIN IMMEDIATE) on SQLite, READ COMMITTED on Postgres where row exclusivity
// comes from SELECT ... FOR UPDATE instead.
func txOptionsForDialect(d Dialect) *sql.TxOptions {
	if d == DialectPostgres {
		return &sql.TxOptions{Isolation: sql.LevelReadCommitted}
	}
	return &sql.TxOptions{Isolation: sql.LevelSerializable}
}
