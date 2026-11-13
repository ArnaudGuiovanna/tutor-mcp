// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package auth

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	storeport "tutor-mcp/store"
)

// LoginFailureBackend is an optional shared store the LoginFailureTracker
// delegates to so per-account brute-force signals are shared across a fleet
// instead of independently per process. Implemented in package db
// (Postgres-backed); nil means the in-memory default.
//
// It is an alias for store.LoginFailureBackend: the interface lives in the
// neutral persistence-port package so package db can implement it without an
// import cycle (auth and db both depend on store; neither depends on the other).
type LoginFailureBackend = storeport.LoginFailureBackend

// LoginFailureTracker counts password-mismatch attempts per email over a
// bounded time window. It drives progressive Retry-After responses after a
// failed bcrypt comparison; it must never prevent checking a correct password.
//
// Email is the bucket key (case-folded by the caller). Successful logins call
// Reset to clear the history.
type LoginFailureTracker struct {
	mu        sync.Mutex
	fails     map[string]*loginFailureBucket
	lru       *list.List
	lastPrune time.Time
	threshold int
	window    time.Duration
	backend   LoginFailureBackend // optional shared store; nil = in-memory only
}

type loginFailureBucket struct {
	stamps []time.Time
	elem   *list.Element
}

const maxLoginFailureBuckets = 10_000

// NewLoginFailureTracker constructs a tracker with the given threshold and
// rolling window. Threshold ≤ 0 disables the tracker.
func NewLoginFailureTracker(threshold int, window time.Duration) *LoginFailureTracker {
	return &LoginFailureTracker{
		fails:     make(map[string]*loginFailureBucket),
		lru:       list.New(),
		threshold: threshold,
		window:    window,
	}
}

// SetBackend installs a shared backend after construction so per-account
// failure signals are fleet-wide. Passing nil restores the in-memory default.
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
	return t.AllowContext(context.Background(), email)
}

// AllowContext is retained for callers that need to inspect the threshold.
// Login handlers intentionally do not call it before bcrypt: doing so would
// let an attacker lock the legitimate account owner out.
func (t *LoginFailureTracker) AllowContext(parent context.Context, email string) bool {
	if t == nil || t.threshold <= 0 {
		return true
	}
	backend := t.getBackend()
	if backend != nil {
		ctx, cancel := loginFailureBackendContext(parent)
		n, err := backend.CountInWindow(ctx, email, t.window, time.Now())
		cancel()
		if err != nil {
			// Degrade to the process-local account bucket. This cannot reproduce
			// fleet-wide state, but it preserves a bounded local abuse signal.
			slog.Warn("login failure backend error on Allow, using local fallback", "error_type", fmt.Sprintf("%T", err))
			return t.allowLocal(email, time.Now())
		}
		return n < t.threshold
	}
	return t.allowLocal(email, time.Now())
}

func (t *LoginFailureTracker) allowLocal(email string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneAllLocked(now)
	t.pruneLocked(email, now)
	bucket := t.fails[email]
	if bucket == nil {
		return true
	}
	t.lru.MoveToFront(bucket.elem)
	return len(bucket.stamps) < t.threshold
}

// Record stamps a new failure for the email and returns the resulting count
// within the window. Stale entries are pruned at the same time.
func (t *LoginFailureTracker) Record(email string) int {
	return t.RecordContext(context.Background(), email)
}

// RecordContext records one failed credential check while preserving the
// request cancellation/deadline for a shared persistence backend.
func (t *LoginFailureTracker) RecordContext(parent context.Context, email string) int {
	if t == nil || t.threshold <= 0 {
		return 0
	}
	backend := t.getBackend()
	if backend != nil {
		ctx, cancel := loginFailureBackendContext(parent)
		n, err := backend.Record(ctx, email, time.Now())
		cancel()
		if err != nil {
			slog.Warn("login failure backend error on Record, using local fallback", "error_type", fmt.Sprintf("%T", err))
			return t.recordLocal(email, time.Now())
		}
		return n
	}
	return t.recordLocal(email, time.Now())
}

