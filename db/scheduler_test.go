// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestClaimJobRunExactlyOnce covers the historical single-shot ClaimJobRun
// helper: ON CONFLICT DO NOTHING and RowsAffected admit exactly one insert per
// (name, window_key) under concurrent callers. Dialect-aware setupTestDB checks
// SQLite by default and PostgreSQL with TUTOR_TEST_PG_DSN. Current distributed
// scheduling uses renewable leases and fencing, covered by separate tests.
func TestClaimJobRunExactlyOnce(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()

	const goroutines = 16
	var winners atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			ok, err := store.ClaimJobRun(ctx, "job", "w1")
			if err != nil {
				t.Errorf("ClaimJobRun: %v", err)
				return
			}
			if ok {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("exactly-once violated: got %d winners, want 1", got)
	}

	// A fresh window key is independently claimable exactly once.
	ok, err := store.ClaimJobRun(ctx, "job", "w2")
	if err != nil {
		t.Fatalf("ClaimJobRun w2: %v", err)
	}
	if !ok {
		t.Fatal("expected to win claim for new window w2")
	}
	// Re-claiming an already-claimed window returns false.
	ok, err = store.ClaimJobRun(ctx, "job", "w1")
	if err != nil {
		t.Fatalf("re-claim w1: %v", err)
	}
	if ok {
		t.Fatal("re-claim of w1 unexpectedly won")
	}
}

// TestPurgeJobRunsBefore proves housekeeping deletes only rows older than the
// cutoff and leaves recent ones intact.
func TestPurgeJobRunsBefore(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()

	// Two recent claims (claimed_at defaults to now).
	if _, err := store.ClaimJobRun(ctx, "job", "recent1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJobRun(ctx, "job", "recent2"); err != nil {
		t.Fatal(err)
	}
	// One artificially old row.
	old := time.Now().Add(-30 * 24 * time.Hour).UTC()
	if _, err := store.exec(ctx,
		`INSERT INTO scheduled_job_runs (name, window_key, claimed_at) VALUES (?, ?, ?)`,
		"job", "stale", old); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeJobRunsBefore(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeJobRunsBefore: %v", err)
	}
	if purged != 1 {
		t.Fatalf("expected 1 purged row, got %d", purged)
	}

	// The recent rows survive: re-claiming them must still return false.
	for _, w := range []string{"recent1", "recent2"} {
		ok, err := store.ClaimJobRun(ctx, "job", w)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("recent window %s was unexpectedly purged", w)
		}
	}
	// The stale window is gone, so it is claimable again.
	ok, err := store.ClaimJobRun(ctx, "job", "stale")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected stale window to be re-claimable after purge")
	}
}
