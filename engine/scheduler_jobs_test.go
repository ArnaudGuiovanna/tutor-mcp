// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"

	_ "modernc.org/sqlite"
)

// jobsCounter avoids dsn collisions across this file's tests.
var jobsCounter int32

// newJobsTestStore opens an isolated in-memory sqlite DB, runs migrations,
// inserts a learner with a (caller-supplied) webhook URL, and returns the
// store + learner ID.
func newJobsTestStore(t *testing.T, webhookURL string) (*db.Store, string) {
	t.Helper()
	id := atomic.AddInt32(&jobsCounter, 1)
	dsn := fmt.Sprintf("file:eng_jobs_%s_%d?mode=memory&cache=shared", t.Name(), id)
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { rawDB.Close() })
	if err := db.Migrate(rawDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	learnerID := "L1"
	_, err = rawDB.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, webhook_url, created_at, last_active)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		learnerID, "test@example.com", "hash", "obj", webhookURL,
		time.Now().UTC(), time.Now().UTC().Add(-48*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert learner: %v", err)
	}
	store := db.NewStore(rawDB)
	optInSchedulerNotifications(t, store, learnerID)
	return store, learnerID
}

// rawTestSetup returns the raw *sql.DB alongside the *db.Store, so tests can
// insert fixtures directly without going through Store helpers.
func rawTestSetup(t *testing.T, webhookURL string) (*sql.DB, *db.Store, string) {
	t.Helper()
	id := atomic.AddInt32(&jobsCounter, 1)
	dsn := fmt.Sprintf("file:eng_jobs_%s_%d?mode=memory&cache=shared", t.Name(), id)
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { rawDB.Close() })
	if err := db.Migrate(rawDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	learnerID := "L1"
	_, err = rawDB.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, webhook_url, created_at, last_active)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		learnerID, "test@example.com", "hash", "obj", webhookURL,
		time.Now().UTC(), time.Now().UTC().Add(-48*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert learner: %v", err)
	}
	store := db.NewStore(rawDB)
	optInSchedulerNotifications(t, store, learnerID)
	return rawDB, store, learnerID
}

func optInSchedulerNotifications(t *testing.T, store *db.Store, learnerID string) {
	t.Helper()
	if err := store.UpsertAvailability(context.Background(), &models.Availability{
		LearnerID:              learnerID,
		Timezone:               "UTC",
		WindowsJSON:            "[]",
		AvgDuration:            30,
		SessionsWeek:           3,
		NotificationConsent:    true,
		NotificationFrequency:  models.NotificationFrequencyAsScheduled,
		MaxNotificationsPerDay: 10,
		AccessibilityJSON:      "{}",
	}); err != nil {
		t.Fatalf("opt in scheduler notifications: %v", err)
	}
}

