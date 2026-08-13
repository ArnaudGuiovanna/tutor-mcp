// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPendingConsolidationLifecycle(t *testing.T) {
	store := setupTestDB(t)
	now := time.Date(2026, time.May, 3, 13, 30, 0, 0, time.UTC)

	if err := store.UpsertPendingConsolidation(context.Background(), "L1", "monthly", "2026-04", now); err != nil {
		t.Fatalf("UpsertPendingConsolidation: %v", err)
	}
	if err := store.UpsertPendingConsolidation(context.Background(), "L1", "monthly", "2026-04", now.Add(time.Minute)); err != nil {
		t.Fatalf("UpsertPendingConsolidation duplicate: %v", err)
	}
	pending, err := store.GetPendingConsolidations(context.Background(), "L1")
	if err != nil {
		t.Fatalf("GetPendingConsolidations: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}

	deliveredAt := now.Add(5 * time.Minute)
	claimed, err := store.ClaimPendingConsolidations(context.Background(), "L1", deliveredAt)
	if err != nil {
		t.Fatalf("ClaimPendingConsolidations: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != pending[0].ID || claimed[0].Status != "delivered" {
		t.Fatalf("unexpected claim: %#v", claimed)
	}
	claimedAgain, err := store.ClaimPendingConsolidations(context.Background(), "L1", deliveredAt)
	if err != nil || len(claimedAgain) != 0 {
		t.Fatalf("second claim = %#v, err=%v; want empty", claimedAgain, err)
	}
	item, err := store.GetConsolidation(context.Background(), "L1", "monthly", "2026-04")
	if err != nil {
		t.Fatalf("GetConsolidation: %v", err)
	}
	if item.Status != "delivered" || item.DeliveredAt == nil {
		t.Fatalf("unexpected delivered item: %#v", item)
	}
	pending, _ = store.GetPendingConsolidations(context.Background(), "L1")
	if len(pending) != 0 {
		t.Fatalf("delivered item should not be pending: %#v", pending)
	}

	requeued, err := store.RequeueStaleDeliveredConsolidations(context.Background(), deliveredAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("RequeueStaleDeliveredConsolidations: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("requeued = %d, want 1", requeued)
	}
	pending, _ = store.GetPendingConsolidations(context.Background(), "L1")
	if len(pending) != 1 {
		t.Fatalf("requeued pending = %d, want 1", len(pending))
	}

	if err := store.MarkConsolidationCompleted(context.Background(), "L1", "monthly", "2026-04", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("MarkConsolidationCompleted: %v", err)
	}
	item, err = store.GetConsolidation(context.Background(), "L1", "monthly", "2026-04")
	if err != nil {
		t.Fatalf("GetConsolidation completed: %v", err)
	}
	if item.Status != "completed" || item.CompletedAt == nil {
		t.Fatalf("unexpected completed item: %#v", item)
	}
	if err := store.UpsertPendingConsolidation(context.Background(), "L1", "monthly", "2026-04", now.Add(time.Hour)); err != nil {
		t.Fatalf("UpsertPendingConsolidation after completed: %v", err)
	}
	pending, _ = store.GetPendingConsolidations(context.Background(), "L1")
	if len(pending) != 0 {
		t.Fatalf("completed item should stay completed: %#v", pending)
	}
}

func TestReleaseConsolidationClaims(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, key := range []string{"2026-03", "2026-04"} {
		if err := store.UpsertPendingConsolidation(ctx, "L1", "monthly", key, now); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := store.ClaimPendingConsolidations(ctx, "L1", now)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim: %#v err=%v", claimed, err)
	}
	if err := store.ReleaseConsolidationClaims(ctx, "L1", []int64{claimed[0].ID, claimed[1].ID}); err != nil {
		t.Fatalf("release: %v", err)
	}
	reclaimed, err := store.ClaimPendingConsolidations(ctx, "L1", now.Add(time.Minute))
	if err != nil || len(reclaimed) != 2 {
		t.Fatalf("reclaim: %#v err=%v", reclaimed, err)
	}
}

func TestClaimPendingConsolidations_ConcurrentSingleDelivery(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, key := range []string{"2026-02", "2026-03", "2026-04"} {
		if err := store.UpsertPendingConsolidation(ctx, "L1", "monthly", key, now); err != nil {
			t.Fatal(err)
		}
	}

	const contenders = 12
	start := make(chan struct{})
	counts := make(chan int, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			items, err := store.ClaimPendingConsolidations(ctx, "L1", now)
			counts <- len(items)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(counts)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent claim: %v", err)
		}
	}
	total := 0
	winners := 0
	for count := range counts {
		total += count
		if count > 0 {
			winners++
		}
	}
	if total != 3 || winners != 1 {
		t.Fatalf("claimed jobs total=%d winners=%d, want total=3 winners=1", total, winners)
	}
}

func TestListLearnerIDsForConsolidationPageIncludesLearnersWithoutWebhook(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO learners (id, email, password_hash, objective, webhook_url, created_at) VALUES (?, ?, ?, ?, ?, ?)`),
		"L2", "l2@test.com", "h", "obj", "", now,
	); err != nil {
		t.Fatal(err)
	}
	ids, err := store.ListLearnerIDsForConsolidationPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "L1" || ids[1] != "L2" {
		t.Fatalf("learner IDs = %#v, want [L1 L2]", ids)
	}
}
