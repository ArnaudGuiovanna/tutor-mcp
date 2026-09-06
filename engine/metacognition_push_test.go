// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/models"
)

// TestEnqueueMirrorWebhook_PersistsAndDedups is the core integration test for
// #59: a real DB receives a row in webhook_message_queue when a mirror message
// is emitted, and a second emission on the same UTC day is a no-op.
func TestEnqueueMirrorWebhook_PersistsAndDedups(t *testing.T) {
	rawDB, store, learnerID := rawTestSetup(t, "")

	mirror := &models.MirrorMessage{
		Pattern:      "hint_overuse",
		Message:      "You often ask for hints on concepts you have mastered.",
		OpenQuestion: "Reflex or unclear?",
	}
	// Keep both emissions inside one UTC civil day regardless of when the test
	// runner happens to execute. Using time.Now().Add(2h) made this test fail
	// after 22:00 UTC even though the production deduplication was correct.
	now := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)

	// First emission: row should land in webhook_message_queue.
	id, enqueued, err := EnqueueMirrorWebhook(context.Background(), store, learnerID, mirror, now)
	if err != nil {
		t.Fatalf("EnqueueMirrorWebhook: %v", err)
	}
	if !enqueued {
		t.Fatal("expected enqueued=true on first emission, got false")
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	// Verify the row exists, has the right kind, and the content carries the
	// mirror text (JSON-encoded so the dispatcher can render it).
	var (
		kind    string
		content string
		status  string
	)
	if err := rawDB.QueryRow(
		`SELECT kind, content, status FROM webhook_message_queue WHERE id = ?`, id,
	).Scan(&kind, &content, &status); err != nil {
		t.Fatalf("scan queue row: %v", err)
	}
	if kind != models.WebhookKindMirror {
		t.Errorf("kind = %q, want %q", kind, models.WebhookKindMirror)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}

	// Content must be JSON containing the mirror text + open question so a
	// downstream consumer can render the full message without re-running
	// detection.
	var payload MirrorWebhookContent
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("content is not JSON: %v (content=%q)", err, content)
	}
	if payload.Message != mirror.Message {
		t.Errorf("payload.Message = %q, want %q", payload.Message, mirror.Message)
	}
	if payload.OpenQuestion != mirror.OpenQuestion {
		t.Errorf("payload.OpenQuestion = %q, want %q", payload.OpenQuestion, mirror.OpenQuestion)
	}
	if payload.Pattern != mirror.Pattern {
		t.Errorf("payload.Pattern = %q, want %q", payload.Pattern, mirror.Pattern)
	}

	// Per-day dedup: a second emission on the same UTC day must be a no-op.
	id2, enqueued2, err := EnqueueMirrorWebhook(context.Background(), store, learnerID, mirror, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("second EnqueueMirrorWebhook: %v", err)
	}
	if enqueued2 {
		t.Errorf("expected enqueued=false on same-day re-emission, got true (id=%d)", id2)
	}

	// Confirm only one row landed in the queue for this learner.
	var queueCount int
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, models.WebhookKindMirror,
	).Scan(&queueCount); err != nil {
		t.Fatalf("count queue rows: %v", err)
	}
	if queueCount != 1 {
		t.Errorf("queue rows = %d, want 1 (dedup must skip second emission)", queueCount)
	}
}

// TestEnqueueMirrorWebhook_NilMirrorIsNoOp guards against accidentally
// inserting empty rows when DetectMirrorPattern returns nil (no pattern).
func TestEnqueueMirrorWebhook_NilMirrorIsNoOp(t *testing.T) {
	rawDB, store, learnerID := rawTestSetup(t, "")

	id, enqueued, err := EnqueueMirrorWebhook(context.Background(), store, learnerID, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("nil mirror should not error, got %v", err)
	}
	if enqueued || id != 0 {
		t.Errorf("nil mirror: enqueued=%v id=%d, want false/0", enqueued, id)
	}

	var queueCount int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM webhook_message_queue`).Scan(&queueCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if queueCount != 0 {
		t.Errorf("queue rows = %d, want 0", queueCount)
	}
}

func TestEnqueueMirrorWebhook_ObservationsRequireGeneratedMessage(t *testing.T) {
	rawDB, store, learnerID := rawTestSetup(t, "")
	mirror := DetectMirrorPattern(MirrorInput{
		SessionCount: 3,
		Interactions: makeInteractions(10, false, false, 0, true, time.Now().UTC()),
	})
	if mirror == nil {
		t.Fatal("expected a descriptive mirror for the in-session tutor")
	}
	body, err := json.Marshal(mirror)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["facts"] == nil || fields["window"] == nil || fields["dialogue_intent"] == nil {
		t.Fatalf("in-session response lacks generation context: %s", body)
	}
	if _, exists := fields["message"]; exists {
		t.Fatal("new mirrors must not ship a static pedagogical message")
	}
	if _, exists := fields["open_question"]; exists {
		t.Fatal("new mirrors must not ship a static pedagogical question")
	}
	id, enqueued, err := EnqueueMirrorWebhook(context.Background(), store, learnerID, mirror, time.Now().UTC())
	if err != nil || enqueued || id != 0 {
		t.Fatalf("observations must wait for generated wording, got id=%d enqueued=%v err=%v", id, enqueued, err)
	}
	var count int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM webhook_message_queue`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("observation-only mirror unexpectedly created %d deliveries", count)
	}
}

