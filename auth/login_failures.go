// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"log/slog"
	"sync"
	"time"

	storeport "tutor-mcp/store"
)

// LoginFailureBackend is an optional shared store the LoginFailureTracker
// delegates to so per-account brute-force lockout is enforced across a fleet
// instead of independently per process. Implemented in package db
// (Postgres-backed); nil means the in-memory default.
//
// It is an alias for store.LoginFailureBackend: the interface lives in the
// neutral persistence-port package so package db can implement it without an
// import cycle (auth and db both depend on store; neither depends on the other).
type LoginFailureBackend = storeport.LoginFailureBackend

// LoginFailureTracker counts password-mismatch attempts per email over a
// sliding time window. Issue #36 part 4: per-account lockout in addition to
// the per-IP rate limit. Once `threshold` failures occur within `window`,
// Allow() returns false until the oldest failure decays out.
//
// Email is the bucket key (case-folded by the caller). Successful logins call
// Reset to clear the history.
type LoginFailureTracker struct {
	mu        sync.Mutex
	fails     map[string][]time.Time
	threshold int
	window    time.Duration
	backend   LoginFailureBackend // optional shared store; nil = in-memory only
}

// NewLoginFailureTracker constructs a tracker with the given threshold and
// rolling window. Threshold ≤ 0 disables the tracker (Allow always true).
func NewLoginFailureTracker(threshold int, window time.Duration) *LoginFailureTracker {
	return &LoginFailureTracker{
		fails:     make(map[string][]time.Time),
		threshold: threshold,
		window:    window,
	}
}

// SetBackend installs a shared backend after construction so per-account
// lockout is enforced fleet-wide. Passing nil restores the in-memory default.
func (t *LoginFailureTracker) SetBackend(backend LoginFailureBackend) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.backend = backend
	t.mu.Unlock()
}

// Allow returns true if the email is below the threshold of recent failures.
// Stale entries (older than `window`) are pruned in passing.
func (t *LoginFailureTracker) Allow(email string) bool {
	if t == nil || t.threshold <= 0 {
		return true
	}
	backend := t.getBackend()
	if backend != nil {
		n, err := backend.CountInWindow(context.Background(), email, t.window, time.Now())
		if err != nil {
			// Degrade to the process-local account bucket. This cannot reproduce
			// fleet-wide state, but it avoids both a global lockout and a complete
			// bypass of per-account brute-force protection.
			slog.Warn("login failure backend error on Allow, using local fallback", "err", err)
			return t.allowLocal(email, time.Now())
		}
		return n < t.threshold
	}
	return t.allowLocal(email, time.Now())
}

func (t *LoginFailureTracker) allowLocal(email string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(email, now)
	return len(t.fails[email]) < t.threshold
}

// Record stamps a new failure for the email and returns the resulting count
// within the window. Stale entries are pruned at the same time.
func (t *LoginFailureTracker) Record(email string) int {
	if t == nil || t.threshold <= 0 {
		return 0
	}
	backend := t.getBackend()
	if backend != nil {
		n, err := backend.Record(context.Background(), email, time.Now())
		if err != nil {
			slog.Warn("login failure backend error on Record, using local fallback", "err", err)
			return t.recordLocal(email, time.Now())
		}
		return n
	}
	return t.recordLocal(email, time.Now())
}

func (t *LoginFailureTracker) recordLocal(email string, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(email, now)
	t.fails[email] = append(t.fails[email], now)
	return len(t.fails[email])
}

// Reset clears the failure history for an email — call on a successful login
// so a learner who eventually authenticates is not penalised by earlier typos.
func (t *LoginFailureTracker) Reset(email string) {
	if t == nil {
		return
	}
	backend := t.getBackend()
	if backend != nil {
		if err := backend.Reset(context.Background(), email); err != nil {
			slog.Warn("login failure backend error on Reset", "err", err)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, email)
}

func (t *LoginFailureTracker) getBackend() LoginFailureBackend {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.backend
}

func (t *LoginFailureTracker) pruneLocked(email string, now time.Time) {
	cutoff := now.Add(-t.window)
	stamps := t.fails[email]
	fresh := stamps[:0]
	for _, ts := range stamps {
		if ts.After(cutoff) {
			fresh = append(fresh, ts)
		}
	}
	if len(fresh) == 0 {
		delete(t.fails, email)
		return
	}
	t.fails[email] = fresh
}
