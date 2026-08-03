// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestEnqueueWebhookMessage_ValidationErrors(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	cases := []struct {
		name         string
		kind         string
		content      string
		scheduledFor time.Time
	}{
		{"empty kind", "", "body", now},
		{"empty content", "daily_motivation", "", now},
		{"zero scheduled_for", "daily_motivation", "body", time.Time{}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			id, err := store.EnqueueWebhookMessage(context.Background(), "L1", tc.kind, tc.content, tc.scheduledFor, time.Time{}, 0)
			if err == nil {
				t.Fatalf("expected validation error, got id=%d", id)
			}
		})
	}
}

func TestEnqueueWebhookMessage_PersistsRow(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	expires := now.Add(2 * time.Hour)

	id, err := store.EnqueueWebhookMessage(context.Background(), "L1", "daily_motivation", "hello", now, expires, 5)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id <= 0 {
		t.Fatal("expected positive id")
	}

	// Direct query: row should exist with status='pending', priority=5, content='hello'.
	var (
		kind, content, status string
		priority              int
	)
	if err := store.root.QueryRow(
		rb(store, `SELECT kind, content, priority, status FROM webhook_message_queue WHERE id = ?`), id,
	).Scan(&kind, &content, &priority, &status); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if kind != "daily_motivation" || content != "hello" || priority != 5 || status != "pending" {
		t.Errorf("row mismatch: kind=%q content=%q priority=%d status=%q",
			kind, content, priority, status)
	}

	// Enqueue without expires_at - expires column should be NULL.
	id2, err := store.EnqueueWebhookMessage(context.Background(), "L1", "reminder", "remind", now, time.Time{}, 0)
	if err != nil {
		t.Fatalf("enqueue no expiry: %v", err)
	}
	var expiresAtNullable any
	if err := store.root.QueryRow(
		rb(store, `SELECT expires_at FROM webhook_message_queue WHERE id = ?`), id2,
	).Scan(&expiresAtNullable); err != nil {
		t.Fatalf("scan expires_at: %v", err)
	}
	if expiresAtNullable != nil {
		t.Errorf("expected expires_at NULL, got %v", expiresAtNullable)
	}
}

