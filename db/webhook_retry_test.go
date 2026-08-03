// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestWebhookRetryBackoffAndDeadLetterLifecycle(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	id, err := store.EnqueueWebhookMessageWithMaxAttempts(
		ctx, "L1", "retry-kind", "sensitive payload", now, now.Add(24*time.Hour), 3, 3,
	)
	if err != nil {
		t.Fatal(err)
	}

	first := claimWebhookAt(t, store, id, "retry-kind", now)
	if first.AttemptCount != 1 || first.MaxAttempts != 3 {
		t.Fatalf("first claim counters = %d/%d, want 1/3", first.AttemptCount, first.MaxAttempts)
	}
	dead, err := store.RecordWebhookFailure(ctx, id, "L1", "http_503", now)
	if err != nil || dead {
		t.Fatalf("first failure dead=%v err=%v", dead, err)
	}
	assertWebhookRetryState(t, store, id, models.WebhookStatusPending, 1, now.Add(time.Minute), "http_503", false)

	if got, err := store.ClaimNextPendingWebhook(ctx, "L1", "retry-kind", now.Add(59*time.Second), time.Minute); err != nil || got != nil {
		t.Fatalf("claimed before durable backoff elapsed: item=%+v err=%v", got, err)
	}
	second := claimWebhookAt(t, store, id, "retry-kind", now.Add(time.Minute))
	if second.AttemptCount != 2 {
		t.Fatalf("second claim attempt_count=%d, want 2", second.AttemptCount)
	}
	dead, err = store.RecordWebhookFailure(ctx, id, "L1", "http_503", now.Add(time.Minute))
	if err != nil || dead {
		t.Fatalf("second failure dead=%v err=%v", dead, err)
	}
	secondDue := now.Add(time.Minute + 5*time.Minute)
	assertWebhookRetryState(t, store, id, models.WebhookStatusPending, 2, secondDue, "http_503", false)

	third := claimWebhookAt(t, store, id, "retry-kind", secondDue)
	if third.AttemptCount != 3 {
		t.Fatalf("third claim attempt_count=%d, want 3", third.AttemptCount)
	}
	dead, err = store.RecordWebhookFailure(ctx, id, "L1", "permanent_503", secondDue)
	if err != nil || !dead {
		t.Fatalf("terminal failure dead=%v err=%v", dead, err)
	}
	assertWebhookRetryState(t, store, id, models.WebhookStatusFailed, 3, time.Time{}, "permanent_503", true)

	dlq, err := store.GetDeadLetterWebhookMessages(ctx, "L1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dlq) != 1 || dlq[0].ID != id || dlq[0].DeadLetteredAt == nil || dlq[0].Content != "sensitive payload" {
		t.Fatalf("dead-letter queue = %+v", dlq)
	}
	if got, err := store.ClaimNextPendingWebhook(ctx, "L1", "retry-kind", secondDue.Add(24*time.Hour), 24*time.Hour); err != nil || got != nil {
		t.Fatalf("dead-letter row became claimable: item=%+v err=%v", got, err)
	}
}