func (t *LoginFailureTracker) recordLocal(email string, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneAllLocked(now)
	t.pruneLocked(email, now)
	bucket := t.fails[email]
	if bucket == nil {
		if len(t.fails) >= maxLoginFailureBuckets {
			t.removeOldestLocked()
		}
		bucket = &loginFailureBucket{}
		bucket.elem = t.lru.PushFront(email)
		t.fails[email] = bucket
	} else {
		t.lru.MoveToFront(bucket.elem)
	}
	// Counts above threshold+5 all map to the same capped Retry-After value;
	// retaining more timestamps would let one address grow memory without bound.
	stampCap := t.threshold + 6
	if stampCap < 1 {
		stampCap = 1
	}
	if len(bucket.stamps) >= stampCap {
		copy(bucket.stamps, bucket.stamps[len(bucket.stamps)-stampCap+1:])
		bucket.stamps = bucket.stamps[:stampCap-1]
	}
	bucket.stamps = append(bucket.stamps, now)
	return len(bucket.stamps)
}

// Reset clears the failure history for an email — call on a successful login
// so a learner who eventually authenticates is not penalised by earlier typos.
func (t *LoginFailureTracker) Reset(email string) {
	t.ResetContext(context.Background(), email)
}

// ResetContext clears failure history after successful authentication.
func (t *LoginFailureTracker) ResetContext(parent context.Context, email string) {
	if t == nil {
		return
	}
	backend := t.getBackend()
	if backend != nil {
		ctx, cancel := loginFailureBackendContext(parent)
		err := backend.Reset(ctx, email)
		cancel()
		if err != nil {
			slog.Warn("login failure backend error on Reset", "error_type", fmt.Sprintf("%T", err))
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeBucketLocked(email)
}

// Threshold exposes the transition point without exposing account state.
func (t *LoginFailureTracker) Threshold() int {
	if t == nil {
		return 0
	}
	return t.threshold
}

// RetryAfter returns a bounded, progressive, non-blocking penalty. Callers
// advertise it only after a failed password comparison; the server never
// sleeps and a subsequent correct credential is always checked immediately.
func (t *LoginFailureTracker) RetryAfter(count int) time.Duration {
	if t == nil || t.threshold <= 0 || count < t.threshold {
		return 0
	}
	exponent := count - t.threshold
	if exponent > 5 {
		exponent = 5
	}
	seconds := 1 << exponent
	if seconds > 30 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func loginFailureBackendContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, authBackendTimeout)
}

func (t *LoginFailureTracker) getBackend() LoginFailureBackend {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.backend
}

func (t *LoginFailureTracker) pruneLocked(email string, now time.Time) {
	cutoff := now.Add(-t.window)
	bucket := t.fails[email]
	if bucket == nil {
		return
	}
	stamps := bucket.stamps
	fresh := stamps[:0]
	for _, ts := range stamps {
		if ts.After(cutoff) {
			fresh = append(fresh, ts)
		}
	}
	if len(fresh) == 0 {
		t.removeBucketLocked(email)
		return
	}
	bucket.stamps = fresh
}

func (t *LoginFailureTracker) pruneAllLocked(now time.Time) {
	if !t.lastPrune.IsZero() && now.Sub(t.lastPrune) < time.Minute {
		return
	}
	for email := range t.fails {
		t.pruneLocked(email, now)
	}
	t.lastPrune = now
}

func (t *LoginFailureTracker) removeOldestLocked() {
	if elem := t.lru.Back(); elem != nil {
		email, _ := elem.Value.(string)
		t.removeBucketLocked(email)
	}
}

func (t *LoginFailureTracker) removeBucketLocked(email string) {
	if bucket := t.fails[email]; bucket != nil {
		t.lru.Remove(bucket.elem)
		delete(t.fails, email)
	}
}