func TestClaimNextPendingWebhook(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	// Overdue but unexpired: it remains eligible after downtime. Its low
	// priority means current messages still win first.
	if _, err := store.EnqueueWebhookMessage(context.Background(),
		"L1", "daily_motivation", "old", now.Add(-2*time.Hour), time.Time{}, 0,
	); err != nil {
		t.Fatalf("enqueue old: %v", err)
	}
	// In-window, low priority.
	idLow, err := store.EnqueueWebhookMessage(context.Background(),
		"L1", "daily_motivation", "low", now, time.Time{}, 1,
	)
	if err != nil {
		t.Fatalf("enqueue low: %v", err)
	}
	// In-window, high priority -> should win.
	idHigh, err := store.EnqueueWebhookMessage(context.Background(),
		"L1", "daily_motivation", "high", now.Add(5*time.Minute), time.Time{}, 9,
	)
	if err != nil {
		t.Fatalf("enqueue high: %v", err)
	}
	// Different kind, should not match.
	if _, err := store.EnqueueWebhookMessage(context.Background(),
		"L1", "reminder", "skip", now, time.Time{}, 99,
	); err != nil {
		t.Fatalf("enqueue other kind: %v", err)
	}
	// Already-expired pending row in window: should NOT be claimed.
	if _, err := store.EnqueueWebhookMessage(context.Background(),
		"L1", "daily_motivation", "stale", now, now.Add(-1*time.Minute), 99,
	); err != nil {
		t.Fatalf("enqueue stale: %v", err)
	}

	got, err := store.ClaimNextPendingWebhook(context.Background(), "L1", "daily_motivation", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got == nil {
		t.Fatal("expected a pending item, got nil")
	}
	if got.ID != idHigh {
		t.Errorf("expected id=%d (high priority), got id=%d", idHigh, got.ID)
	}
	if got.Content != "high" {
		t.Errorf("content = %q want 'high'", got.Content)
	}
	if got.Priority != 9 {
		t.Errorf("priority = %d want 9", got.Priority)
	}
	if got.Status != "processing" {
		t.Errorf("status = %q want 'processing'", got.Status)
	}
	if got.ClaimedAt == nil {
		t.Error("claimed_at must be set on a claimed item")
	}

	// Mark high-priority as sent; the next claim should pick the low-priority one.
	sentAt := time.Now().UTC()
	if err := store.MarkWebhookSent(context.Background(), idHigh, "L1", sentAt); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	var status string
	var sentAtScan time.Time
	if err := store.root.QueryRow(
		rb(store, `SELECT status, sent_at FROM webhook_message_queue WHERE id = ?`), idHigh,
	).Scan(&status, &sentAtScan); err != nil {
		t.Fatalf("scan after send: %v", err)
	}
	if status != "sent" {
		t.Errorf("status after MarkWebhookSent = %q want 'sent'", status)
	}

	got, err = store.ClaimNextPendingWebhook(context.Background(), "L1", "daily_motivation", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("claim after send: %v", err)
	}
	if got == nil || got.ID != idLow {
		t.Fatalf("expected idLow=%d, got %+v", idLow, got)
	}

	// Empty case for unknown learner returns (nil, nil).
	got, err = store.ClaimNextPendingWebhook(context.Background(), "L-missing", "daily_motivation", now, 30*time.Minute)
	if err != nil {
		t.Errorf("expected nil err on no rows, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil item, got %+v", got)
	}
}

func TestMarkWebhookFailedDeadLettersAtMaxAttempts(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	id, err := store.EnqueueWebhookMessageWithMaxAttempts(context.Background(), "L1", "reminder", "retry", now, time.Time{}, 0, 1)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := store.ClaimNextPendingWebhook(context.Background(), "L1", "reminder", now, time.Minute)
	if err != nil || claimed == nil || claimed.ID != id {
		t.Fatalf("claim before failure: item=%+v err=%v", claimed, err)
	}
	if err := store.MarkWebhookFailed(context.Background(), id, "L1"); err != nil {
		t.Fatalf("MarkWebhookFailed: %v", err)
	}
	var status string
	if err := store.root.QueryRow(
		rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), id,
	).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q want 'failed'", status)
	}
}

