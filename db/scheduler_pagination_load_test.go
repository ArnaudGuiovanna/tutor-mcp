// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSchedulerTargetPaginationCardinality is an opt-in staging/load gate. Run
// it with TUTOR_TEST_SCHEDULER_CARDINALITY=10000 and =100000, and set
// TUTOR_TEST_PG_DSN to exercise the production database. Keeping the
// cardinality outside the normal unit suite avoids inserting 110k rows on
// every developer test run while preserving a repeatable, asserted gate.
func TestSchedulerTargetPaginationCardinality(t *testing.T) {
	rawCardinality := os.Getenv("TUTOR_TEST_SCHEDULER_CARDINALITY")
	if rawCardinality == "" {
		t.Skip("set TUTOR_TEST_SCHEDULER_CARDINALITY to run the scheduler load gate")
	}
	cardinality, err := strconv.Atoi(rawCardinality)
	if err != nil || cardinality < 1 || cardinality > 100_000 {
		t.Fatalf("TUTOR_TEST_SCHEDULER_CARDINALITY must be an integer between 1 and 100000, got %q", rawCardinality)
	}

	store := setupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	insertStarted := time.Now()
	insertSchedulerLoadLearners(t, ctx, store, cardinality)
	t.Logf("inserted %d scheduler learners in %s", cardinality, time.Since(insertStarted))

	const pageSize = 128
	targetStarted := time.Now()
	var targetCount, targetPages int
	lastID := ""
	for {
		page, err := store.ListWebhookDispatchTargetsPage(ctx, lastID, pageSize)
		if err != nil {
			t.Fatalf("list webhook target page after %q: %v", lastID, err)
		}
		if len(page) > pageSize {
			t.Fatalf("webhook page exceeded bound: got %d, limit %d", len(page), pageSize)
		}
		if len(page) == 0 {
			break
		}
		targetPages++
		for _, target := range page {
			if target.LearnerID == "" || target.Availability == nil {
				t.Fatal("webhook target omitted required narrow read model")
			}
			if target.LearnerID <= lastID {
				t.Fatalf("webhook keyset did not advance: previous=%q current=%q", lastID, target.LearnerID)
			}
			lastID = target.LearnerID
			targetCount++
		}
	}
	if targetCount != cardinality {
		t.Fatalf("webhook target count: got %d, want %d", targetCount, cardinality)
	}
	t.Logf("scanned %d webhook targets in %d pages of at most %d in %s", targetCount, targetPages, pageSize, time.Since(targetStarted))

	consolidationStarted := time.Now()
	var consolidationCount, consolidationPages int
	lastID = ""
	for {
		page, err := store.ListLearnerIDsForConsolidationPage(ctx, lastID, pageSize)
		if err != nil {
			t.Fatalf("list consolidation page after %q: %v", lastID, err)
		}
		if len(page) > pageSize {
			t.Fatalf("consolidation page exceeded bound: got %d, limit %d", len(page), pageSize)
		}
		if len(page) == 0 {
			break
		}
		consolidationPages++
		for _, learnerID := range page {
			if learnerID <= lastID {
				t.Fatalf("consolidation keyset did not advance: previous=%q current=%q", lastID, learnerID)
			}
			lastID = learnerID
			consolidationCount++
		}
	}
	// setupTestDB seeds L1, which intentionally has no webhook and therefore
	// appears only in the all-learner consolidation traversal.
	if want := cardinality + 1; consolidationCount != want {
		t.Fatalf("consolidation learner count: got %d, want %d", consolidationCount, want)
	}
	t.Logf("scanned %d consolidation learners in %d pages of at most %d in %s", consolidationCount, consolidationPages, pageSize, time.Since(consolidationStarted))
}

func insertSchedulerLoadLearners(t *testing.T, ctx context.Context, store *Store, cardinality int) {
	t.Helper()
	tx, err := store.root.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin scheduler load seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	const batchSize = 500
	now := time.Now().UTC()
	for first := 0; first < cardinality; first += batchSize {
		last := first + batchSize
		if last > cardinality {
			last = cardinality
		}
		var query strings.Builder
		query.WriteString(`INSERT INTO learners
			(id, email, password_hash, objective, webhook_url, created_at, email_verified_at) VALUES `)
		args := make([]any, 0, (last-first)*7)
		for i := first; i < last; i++ {
			if i > first {
				query.WriteByte(',')
			}
			query.WriteString("(?, ?, ?, ?, ?, ?, ?)")
			learnerID := fmt.Sprintf("load-%06d", i)
			args = append(args,
				learnerID,
				learnerID+"@example.test",
				"load-test-hash",
				"load-test-objective",
				"https://discord.com/api/webhooks/1/load-test-token",
				now,
				now,
			)
		}
		if _, err := tx.ExecContext(ctx, store.rebind(query.String()), args...); err != nil {
			t.Fatalf("insert scheduler load batch starting at %d: %v", first, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit scheduler load seed: %v", err)
	}
}
