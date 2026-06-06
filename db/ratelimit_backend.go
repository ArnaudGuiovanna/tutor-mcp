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
	tx, err := s.root.BeginTx(ctx, txOptionsForDialect(s.dialect))
	if err != nil {
		return false, fmt.Errorf("rate limit allow: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	txs := &Store{db: tx, dialect: s.dialect}

	query := `SELECT tokens, updated_at FROM rate_limit_buckets WHERE bucket_key = ?`
	if s.dialect == DialectPostgres {
		query += ` FOR UPDATE`
	}
	var tokens float64
	var updatedAt flexTime
	err = txs.queryRow(ctx, query, key).Scan(&tokens, &updatedAt)
	switch {
	case err == sql.ErrNoRows:
		// First request for this key: start full, spend one.
		tokens = float64(burst) - 1
		if _, err := txs.exec(ctx,
			`INSERT INTO rate_limit_buckets (bucket_key, tokens, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (bucket_key) DO UPDATE SET tokens = excluded.tokens, updated_at = excluded.updated_at`,
			key, tokens, now,
		); err != nil {
			return false, fmt.Errorf("rate limit allow: insert: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("rate limit allow: commit: %w", err)
		}
		committed = true
		return true, nil
	case err != nil:
		return false, fmt.Errorf("rate limit allow: select: %w", err)
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

	allowed := false
	if tokens >= 1 {
		tokens--
		allowed = true
	}

	if _, err := txs.exec(ctx,
		`UPDATE rate_limit_buckets SET tokens = ?, updated_at = ? WHERE bucket_key = ?`,
		tokens, now, key,
	); err != nil {
		return false, fmt.Errorf("rate limit allow: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("rate limit allow: commit: %w", err)
	}
	committed = true
	return allowed, nil
}

// loginFailureBackend adapts *Store to auth.LoginFailureBackend.
type loginFailureBackend struct{ s *Store }

// NewLoginFailureBackend returns a shared, DB-backed login-failure backend
// wrapping the given Store.
func NewLoginFailureBackend(s *Store) storeport.LoginFailureBackend {
	return &loginFailureBackend{s: s}
}

// Record inserts a failure stamp and returns the count of failures within the
// default window. (The port keeps Record window-free so it stays a cheap
// insert; the authoritative lockout decision is made by Allow→CountInWindow
// with the tracker's real window.)
func (b *loginFailureBackend) Record(ctx context.Context, key string, now time.Time) (int, error) {
	return b.s.recordLoginFailure(ctx, key, now)
}

func (b *loginFailureBackend) CountInWindow(ctx context.Context, key string, window time.Duration, now time.Time) (int, error) {
	return b.s.countLoginFailuresInWindow(ctx, key, window, now)
}

func (b *loginFailureBackend) Reset(ctx context.Context, key string) error {
	return b.s.resetLoginFailures(ctx, key)
}

// defaultLoginWindow bounds the count Record returns. Record has no window
// parameter (the auth port keeps Record window-free so it stays a cheap insert),
// so it reports failures within this fixed lookback. It matches the tracker's
// default 10-minute window (auth.NewLoginFailureTracker in oauth.go); the
// authoritative lockout decision is made by Allow→CountInWindow with the
// tracker's real window, so a mismatch here would only affect the integer
// returned to the (unused) Record caller, never the lockout threshold.
const defaultLoginWindow = 10 * time.Minute

func (s *Store) recordLoginFailure(ctx context.Context, key string, now time.Time) (int, error) {
	now = now.UTC()
	if _, err := s.exec(ctx,
		`INSERT INTO login_failures (account_key, attempted_at) VALUES (?, ?)`,
		key, now,
	); err != nil {
		return 0, fmt.Errorf("record login failure: %w", err)
	}
	return s.countLoginFailuresInWindow(ctx, key, defaultLoginWindow, now)
}

func (s *Store) countLoginFailuresInWindow(ctx context.Context, key string, window time.Duration, now time.Time) (int, error) {
	cutoff := now.UTC().Add(-window)
	var count int
	if err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM login_failures WHERE account_key = ? AND attempted_at > ?`,
		key, cutoff,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count login failures: %w", err)
	}
	return count, nil
}

func (s *Store) resetLoginFailures(ctx context.Context, key string) error {
	if _, err := s.exec(ctx, `DELETE FROM login_failures WHERE account_key = ?`, key); err != nil {
		return fmt.Errorf("reset login failures: %w", err)
	}
	return nil
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
