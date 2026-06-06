package db

import (
	"context"
	"testing"
	"time"
)

// TestRateLimitBackendTokenBucket verifies the shared token bucket: a burst-N
// bucket allows exactly N immediate requests then denies the (N+1)th, and after
// a refill interval it allows again. Runs on SQLite by default and on Postgres
// when TUTOR_TEST_PG_DSN is set (setupTestDB is dialect-aware).
func TestRateLimitBackendTokenBucket(t *testing.T) {
	store := setupTestDB(t)
	backend := NewRateLimitBackend(store)
	ctx := context.Background()

	const (
		burst = 3
		rate  = 1.0 // 1 token/sec
		key   = "ip:198.51.100.7"
	)
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

	// N immediate requests at the same instant: all allowed.
	for i := 0; i < burst; i++ {
		ok, err := backend.Allow(ctx, key, rate, burst, base)
		if err != nil {
			t.Fatalf("Allow #%d: %v", i+1, err)
		}
		if !ok {
			t.Fatalf("Allow #%d: expected allowed within burst, got denied", i+1)
		}
	}

	// (N+1)th at the same instant: denied (bucket empty, no time elapsed).
	if ok, err := backend.Allow(ctx, key, rate, burst, base); err != nil {
		t.Fatalf("Allow N+1: %v", err)
	} else if ok {
		t.Fatalf("Allow N+1: expected denied (bucket empty), got allowed")
	}

	// After a refill interval (>=1s at 1 token/sec): allowed again.
	later := base.Add(1100 * time.Millisecond)
	if ok, err := backend.Allow(ctx, key, rate, burst, later); err != nil {
		t.Fatalf("Allow after refill: %v", err)
	} else if !ok {
		t.Fatalf("Allow after refill: expected allowed, got denied")
	}

	// A different key has its own independent bucket.
	if ok, err := backend.Allow(ctx, "ip:203.0.113.9", rate, burst, base); err != nil {
		t.Fatalf("Allow other key: %v", err)
	} else if !ok {
		t.Fatalf("Allow other key: expected allowed (fresh bucket), got denied")
	}
}

// TestLoginFailureBackendWindow verifies Record/CountInWindow/Reset on the
// shared store. Runs on SQLite and (when TUTOR_TEST_PG_DSN is set) Postgres.
func TestLoginFailureBackendWindow(t *testing.T) {
	store := setupTestDB(t)
	backend := NewLoginFailureBackend(store)
	ctx := context.Background()

	const (
		key    = "user@example.com"
		window = 10 * time.Minute
		m      = 4
	)
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

	for i := 0; i < m; i++ {
		if _, err := backend.Record(ctx, key, now); err != nil {
			t.Fatalf("Record #%d: %v", i+1, err)
		}
	}

	n, err := backend.CountInWindow(ctx, key, window, now)
	if err != nil {
		t.Fatalf("CountInWindow: %v", err)
	}
	if n != m {
		t.Fatalf("CountInWindow = %d, want %d", n, m)
	}

	// Stamps older than the window must not be counted.
	if old, err := backend.CountInWindow(ctx, key, time.Minute, now.Add(time.Hour)); err != nil {
		t.Fatalf("CountInWindow (stale): %v", err)
	} else if old != 0 {
		t.Fatalf("CountInWindow stale = %d, want 0", old)
	}

	if err := backend.Reset(ctx, key); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if n, err := backend.CountInWindow(ctx, key, window, now); err != nil {
		t.Fatalf("CountInWindow after reset: %v", err)
	} else if n != 0 {
		t.Fatalf("CountInWindow after reset = %d, want 0", n)
	}
}
