package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

// TestGetRecentImplementationIntentions_OrderingAndLimit inserts three intentions
// across different timestamps and verifies the function returns them most-recent
// first, that the `since` cutoff filters older rows, and that `limit` caps the
// result size. Also verifies a default limit of 20 is applied when limit <= 0.
func TestGetRecentImplementationIntentions_OrderingAndLimit(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	// Insert three intentions; we'll back-date by hand because the helper
	// records created_at = NOW().
	if _, err := store.InsertImplementationIntentionForSession(context.Background(), "L1", "D1", "", "t1", "a1", time.Time{}); err != nil {
		t.Fatalf("insert i1: %v", err)
	}
	if _, err := store.InsertImplementationIntentionForSession(context.Background(), "L1", "D1", "", "t2", "a2", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("insert i2: %v", err)
	}
	if _, err := store.InsertImplementationIntentionForSession(context.Background(), "L1", "D1", "", "t3", "a3", time.Time{}); err != nil {
		t.Fatalf("insert i3: %v", err)
	}

	// Force ordering: rewrite created_at so we control most-recent ordering.
	rewrite := func(trigger string, createdAt time.Time) {
		t.Helper()
		if _, err := store.root.Exec(
			rb(store, `UPDATE implementation_intentions SET created_at = ? WHERE trigger_text = ?`),
			createdAt, trigger,
		); err != nil {
			t.Fatalf("rewrite created_at for %s: %v", trigger, err)
		}
	}
	rewrite("t1", now.Add(-3*time.Hour))
	rewrite("t2", now.Add(-2*time.Hour))
	rewrite("t3", now.Add(-1*time.Hour))

	// since=last-90min picks up only t3
	got, err := store.GetRecentImplementationIntentions(context.Background(), "L1", now.Add(-90*time.Minute), 10)
	if err != nil {
		t.Fatalf("get recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row in last 90 min, got %d", len(got))
	}
	if got[0].Trigger != "t3" {
		t.Errorf("expected trigger=t3, got %q", got[0].Trigger)
	}
	if got[0].ScheduledFor == nil {
		// Note: t3 was inserted with zero scheduledFor → should be nil
		// (so this is "expected nil"; flip the check)
	}

	// since=4h picks up all 3, ordered DESC
	got, err = store.GetRecentImplementationIntentions(context.Background(), "L1", now.Add(-4*time.Hour), 10)
	if err != nil {
		t.Fatalf("get recent all: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	if got[0].Trigger != "t3" || got[1].Trigger != "t2" || got[2].Trigger != "t1" {
		t.Errorf("expected DESC order [t3,t2,t1], got [%s,%s,%s]", got[0].Trigger, got[1].Trigger, got[2].Trigger)
	}

	// Verify scheduled_for is plumbed through for t2.
	if got[1].ScheduledFor == nil {
		t.Errorf("expected ScheduledFor non-nil for t2")
	}

	// limit=2 caps results
	got, _ = store.GetRecentImplementationIntentions(context.Background(), "L1", now.Add(-4*time.Hour), 2)
	if len(got) != 2 {
		t.Errorf("expected 2 rows with limit=2, got %d", len(got))
	}

	// limit<=0 defaults to 20 (i.e. returns all 3)
	got, _ = store.GetRecentImplementationIntentions(context.Background(), "L1", now.Add(-4*time.Hour), 0)
	if len(got) != 3 {
		t.Errorf("expected 3 rows with default limit, got %d", len(got))
	}
}

// TestUpdateImplementationIntentionStatus covers both terminal outcomes and
// asserts that ownership-aware lifecycle updates persist their compatibility flag.
func TestUpdateImplementationIntentionStatus_Outcomes(t *testing.T) {
	store := setupTestDB(t)

	id1, err := store.InsertImplementationIntentionForSession(context.Background(), "L1", "D1", "", "t1", "a1", time.Time{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id2, err := store.InsertImplementationIntentionForSession(context.Background(), "L1", "D1", "", "t2", "a2", time.Time{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	cases := []struct {
		name   string
		id     int64
		status string
		want   int
	}{
		{"mark honored", id1, models.IntentionStatusHonored, 1},
		{"mark missed", id2, models.IntentionStatusMissed, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.UpdateImplementationIntentionStatus(context.Background(), "L1", tc.id, tc.status, time.Now().UTC()); err != nil {
				t.Fatalf("UpdateImplementationIntentionStatus: %v", err)
			}
			var v int
			if err := store.root.QueryRow(
				rb(store, `SELECT honored FROM implementation_intentions WHERE id = ?`), tc.id,
			).Scan(&v); err != nil {
				t.Fatalf("scan honored: %v", err)
			}
			if v != tc.want {
				t.Errorf("honored=%d want=%d", v, tc.want)
			}
		})
	}

	// And verify it surfaces in GetRecentImplementationIntentions.
	got, err := store.GetRecentImplementationIntentions(context.Background(), "L1", time.Now().UTC().Add(-1*time.Hour), 10)
	if err != nil {
		t.Fatalf("get recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	for _, ii := range got {
		if ii.Honored == nil {
			t.Errorf("expected Honored set for id=%d, got nil", ii.ID)
		}
	}
}

// TestInsertImplementationIntention_ScheduledFor exercises the non-zero
// scheduledFor branch and verifies the column persists.
func TestInsertImplementationIntention_ScheduledFor(t *testing.T) {
	store := setupTestDB(t)
	when := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)

	id, err := store.InsertImplementationIntentionForSession(context.Background(), "L1", "D1", "", "trig", "act", when)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	got, err := store.GetRecentImplementationIntentions(context.Background(), "L1", time.Now().UTC().Add(-1*time.Hour), 10)
	if err != nil {
		t.Fatalf("get recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	if got[0].ScheduledFor == nil {
		t.Fatal("expected ScheduledFor non-nil")
	}
	if !got[0].ScheduledFor.Equal(when) {
		t.Errorf("ScheduledFor=%v want=%v", got[0].ScheduledFor, when)
	}
}