func TestMarkWebhookMutatorsRequireLearnerOwnership(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, ?, ?, ?)`),
		"L2", "l2@test.com", "h", "obj", now,
	); err != nil {
		t.Fatal(err)
	}

	idSent, err := store.EnqueueWebhookMessage(context.Background(), "L2", "reminder", "do not send", now, time.Time{}, 0)
	if err != nil {
		t.Fatalf("enqueue sent guard row: %v", err)
	}
	idFailed, err := store.EnqueueWebhookMessage(context.Background(), "L2", "reminder", "do not fail", now, time.Time{}, 0)
	if err != nil {
		t.Fatalf("enqueue failed guard row: %v", err)
	}

	first, err := store.ClaimNextPendingWebhook(context.Background(), "L2", "reminder", now, time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim first L2 row: item=%+v err=%v", first, err)
	}
	second, err := store.ClaimNextPendingWebhook(context.Background(), "L2", "reminder", now, time.Minute)
	if err != nil || second == nil {
		t.Fatalf("claim second L2 row: item=%+v err=%v", second, err)
	}
	if first.ID == second.ID {
		t.Fatal("claim returned the same row twice")
	}

	if err := store.MarkWebhookSent(context.Background(), idSent, "L1", now); err == nil {
		t.Fatal("MarkWebhookSent with wrong learner should fail")
	}
	if err := store.MarkWebhookFailed(context.Background(), idFailed, "L1"); err == nil {
		t.Fatal("MarkWebhookFailed with wrong learner should fail")
	}

	var sentGuardCount int
	if err := store.root.QueryRow(
		rb(store, `SELECT COUNT(*) FROM webhook_message_queue WHERE id = ? AND status = 'processing' AND sent_at IS NULL`),
		idSent,
	).Scan(&sentGuardCount); err != nil {
		t.Fatalf("scan sent guard row: %v", err)
	}
	if sentGuardCount != 1 {
		t.Fatal("MarkWebhookSent changed a row owned by another learner")
	}

	var failedStatus string
	if err := store.root.QueryRow(
		rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), idFailed,
	).Scan(&failedStatus); err != nil {
		t.Fatalf("scan failed guard row: %v", err)
	}
	if failedStatus != "processing" {
		t.Fatalf("MarkWebhookFailed status = %q want 'processing'", failedStatus)
	}
}

func TestExpirePastWebhookMessages(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	// One pending row with expires_at in the past.
	idStale, err := store.EnqueueWebhookMessage(context.Background(),
		"L1", "reminder", "stale", now, now.Add(-5*time.Minute), 0,
	)
	if err != nil {
		t.Fatalf("enqueue stale: %v", err)
	}
	// One pending row with expires_at in the future.
	idFresh, err := store.EnqueueWebhookMessage(context.Background(),
		"L1", "reminder", "fresh", now, now.Add(1*time.Hour), 0,
	)
	if err != nil {
		t.Fatalf("enqueue fresh: %v", err)
	}
	// One pending row with no expires_at (NULL) — should not be expired.
	idNoExp, err := store.EnqueueWebhookMessage(context.Background(),
		"L1", "reminder", "noexp", now, time.Time{}, 0,
	)
	if err != nil {
		t.Fatalf("enqueue noexp: %v", err)
	}

	n, err := store.ExpirePastWebhookMessages(context.Background(), now)
	if err != nil {
		t.Fatalf("ExpirePastWebhookMessages: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row affected, got %d", n)
	}
	var statusStale, statusFresh, statusNoExp string
	if err := store.root.QueryRow(
		rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), idStale,
	).Scan(&statusStale); err != nil {
		t.Fatalf("scan stale: %v", err)
	}
	if err := store.root.QueryRow(
		rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), idFresh,
	).Scan(&statusFresh); err != nil {
		t.Fatalf("scan fresh: %v", err)
	}
	if err := store.root.QueryRow(
		rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), idNoExp,
	).Scan(&statusNoExp); err != nil {
		t.Fatalf("scan noexp: %v", err)
	}
	if statusStale != "expired" {
		t.Errorf("stale status = %q want 'expired'", statusStale)
	}
	if statusFresh != "pending" {
		t.Errorf("fresh status = %q want 'pending'", statusFresh)
	}
	if statusNoExp != "pending" {
		t.Errorf("noexp status = %q want 'pending'", statusNoExp)
	}
}

func TestGetPendingWebhookMessages(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	// Insert three pending rows for L1.
	if _, err := store.EnqueueWebhookMessage(context.Background(), "L1", "k", "a", now.Add(1*time.Hour), time.Time{}, 0); err != nil {
		t.Fatalf("a: %v", err)
	}
	if _, err := store.EnqueueWebhookMessage(context.Background(), "L1", "k", "b", now.Add(2*time.Hour), time.Time{}, 0); err != nil {
		t.Fatalf("b: %v", err)
	}
	idC, err := store.EnqueueWebhookMessage(context.Background(), "L1", "k", "c", now.Add(3*time.Hour), time.Time{}, 99)
	if err != nil {
		t.Fatalf("c: %v", err)
	}
	// Claim and mark one as sent: it should not appear in the pending list.
	claimed, err := store.ClaimNextPendingWebhook(context.Background(), "L1", "k", now, 4*time.Hour)
	if err != nil || claimed == nil || claimed.ID != idC {
		t.Fatalf("claim c: item=%+v err=%v", claimed, err)
	}
	if err := store.MarkWebhookSent(context.Background(), claimed.ID, "L1", now); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	got, err := store.GetPendingWebhookMessages(context.Background(), "L1")
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(got))
	}
	// Ordering is ASC by scheduled_for.
	if got[0].Content != "a" || got[1].Content != "b" {
		t.Errorf("expected order [a,b], got [%s,%s]", got[0].Content, got[1].Content)
	}

	// Other learner: empty list.
	got, _ = store.GetPendingWebhookMessages(context.Background(), "L-other")
	if len(got) != 0 {
		t.Errorf("expected 0 for other learner, got %d", len(got))
	}
}

func TestClaimNextPendingWebhook_ConcurrentSingleWinner(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.EnqueueWebhookMessage(ctx, "L1", "mirror_message", "one", now, time.Time{}, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	const contenders = 12
	start := make(chan struct{})
	results := make(chan *webhookClaimResult, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			item, err := store.ClaimNextPendingWebhook(ctx, "L1", "mirror_message", now, time.Minute)
			results <- &webhookClaimResult{item: item, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent claim: %v", result.err)
		}
		if result.item != nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("successful claims = %d, want exactly 1", winners)
	}
}

type webhookClaimResult struct {
	item *models.WebhookQueueItem
	err  error
}

func TestRequeueStaleWebhookClaims(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	claimedAt := now.Add(-20 * time.Minute)

	idFresh, err := store.EnqueueWebhookMessage(ctx, "L1", "fresh", "retry me", claimedAt, now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	idExpired, err := store.EnqueueWebhookMessage(ctx, "L1", "expired", "too late", claimedAt, now.Add(-time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"fresh", "expired"} {
		item, claimErr := store.ClaimNextPendingWebhook(ctx, "L1", kind, claimedAt, time.Minute)
		if claimErr != nil || item == nil {
			t.Fatalf("claim %s: item=%+v err=%v", kind, item, claimErr)
		}
	}

	n, err := store.RequeueStaleWebhookClaims(ctx, now.Add(-15*time.Minute), now)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 2 {
		t.Fatalf("requeued rows = %d, want 2", n)
	}

	for id, want := range map[int64]string{idFresh: "pending", idExpired: "expired"} {
		var status string
		var claimed any
		if err := store.root.QueryRow(
			rb(store, `SELECT status, claimed_at FROM webhook_message_queue WHERE id = ?`), id,
		).Scan(&status, &claimed); err != nil {
			t.Fatal(err)
		}
		if status != want || claimed != nil {
			t.Errorf("id=%d status=%q claimed_at=%v, want status=%q claimed_at=NULL", id, status, claimed, want)
		}
	}
}

func TestEnqueueWebhookMessageOncePerDay_ConcurrentSingleReservation(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	availability := models.DefaultAvailability("L1")
	availability.NotificationConsent = true
	availability.NotificationFrequency = models.NotificationFrequencyAsScheduled
	if err := store.UpsertAvailability(ctx, availability); err != nil {
		t.Fatalf("enable notification consent: %v", err)
	}

	const contenders = 12
	start := make(chan struct{})
	results := make(chan error, contenders)
	wins := make(chan bool, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, enqueued, err := store.EnqueueWebhookMessageOncePerDay(
				ctx, "L1", "mirror_message", "METACOG_MIRROR", "body",
				now, now.Add(time.Hour), 0,
			)
			results <- err
			wins <- enqueued
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(wins)

	for err := range results {
		if err != nil {
			t.Fatalf("concurrent daily enqueue: %v", err)
		}
	}
	winners := 0
	for won := range wins {
		if won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("daily enqueue winners = %d, want 1", winners)
	}
	for table, want := range map[string]int{"webhook_message_queue": 1, "scheduled_alerts": 1} {
		var count int
		if err := store.root.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("%s rows = %d, want %d", table, count, want)
		}
	}
}
