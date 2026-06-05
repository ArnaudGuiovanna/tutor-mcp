package db

import (
	"context"
	"testing"
	"time"
)

// Seed a concept_state with a given PMastery.
func seedConceptState(t *testing.T, store *Store, concept string, pMastery float64) {
	t.Helper()
	_, err := store.db.Exec(
		`INSERT INTO concept_states (learner_id, concept, p_mastery, card_state, updated_at)
		 VALUES ('L1', ?, ?, 'learning', ?)`,
		concept, pMastery, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func insertSimpleInteraction(t *testing.T, store *Store, concept string, success bool, createdAt time.Time) {
	t.Helper()
	succInt := 0
	if success {
		succInt = 1
	}
	_, err := store.db.Exec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, notes, created_at)
		 VALUES ('L1', ?, 'RECALL_EXERCISE', ?, 60, 0.5, '', ?)`,
		concept, succInt, createdAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// TestImplementationIntentionsInsertAndRecent verifies that an insert is persisted
// and HasRecentImplementationIntention correctly respects the `since` cutoff.
func TestImplementationIntentionsInsertAndRecent(t *testing.T) {
	store := setupTestDB(t)

	_, err := store.InsertImplementationIntention(context.Background(), "L1", "D1", "demain matin", "ferai 1 exercice", time.Time{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Should find it within the last hour.
	has, err := store.HasRecentImplementationIntention(context.Background(), "L1", "D1", time.Now().UTC().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("has recent: %v", err)
	}
	if !has {
		t.Errorf("expected has=true within 1h")
	}

	// Should NOT find it when looking in the future.
	has, _ = store.HasRecentImplementationIntention(context.Background(), "L1", "D1", time.Now().UTC().Add(1*time.Hour))
	if has {
		t.Errorf("expected has=false for future cutoff")
	}

	// Any-domain check works too.
	hasAny, _ := store.HasRecentImplementationIntention(context.Background(), "L1", "", time.Now().UTC().Add(-1*time.Hour))
	if !hasAny {
		t.Errorf("expected any-domain has=true")
	}
}

// TestImplementationIntentionsDomainScope ensures that the domain filter in
// HasRecentImplementationIntention is strict.
func TestImplementationIntentionsDomainScope(t *testing.T) {
	store := setupTestDB(t)

	_, err := store.InsertImplementationIntention(context.Background(), "L1", "D1", "t", "a", time.Time{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// D2 must not see D1's intention.
	has, _ := store.HasRecentImplementationIntention(context.Background(), "L1", "D2", time.Now().UTC().Add(-1*time.Hour))
	if has {
		t.Errorf("expected D2 has=false, intention belongs to D1")
	}
}

// TestWebhookQueueEnqueueDequeueLifecycle covers enqueue, dequeue within the
// scheduling window, and the mark-sent transition (which should prevent redelivery).
func TestWebhookQueueEnqueueDequeueLifecycle(t *testing.T) {
	store := setupTestDB(t)

	now := time.Now().UTC()
	scheduled := now.Add(5 * time.Minute) // within a 30min dispatch window
	id, err := store.EnqueueWebhookMessage(context.Background(), "L1", "daily_motivation", "hello", scheduled, time.Time{}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	// Dequeue with 30min window.
	item, err := store.DequeueNextPending(context.Background(), "L1", "daily_motivation", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if item == nil {
		t.Fatal("expected a queued item, got nil")
	}
	if item.Content != "hello" {
		t.Errorf("expected content 'hello', got %q", item.Content)
	}

	// Mark sent.
	if err := store.MarkWebhookSent(context.Background(), item.ID, "L1", now); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	// Next dequeue should find nothing.
	item2, _ := store.DequeueNextPending(context.Background(), "L1", "daily_motivation", now, 30*time.Minute)
	if item2 != nil {
		t.Errorf("expected nil after mark sent, got id=%d", item2.ID)
	}
}

// TestWebhookQueuePriorityOrdering verifies the highest-priority pending message
// is returned first when multiple messages match the window.
func TestWebhookQueuePriorityOrdering(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	// Lower priority
	_, _ = store.EnqueueWebhookMessage(context.Background(), "L1", "daily_motivation", "low", now, time.Time{}, 0)
	// Higher priority
	_, _ = store.EnqueueWebhookMessage(context.Background(), "L1", "daily_motivation", "high", now, time.Time{}, 5)

	item, _ := store.DequeueNextPending(context.Background(), "L1", "daily_motivation", now, 30*time.Minute)
	if item == nil || item.Content != "high" {
		t.Errorf("expected 'high', got %+v", item)
	}
}

// TestWebhookQueueExpiry checks that a message whose expires_at is in the past
// is filtered out by dequeue, and ExpirePastWebhookMessages transitions it.
func TestWebhookQueueExpiry(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	scheduled := now.Add(-10 * time.Minute)
	expired := now.Add(-1 * time.Minute) // already expired
	_, err := store.EnqueueWebhookMessage(context.Background(), "L1", "daily_recap", "stale", scheduled, expired, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	item, _ := store.DequeueNextPending(context.Background(), "L1", "daily_recap", now, 30*time.Minute)
	if item != nil {
		t.Errorf("expected nil for expired item, got id=%d", item.ID)
	}

	n, err := store.ExpirePastWebhookMessages(context.Background(), now)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 expired row, got %d", n)
	}
}

// TestConceptMasteryDelta — seed a high current mastery + all-failure history
// → delta is positive and concept appears.
func TestConceptMasteryDelta(t *testing.T) {
	store := setupTestDB(t)
	since := time.Now().UTC().Add(-30 * 24 * time.Hour)

	seedConceptState(t, store, "Goroutines", 0.85)
	// 2 old failures before `since` → past mastery estimate ≈ 0
	insertSimpleInteraction(t, store, "Goroutines", false, since.Add(-5*24*time.Hour))
	insertSimpleInteraction(t, store, "Goroutines", false, since.Add(-10*24*time.Hour))

	deltas, err := store.ConceptMasteryDelta(context.Background(), "L1", []string{"Goroutines"}, since, 3)
	if err != nil {
		t.Fatalf("mastery delta: %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("expected 1 delta, got %d", len(deltas))
	}
	if deltas[0].Delta < 0.5 {
		t.Errorf("expected significant delta, got %.2f", deltas[0].Delta)
	}
}

// TestMilestonesInWindow — concept crossed mastery threshold AND has recent
// successful interactions → appears; concept that's mastered but inactive → skipped.
func TestMilestonesInWindow(t *testing.T) {
	store := setupTestDB(t)
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)

	// Active concept — mastered, has recent success
	seedConceptState(t, store, "Goroutines", 0.9)
	insertSimpleInteraction(t, store, "Goroutines", true, time.Now().UTC().Add(-1*time.Hour))

	// Inactive concept — mastered but last interaction was before window
	seedConceptState(t, store, "Interfaces", 0.9)
	insertSimpleInteraction(t, store, "Interfaces", true, since.Add(-5*24*time.Hour))

	// Unmastered concept — should not appear
	seedConceptState(t, store, "Channels", 0.3)
	insertSimpleInteraction(t, store, "Channels", true, time.Now().UTC().Add(-1*time.Hour))

	milestones, err := store.MilestonesInWindow(context.Background(), "L1", []string{"Goroutines", "Interfaces", "Channels"}, since)
	if err != nil {
		t.Fatalf("milestones: %v", err)
	}
	if len(milestones) != 1 {
		t.Fatalf("expected 1 milestone, got %d: %v", len(milestones), milestones)
	}
	if milestones[0] != "Goroutines" {
		t.Errorf("expected 'Goroutines', got %q", milestones[0])
	}
}

// TestCountSessionsOnConcept uses distinct-date heuristic.
func TestCountSessionsOnConcept(t *testing.T) {
	store := setupTestDB(t)

	// Anchor the first two interactions to a stable UTC day (D-2 at 02:00 and 08:00 UTC)
	// so they always fall on the same UTC date regardless of wallclock at run time.
	// The third uses "now-1h", which is on a strictly later UTC date than D-2.
	twoDaysAgoMidnight := time.Now().UTC().Truncate(24 * time.Hour).Add(-2 * 24 * time.Hour)
	insertSimpleInteraction(t, store, "Goroutines", true, twoDaysAgoMidnight.Add(2*time.Hour))
	insertSimpleInteraction(t, store, "Goroutines", false, twoDaysAgoMidnight.Add(8*time.Hour))
	insertSimpleInteraction(t, store, "Goroutines", true, time.Now().UTC().Add(-1*time.Hour))

	count, err := store.CountSessionsOnConcept(context.Background(), "L1", "Goroutines")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 distinct sessions, got %d", count)
	}
}

// TestLastFailureOnConcept_WithinWindow — a fresh failure should be returned;
// an old one (outside window) should not.
func TestLastFailureOnConcept_WithinWindow(t *testing.T) {
	store := setupTestDB(t)

	insertSimpleInteraction(t, store, "Goroutines", false, time.Now().UTC().Add(-2*time.Hour))
	failure, err := store.LastFailureOnConcept(context.Background(), "L1", "Goroutines", 24*time.Hour)
	if err != nil {
		t.Fatalf("last failure: %v", err)
	}
	if failure == nil {
		t.Fatal("expected a failure within 24h")
	}
	if failure.Success {
		t.Errorf("expected success=false, got true")
	}

	// Outside window
	none, _ := store.LastFailureOnConcept(context.Background(), "L1", "Goroutines", 30*time.Minute)
	if none != nil {
		t.Errorf("expected nil outside window, got id=%d", none.ID)
	}
}