func TestReleaseWebhookClaimDoesNotConsumeAttemptBudget(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	id, err := store.EnqueueWebhookMessageWithMaxAttempts(ctx, "L1", "release", "body", now, time.Time{}, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	claimWebhookAt(t, store, id, "release", now)
	if err := store.ReleaseWebhookClaim(ctx, id, "L1"); err != nil {
		t.Fatal(err)
	}

	pending, err := store.GetPendingWebhookMessages(ctx, "L1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].AttemptCount != 0 {
		t.Fatalf("released row = %+v, want attempt_count=0", pending)
	}
	again := claimWebhookAt(t, store, id, "release", now)
	if again.AttemptCount != 1 {
		t.Fatalf("reclaimed attempt_count=%d, want 1", again.AttemptCount)
	}
}

func TestClaimWebhookCatchesUpOverdueUnexpiredFirstAttempt(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	overdueID, err := store.EnqueueWebhookMessage(ctx, "L1", "catch-up", "overdue", now.Add(-24*time.Hour), time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueWebhookMessage(ctx, "L1", "expired-catch-up", "expired", now.Add(-24*time.Hour), now.Add(-time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueWebhookMessage(ctx, "L1", "future-catch-up", "future", now.Add(2*time.Hour), time.Time{}, 0); err != nil {
		t.Fatal(err)
	}

	item := claimWebhookAt(t, store, overdueID, "catch-up", now)
	if item.ScheduledFor.After(now.Add(-23 * time.Hour)) {
		t.Fatalf("claimed item was not the overdue row: %+v", item)
	}
	if got, err := store.ClaimNextPendingWebhook(ctx, "L1", "expired-catch-up", now, 30*time.Minute); err != nil || got != nil {
		t.Fatalf("expired overdue row claimed: item=%+v err=%v", got, err)
	}
	if got, err := store.ClaimNextPendingWebhook(ctx, "L1", "future-catch-up", now, 30*time.Minute); err != nil || got != nil {
		t.Fatalf("future row beyond look-ahead claimed: item=%+v err=%v", got, err)
	}
}

func TestRequeueStaleWebhookClaimUsesRetryBudget(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	claimedAt := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)
	now := claimedAt.Add(20 * time.Minute)
	id, err := store.EnqueueWebhookMessageWithMaxAttempts(ctx, "L1", "stale-max", "body", claimedAt, time.Time{}, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	claimWebhookAt(t, store, id, "stale-max", claimedAt)

	count, err := store.RequeueStaleWebhookClaims(ctx, now.Add(-15*time.Minute), now)
	if err != nil || count != 1 {
		t.Fatalf("recover stale count=%d err=%v", count, err)
	}
	assertWebhookRetryState(t, store, id, models.WebhookStatusFailed, 1, time.Time{}, "delivery_claim_timed_out", true)
}

func TestWebhookRetryPolicyValidationAndReasonBound(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, maxAttempts := range []int{0, maxWebhookMaxAttempts + 1} {
		if _, err := store.EnqueueWebhookMessageWithMaxAttempts(ctx, "L1", "kind", "body", now, time.Time{}, 0, maxAttempts); err == nil {
			t.Fatalf("max_attempts=%d accepted", maxAttempts)
		}
	}
	id, err := store.EnqueueWebhookMessageWithMaxAttempts(ctx, "L1", "kind", "body", now, time.Time{}, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	claimWebhookAt(t, store, id, "kind", now)
	longReason := strings.Repeat("secret", maxWebhookFailureCodeLen)
	if _, err := store.RecordWebhookFailure(ctx, id, "L1", longReason, now); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := store.root.QueryRow(
		rb(store, `SELECT last_error FROM webhook_message_queue WHERE id = ?`), id,
	).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "delivery_failed" {
		t.Fatalf("unsafe reason persisted as %q, want generic code", reason)
	}
}

func claimWebhookAt(t *testing.T, store *Store, id int64, kind string, at time.Time) *models.WebhookQueueItem {
	t.Helper()
	item, err := store.ClaimNextPendingWebhook(context.Background(), "L1", kind, at, time.Minute)
	if err != nil {
		t.Fatalf("claim %s at %s: %v", kind, at, err)
	}
	if item == nil || item.ID != id {
		t.Fatalf("claim %s at %s: item=%+v, want id=%d", kind, at, item, id)
	}
	return item
}

func assertWebhookRetryState(t *testing.T, store *Store, id int64, wantStatus string, wantAttempts int, wantNext time.Time, wantError string, wantDeadLetter bool) {
	t.Helper()
	var status, lastError string
	var nextAttempt, deadLettered sql.NullTime
	var attempts int
	if err := store.root.QueryRow(
		rb(store, `SELECT status, attempt_count, next_attempt_at, last_error, dead_lettered_at
			FROM webhook_message_queue WHERE id = ?`), id,
	).Scan(&status, &attempts, &nextAttempt, &lastError, &deadLettered); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || attempts != wantAttempts || lastError != wantError {
		t.Fatalf("retry state status=%q attempts=%d error=%q, want %q/%d/%q",
			status, attempts, lastError, wantStatus, wantAttempts, wantError)
	}
	if wantNext.IsZero() {
		if nextAttempt.Valid {
			t.Fatalf("next_attempt_at=%v, want NULL", nextAttempt.Time)
		}
	} else if !nextAttempt.Valid || !nextAttempt.Time.Equal(wantNext) {
		t.Fatalf("next_attempt_at=%v valid=%v, want %v", nextAttempt.Time, nextAttempt.Valid, wantNext)
	}
	if deadLettered.Valid != wantDeadLetter {
		t.Fatalf("dead_lettered_at valid=%v, want %v", deadLettered.Valid, wantDeadLetter)
	}
}
