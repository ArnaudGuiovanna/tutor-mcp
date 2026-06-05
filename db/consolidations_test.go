// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
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
	if err := store.MarkConsolidationsDelivered(context.Background(), "L1", []int64{pending[0].ID}, deliveredAt); err != nil {
		t.Fatalf("MarkConsolidationsDelivered: %v", err)
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