// TestSendMirrorMessages_DispatchesQueuedItem covers the scheduler-side end
// of the loop: a mirror enqueued in-session is picked up by the cron tick,
// rendered into a Discord embed, and posted via the webhook.
func TestSendMirrorMessages_DispatchesQueuedItem(t *testing.T) {
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

	mirror := &models.MirrorMessage{
		Pattern:      "no_initiative",
		Message:      "All your sessions were triggered by a notification.",
		OpenQuestion: "Do you prefer system reminders?",
	}
	// Direct queue insertion also remains supported for operator-authored
	// messages; the scheduler records the daily reservation after delivery.
	now := time.Now().UTC()
	body, _ := json.Marshal(MirrorWebhookContent{
		Pattern: mirror.Pattern, Message: mirror.Message, OpenQuestion: mirror.OpenQuestion,
	})
	if _, err := store.EnqueueWebhookMessage(context.Background(),
		learnerID, models.WebhookKindMirror, string(body), now, now.Add(2*time.Hour), 0,
	); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	s := schedulerForTest(store)
	s.sendMirrorMessages()

	if len(bodies) != 1 {
		t.Fatalf("got %d webhook hits, want 1", len(bodies))
	}
	if !contains(bodies[0], "All your sessions") {
		t.Errorf("payload should contain the mirror message, got %s", bodies[0])
	}
	if !contains(bodies[0], "prefer system reminders") {
		t.Errorf("payload should contain the open question, got %s", bodies[0])
	}

	// Queue item must be marked sent.
	var status string
	if err := rawDB.QueryRow(
		`SELECT status FROM webhook_message_queue WHERE kind = ? LIMIT 1`,
		models.WebhookKindMirror,
	).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "sent" {
		t.Errorf("status = %q, want sent", status)
	}

	// A scheduled_alert tagged MirrorAlertKind must be recorded so the next
	// tick today is a no-op.
	sent, err := store.WasAlertSentToday(context.Background(), learnerID, MirrorAlertKind)
	if err != nil {
		t.Fatalf("WasAlertSentToday: %v", err)
	}
	if !sent {
		t.Error("expected MIRROR_MESSAGE alert recorded after dispatch")
	}
}

// TestSendMirrorMessages_DedupSameDay confirms two scheduler ticks in the
// same UTC day post the message at most once, mirroring the OLM /
// daily-motivation dedup contract.
func TestSendMirrorMessages_DedupSameDay(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	_, store, learnerID := rawTestSetup(t, srv.URL)

	now := time.Now().UTC()
	body, _ := json.Marshal(MirrorWebhookContent{
		Pattern: "calibration_drift", Message: "You overestimate your level.", OpenQuestion: "Should we calibrate?",
	})
	if _, err := store.EnqueueWebhookMessage(context.Background(),
		learnerID, models.WebhookKindMirror, string(body), now, now.Add(2*time.Hour), 0,
	); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	s := schedulerForTest(store)
	s.sendMirrorMessages()
	s.sendMirrorMessages()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1 (dedup must skip second tick)", got)
	}
}

// TestEnqueueMirrorWebhook_ThenConcurrentSchedulersPostExactlyOnce verifies
// the official end-to-end path. The enqueue-time daily reservation prevents a
// duplicate enqueue but does not masquerade as delivery: one scheduler claims
// and posts the pending item, while a concurrent scheduler sees no claim.
func TestEnqueueMirrorWebhook_ThenConcurrentSchedulersPostExactlyOnce(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	_, store, learnerID := rawTestSetup(t, srv.URL)

	mirror := &models.MirrorMessage{
		Pattern:      "dependency_increasing",
		Message:      "Your autonomy score has dropped.",
		OpenQuestion: "More guidance?",
	}
	if _, _, err := EnqueueMirrorWebhook(context.Background(), store, learnerID, mirror, time.Now().UTC()); err != nil {
		t.Fatalf("EnqueueMirrorWebhook: %v", err)
	}

	s := schedulerForTest(store)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.sendMirrorMessages()
		}()
	}
	wg.Wait()
	// A later tick must remain a no-op after the processing -> sent transition.
	s.sendMirrorMessages()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want exactly 1", got)
	}
	var status string
	if err := store.RawDB().QueryRow(
		`SELECT status FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, models.WebhookKindMirror,
	).Scan(&status); err != nil {
		t.Fatalf("scan queue status: %v", err)
	}
	if status != models.WebhookStatusSent {
		t.Fatalf("queue status = %q, want %q", status, models.WebhookStatusSent)
	}
}
