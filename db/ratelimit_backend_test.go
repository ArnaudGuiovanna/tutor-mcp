package db

import (
	"context"
	"sync"
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

func TestRateLimitBackendConcurrentFirstKeyHonorsBurst(t *testing.T) {
	store := setupTestDB(t)
	backend := NewRateLimitBackend(store)
	ctx := context.Background()
	const burst = 3
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

	const contenders = 12
	start := make(chan struct{})
	results := make(chan bool, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := backend.Allow(ctx, "oauth:198.51.100.42", 0, burst, now)
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			results <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	allowed := 0
	for ok := range results {
		if ok {
			allowed++
		}
	}
	if allowed != burst {
		t.Fatalf("concurrent first-key allowed=%d, want burst=%d", allowed, burst)
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

func TestLoginFailureBackendConcurrentIncrementIsAtomicAndCapped(t *testing.T) {
	store := setupTestDB(t)
	backend := NewLoginFailureBackend(store)
	ctx := context.Background()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	const contenders = maxLoginFailuresPerWindow + 24
	start := make(chan struct{})
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := backend.Record(ctx, "contended@example.com", now)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Record: %v", err)
		}
	}

	count, err := backend.CountInWindow(ctx, "contended@example.com", defaultLoginWindow, now)
	if err != nil {
		t.Fatal(err)
	}
	if count != maxLoginFailuresPerWindow {
		t.Fatalf("failure count=%d, want capped %d", count, maxLoginFailuresPerWindow)
	}
	var rows int
	if err := store.root.QueryRow(store.rebind(`SELECT COUNT(*) FROM login_failure_windows WHERE account_key = ?`), "contended@example.com").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("aggregate rows=%d, want exactly one", rows)
	}

	later := now.Add(defaultLoginWindow + time.Second)
	if resetCount, err := backend.Record(ctx, "contended@example.com", later); err != nil || resetCount != 1 {
		t.Fatalf("new window count=%d err=%v, want 1", resetCount, err)
	}
}

func TestCleanupRateLimitState(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	rateBackend := NewRateLimitBackend(store)
	loginBackend := NewLoginFailureBackend(store)
	old := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	recent := old.Add(2 * time.Hour)
	cutoff := old.Add(time.Hour)

	if _, err := rateBackend.Allow(ctx, "old-bucket", 0, 2, old); err != nil {
		t.Fatalf("seed old bucket: %v", err)
	}
	if _, err := rateBackend.Allow(ctx, "recent-bucket", 0, 2, recent); err != nil {
		t.Fatalf("seed recent bucket: %v", err)
	}
	if _, err := loginBackend.Record(ctx, "old@example.com", old); err != nil {
		t.Fatalf("seed old failure: %v", err)
	}
	if _, err := loginBackend.Record(ctx, "recent@example.com", recent); err != nil {
		t.Fatalf("seed recent failure: %v", err)
	}

	buckets, failures, err := store.CleanupRateLimitState(ctx, cutoff, cutoff)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if buckets != 1 || failures != 1 {
		t.Fatalf("deleted buckets=%d failures=%d, want 1/1", buckets, failures)
	}

	var bucketRows, failureRows int
	if err := store.root.QueryRow(`SELECT COUNT(*) FROM rate_limit_buckets`).Scan(&bucketRows); err != nil {
		t.Fatalf("count buckets: %v", err)
	}
	if err := store.root.QueryRow(`SELECT COUNT(*) FROM login_failure_windows`).Scan(&failureRows); err != nil {
		t.Fatalf("count failures: %v", err)
	}
	if bucketRows != 1 || failureRows != 1 {
		t.Fatalf("remaining buckets=%d failures=%d, want 1/1", bucketRows, failureRows)
	}
}
