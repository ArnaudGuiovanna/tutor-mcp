// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/models"
)

// TestDispatchMetacognitiveAlerts_EnqueuesAndPostsAffect is the regression
// for sub-issue #58: the scheduler tick must (a) compute metacognitive
// alerts for an active learner, (b) enqueue a webhook_message_queue row,
// (c) drain the queue and POST to the learner's webhook, and (d) stamp
// scheduled_alerts so the next tick is a no-op (per-day dedup).
func TestDispatchMetacognitiveAlerts_EnqueuesAndPostsAffect(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	rawDB, store, learnerID := rawTestSetup(t, srv.URL)

	// Two consecutive low-satisfaction affect rows trip AFFECT_NEGATIVE.
	if err := store.UpsertAffectState(context.Background(), &models.AffectState{
		LearnerID:    learnerID,
		SessionID:    "s1",
		Satisfaction: 2,
	}); err != nil {
		t.Fatalf("upsert affect s1: %v", err)
	}
	if err := store.UpsertAffectState(context.Background(), &models.AffectState{
		LearnerID:    learnerID,
		SessionID:    "s2",
		Satisfaction: 1,
	}); err != nil {
		t.Fatalf("upsert affect s2: %v", err)
	}

	s := schedulerForTest(store)
	s.dispatchMetacognitiveAlerts()

	// (b) webhook_message_queue row was added under metacog_affect.
	var queueCount int
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, "metacog_affect",
	).Scan(&queueCount); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	if queueCount != 1 {
		t.Fatalf("webhook_message_queue rows for metacog_affect = %d, want 1", queueCount)
	}

	// (c) the webhook fired.
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("webhook hits = %d, want 1", got)
	}

	// Queue row marked sent.
	var status string
	if err := rawDB.QueryRow(
		`SELECT status FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, "metacog_affect",
	).Scan(&status); err != nil {
		t.Fatalf("scan status: %v", err)
	}
	if status != "sent" {
		t.Errorf("queue status = %q, want 'sent'", status)
	}

	// (d) per-day dedup: a second tick must not re-enqueue.
	s.dispatchMetacognitiveAlerts()
	var queueCount2 int
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, "metacog_affect",
	).Scan(&queueCount2); err != nil {
		t.Fatalf("count queue 2: %v", err)
	}
	if queueCount2 != 1 {
		t.Errorf("second tick must not re-enqueue, queue count = %d, want 1", queueCount2)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("second tick must not re-fire webhook, hits = %d, want 1", got)
	}
}

// TestDispatchMetacognitiveAlerts_NoOpWithoutTriggerState ensures the job
// doesn't enqueue anything for a learner whose state doesn't trip any of
// the four metacognitive alerts.
func TestDispatchMetacognitiveAlerts_NoOpWithoutTriggerState(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("scheduler must not POST when no metacognitive alert is active")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	_, store, _ := rawTestSetup(t, srv.URL)
	s := schedulerForTest(store)
	s.dispatchMetacognitiveAlerts()
}

// TestDispatchMetacognitiveAlerts_SelectsHighestValueKind ensures that when
// several metacognitive signals fire on the same tick, the planner emits one
// strong learner-facing nudge instead of a bundle of weak Discord messages.
func TestDispatchMetacognitiveAlerts_SelectsHighestValueKind(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	rawDB, store, learnerID := rawTestSetup(t, srv.URL)

	// Trip AFFECT_NEGATIVE.
	if err := store.UpsertAffectState(context.Background(), &models.AffectState{
		LearnerID:    learnerID,
		SessionID:    "s1",
		Satisfaction: 2,
	}); err != nil {
		t.Fatalf("upsert affect s1: %v", err)
	}
	if err := store.UpsertAffectState(context.Background(), &models.AffectState{
		LearnerID:    learnerID,
		SessionID:    "s2",
		Satisfaction: 1,
	}); err != nil {
		t.Fatalf("upsert affect s2: %v", err)
	}

	// Trip CALIBRATION_DIVERGING by inserting completed calibration records
	// with a delta well above 1.5.
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		actual := 0.0
		delta := 2.0
		if _, err := rawDB.Exec(
			`INSERT INTO calibration_records (prediction_id, learner_id, concept_id, predicted, actual, delta, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"p"+string(rune('0'+i)), learnerID, "c", 0.9, actual, delta, now.Add(-time.Duration(i)*time.Minute),
		); err != nil {
			t.Fatalf("insert calibration: %v", err)
		}
	}

	s := schedulerForTest(store)
	s.dispatchMetacognitiveAlerts()

	var calibrationRows, affectRows int
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, "metacog_calibration",
	).Scan(&calibrationRows); err != nil {
		t.Fatalf("count calibration: %v", err)
	}
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, "metacog_affect",
	).Scan(&affectRows); err != nil {
		t.Fatalf("count affect: %v", err)
	}
	if calibrationRows != 1 || affectRows != 0 {
		t.Errorf("queue rows calibration=%d affect=%d, want calibration=1 affect=0", calibrationRows, affectRows)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("webhook hits = %d, want 1", got)
	}
}

// TestMetacogKindToWebhookKind exercises the kind mapping for each of
// the four metacognitive alert types.
func TestMetacogKindToWebhookKind(t *testing.T) {
	cases := []struct {
		in   models.AlertType
		want string
	}{
		{models.AlertDependencyIncreasing, "metacog_dependency"},
		{models.AlertCalibrationDiverging, "metacog_calibration"},
		{models.AlertAffectNegative, "metacog_affect"},
		{models.AlertTransferBlocked, "metacog_transfer"},
		{models.AlertForgetting, ""}, // not a metacog kind → empty string
	}
	for _, tc := range cases {
		if got := metacogKindToWebhookKind(tc.in); got != tc.want {
			t.Errorf("metacogKindToWebhookKind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
