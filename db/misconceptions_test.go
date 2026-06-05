package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

var testDBCounter int

func setupTestDB(t *testing.T) *Store {
	t.Helper()
	testDBCounter++
	dsn := fmt.Sprintf("file:memdb_%s_%d?mode=memory&cache=shared", t.Name(), testDBCounter)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES ('L1', 'test@test.com', 'hash', 'test', ?)`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func insertInteraction(t *testing.T, store *Store, concept string, success bool, miscType, miscDetail string, createdAt time.Time) {
	t.Helper()
	succInt := 0
	if success {
		succInt = 1
	}
	_, err := store.db.Exec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, notes, misconception_type, misconception_detail, created_at)
		 VALUES ('L1', ?, 'RECALL_EXERCISE', ?, 60, 0.5, '', ?, ?, ?)`,
		concept, succInt, nullString(miscType), nullString(miscDetail), createdAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// TestGetMisconceptionGroups_Basic inserts 2 "confusion goroutine/thread" and
// 1 "missing sync" misconception on "Goroutines", then verifies group counts
// and last_error_detail.
func TestGetMisconceptionGroups_Basic(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "thought goroutines are OS threads", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "mixed up scheduler", now.Add(-1*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "missing sync", "forgot to use WaitGroup", now.Add(-2*time.Hour))

	groups, err := store.GetMisconceptionGroups(context.Background(), "L1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// First group should be "confusion goroutine/thread" with count=2 (ordered by count DESC)
	if groups[0].MisconceptionType != "confusion goroutine/thread" {
		t.Errorf("expected first group type 'confusion goroutine/thread', got %q", groups[0].MisconceptionType)
	}
	if groups[0].Count != 2 {
		t.Errorf("expected first group count=2, got %d", groups[0].Count)
	}
	if groups[0].LastErrorDetail != "mixed up scheduler" {
		t.Errorf("expected last_error_detail 'mixed up scheduler', got %q", groups[0].LastErrorDetail)
	}

	// Second group should be "missing sync" with count=1
	if groups[1].MisconceptionType != "missing sync" {
		t.Errorf("expected second group type 'missing sync', got %q", groups[1].MisconceptionType)
	}
	if groups[1].Count != 1 {
		t.Errorf("expected second group count=1, got %d", groups[1].Count)
	}
}

// TestMisconceptionStatus_Active verifies that a misconception appearing in the
// last 3 interactions is reported as "active".
func TestMisconceptionStatus_Active(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	// Chronological: fail, success, fail (same misconception)
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "detail1", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-2*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "detail2", now.Add(-1*time.Hour))

	groups, err := store.GetMisconceptionGroups(context.Background(), "L1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Status != "active" {
		t.Errorf("expected status 'active', got %q", groups[0].Status)
	}
}

// TestMisconceptionStatus_Resolved verifies that a misconception NOT appearing
// in the last 3 interactions is reported as "resolved".
func TestMisconceptionStatus_Resolved(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	// Old fail, then 3 successes
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "old detail", now.Add(-4*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-2*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-1*time.Hour))

	groups, err := store.GetMisconceptionGroups(context.Background(), "L1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Status != "resolved" {
		t.Errorf("expected status 'resolved', got %q", groups[0].Status)
	}
}

// TestGetActiveMisconceptions inserts an active misconception on "Goroutines"
// and a resolved one on "Interfaces", then verifies that GetActiveMisconceptions
// for "Goroutines" returns only the active one.
func TestGetActiveMisconceptions(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	// Active misconception on Goroutines (recent fail)
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "recent", now.Add(-1*time.Hour))

	// Resolved misconception on Interfaces (old fail + 3 successes)
	insertInteraction(t, store, "Interfaces", false, "type assertion error", "old", now.Add(-5*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-4*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-2*time.Hour))

	active, err := store.GetActiveMisconceptions(context.Background(), "L1", "Goroutines")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active misconception, got %d", len(active))
	}
	if active[0].MisconceptionType != "confusion goroutine/thread" {
		t.Errorf("expected type 'confusion goroutine/thread', got %q", active[0].MisconceptionType)
	}
	if active[0].Status != "active" {
		t.Errorf("expected status 'active', got %q", active[0].Status)
	}
}

// TestGetDistinctMisconceptionTypes inserts 2 different misconception types and
// 1 success (no misconception), then verifies exactly 2 types are returned.
func TestGetDistinctMisconceptionTypes(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "d1", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "missing sync", "d2", now.Add(-2*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-1*time.Hour)) // no misconception

	types, err := store.GetDistinctMisconceptionTypes(context.Background(), "L1", "Goroutines")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(types))
	}

	// Alphabetical order
	if types[0] != "confusion goroutine/thread" {
		t.Errorf("expected first type 'confusion goroutine/thread', got %q", types[0])
	}
	if types[1] != "missing sync" {
		t.Errorf("expected second type 'missing sync', got %q", types[1])
	}
}

// TestConceptFilter inserts misconceptions on 2 different concepts and verifies
// that filtering to 1 concept returns only that concept's groups.
func TestConceptFilter(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "d1", now.Add(-2*time.Hour))
	insertInteraction(t, store, "Interfaces", false, "type assertion error", "d2", now.Add(-1*time.Hour))

	filter := map[string]bool{"Goroutines": true}
	groups, err := store.GetMisconceptionGroups(context.Background(), "L1", filter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Concept != "Goroutines" {
		t.Errorf("expected concept 'Goroutines', got %q", groups[0].Concept)
	}
}