// schedulerForTest builds a Scheduler using the given store and an http client
// with a tight timeout.
func schedulerForTest(store *db.Store) *Scheduler {
	return &Scheduler{
		store:  store,
		logger: quietLogger(),
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// ─── Start / Stop ───────────────────────────────────────────────────────────

func TestStart_RegistersJobsAndStops(t *testing.T) {
	store, _ := newJobsTestStore(t, "")
	s := NewScheduler(store, quietLogger())
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Ensure cron entries were added (8 jobs: olm, consolidation,
	// consolidation timeout, motivation, recap, mirror, cleanup, metacog).
	if got := len(s.cron.Entries()); got != 8 {
		t.Errorf("registered jobs = %d, want 8", got)
	}
	s.Stop() // must not panic, must complete promptly
}

// ─── cleanupExpiredData ─────────────────────────────────────────────────────

func TestCleanupExpiredData_DeletesExpiredAndExpiresQueue(t *testing.T) {
	rawDB, store, learnerID := rawTestSetup(t, "")

	// Expired oauth code (must be deleted).
	_, err := rawDB.Exec(
		`INSERT INTO oauth_codes (code, learner_id, code_challenge, client_id, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"expired-code", learnerID, "ch", "cid",
		time.Now().UTC().Add(-1*time.Hour), time.Now().UTC().Add(-2*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert oauth_code: %v", err)
	}

	// Fresh oauth code (must be kept).
	_, err = rawDB.Exec(
		`INSERT INTO oauth_codes (code, learner_id, code_challenge, client_id, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"fresh-code", learnerID, "ch", "cid",
		time.Now().UTC().Add(10*time.Minute), time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert fresh oauth_code: %v", err)
	}

	// Expired refresh token (must be deleted).
	_, err = rawDB.Exec(
		`INSERT INTO refresh_tokens (token, learner_id, expires_at, created_at)
		 VALUES (?, ?, ?, ?)`,
		"old-token", learnerID,
		time.Now().UTC().Add(-1*time.Hour), time.Now().UTC().Add(-2*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert refresh_token: %v", err)
	}

	// Pending webhook message with expires_at in the past (must be marked expired).
	_, err = rawDB.Exec(
		`INSERT INTO webhook_message_queue (learner_id, kind, scheduled_for, expires_at, content, priority, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
		learnerID, "daily_motivation",
		time.Now().UTC().Add(-1*time.Hour),
		time.Now().UTC().Add(-30*time.Minute),
		"old", 0, time.Now().UTC().Add(-2*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert webhook queue: %v", err)
	}

	s := schedulerForTest(store)
	s.cleanupExpiredData()

	// Verify oauth_codes: only 'fresh-code' remains.
	var codeCount int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM oauth_codes`).Scan(&codeCount); err != nil {
		t.Fatalf("count codes: %v", err)
	}
	if codeCount != 1 {
		t.Errorf("oauth_codes count = %d, want 1 (fresh only)", codeCount)
	}

	// Verify refresh_tokens: zero remain.
	var tokenCount int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM refresh_tokens`).Scan(&tokenCount); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokenCount != 0 {
		t.Errorf("refresh_tokens count = %d, want 0", tokenCount)
	}

	// Verify webhook queue: pending → expired.
	var status string
	if err := rawDB.QueryRow(`SELECT status FROM webhook_message_queue LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("scan webhook status: %v", err)
	}
	if status != "expired" {
		t.Errorf("webhook status = %q, want 'expired'", status)
	}
}

// TestCleanupExpiredData_NoOpOnEmpty makes sure we hit the "nothing to do"
// branch (cleanup must not error on empty tables).
func TestCleanupExpiredData_NoOpOnEmpty(t *testing.T) {
	_, store, _ := rawTestSetup(t, "")
	s := schedulerForTest(store)
	s.cleanupExpiredData() // should not panic
}

// ─── dispatchQueued / sendDailyMotivation / sendDailyRecap ──────────────────

func TestDispatchQueued_UsesQueuedItem(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, b[:n])
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	rawDB, store, learnerID := rawTestSetup(t, srv.URL)

	now := time.Now().UTC()
	if _, err := store.EnqueueWebhookMessage(context.Background(),
		learnerID, "daily_motivation", "tu peux le faire", now, now.Add(2*time.Hour), 5,
	); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	s := schedulerForTest(store)
	s.sendDailyMotivation()

	if len(bodies) != 1 {
		t.Fatalf("got %d webhook hits, want 1", len(bodies))
	}
	if want := "tu peux le faire"; !contains(bodies[0], want) {
		t.Errorf("payload %s should contain %q", bodies[0], want)
	}

	// queue item must be marked sent.
	var status string
	if err := rawDB.QueryRow(`SELECT status FROM webhook_message_queue LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "sent" {
		t.Errorf("queue status = %q, want 'sent'", status)
	}
}

func TestDispatchQueued_FallbackWhenQueueEmpty(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, b[:n])
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	_, store, _ := rawTestSetup(t, srv.URL)
	s := schedulerForTest(store)
	s.sendDailyRecap()

	if len(bodies) != 1 {
		t.Fatalf("got %d webhook hits, want 1 (fallback)", len(bodies))
	}
	if !contains(bodies[0], "Tonight") {
		t.Errorf("fallback payload should mention 'Tonight', got %s", bodies[0])
	}
}

func TestDispatchQueued_DedupSameDay(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	_, store, _ := rawTestSetup(t, srv.URL)
	s := schedulerForTest(store)
	s.sendDailyMotivation()
	s.sendDailyMotivation()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1 (dedup must skip second call)", got)
	}
}

func TestDispatchQueued_RequeuesAfterWebhookError(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	// Always-500 server. A durable claim performs exactly one HTTP request; the
	// queue item then remains pending until its persisted backoff is due.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	rawDB, store, learnerID := rawTestSetup(t, srv.URL)
	now := time.Now().UTC()
	if _, err := store.EnqueueWebhookMessage(context.Background(),
		learnerID, "daily_motivation", "msg", now, now.Add(2*time.Hour), 5,
	); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	s := schedulerForTest(store)
	s.sendDailyMotivation()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("HTTP attempts in one durable claim = %d, want 1", got)
	}

	var (
		status         string
		attemptCount   int
		nextAttemptAt  sql.NullTime
		lastError      string
		deadLetteredAt sql.NullTime
	)
	if err := rawDB.QueryRow(
		`SELECT status, attempt_count, next_attempt_at, last_error, dead_lettered_at
		 FROM webhook_message_queue LIMIT 1`,
	).Scan(&status, &attemptCount, &nextAttemptAt, &lastError, &deadLetteredAt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != models.WebhookStatusPending {
		t.Errorf("queue status = %q, want %q", status, models.WebhookStatusPending)
	}
	if attemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", attemptCount)
	}
	if !nextAttemptAt.Valid || !nextAttemptAt.Time.After(now) {
		t.Errorf("next_attempt_at = %v, want a persisted retry after %v", nextAttemptAt, now)
	}
	if lastError != "delivery_failed" {
		t.Errorf("last_error = %q, want delivery_failed", lastError)
	}
	if deadLetteredAt.Valid {
		t.Errorf("dead_lettered_at = %v, want NULL before durable retry budget is exhausted", deadLetteredAt.Time)
	}
}

func TestDispatchQueued_NoActiveLearners(t *testing.T) {
	// Learner without webhook_url is filtered by GetActiveLearners → no panic,
	// no hits.
	_, store, _ := rawTestSetup(t, "")
	s := schedulerForTest(store)
	s.sendDailyMotivation()
	s.sendDailyRecap()
}

// contains is a small helper to keep test assertions readable without pulling
// in extra deps.
func contains(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	hs := string(haystack)
	for i := 0; i+len(needle) <= len(hs); i++ {
		if hs[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
