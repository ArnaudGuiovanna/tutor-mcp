// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
)

type recordingRoundTripper struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Request:    req,
	}, nil
}

func (r *recordingRoundTripper) snapshot() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies...)
}

func TestSchedulerRechecksConsentDNDAndDailyCap(t *testing.T) {
	allowAnyURL(t)
	_, store, learnerID := rawTestSetup(t, "https://discord.com/api/webhooks/1/test")
	recorder := &recordingRoundTripper{}
	scheduler := schedulerForTest(store)
	scheduler.client = &http.Client{Transport: recorder, Timeout: time.Second}
	now := time.Now().UTC()
	if _, err := store.EnqueueWebhookMessage(context.Background(), learnerID, models.WebhookKindDailyMotivation, "hello", now, now.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}

	a := optedInEngineAvailability(learnerID)
	a.NotificationConsent = false
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	scheduler.sendDailyMotivation()
	if got := len(recorder.snapshot()); got != 0 {
		t.Fatalf("notification sent without consent: %d", got)
	}

	a.NotificationConsent = true
	a.DoNotDisturb = true
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	scheduler.sendDailyMotivation()
	if got := len(recorder.snapshot()); got != 0 {
		t.Fatalf("notification sent during DND: %d", got)
	}

	a.DoNotDisturb = false
	a.MaxNotificationsPerDay = 1
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	scheduler.sendDailyMotivation()
	if got := len(recorder.snapshot()); got != 1 {
		t.Fatalf("consented notification count=%d, want 1", got)
	}
	if _, err := store.EnqueueWebhookMessage(context.Background(), learnerID, models.WebhookKindDailyRecap, "recap", now, now.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	scheduler.sendDailyRecap()
	if got := len(recorder.snapshot()); got != 1 {
		t.Fatalf("daily cap allowed a second notification: %d", got)
	}
}

func TestSchedulerAppliesScreenReaderAndEmojiPreferences(t *testing.T) {
	allowAnyURL(t)
	_, store, learnerID := rawTestSetup(t, "https://discord.com/api/webhooks/1/test")
	a := optedInEngineAvailability(learnerID)
	a.AccessibilityJSON = `{"screen_reader_optimized":true,"avoid_emojis":true}`
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingRoundTripper{}
	scheduler := schedulerForTest(store)
	scheduler.client = &http.Client{Transport: recorder, Timeout: time.Second}
	now := time.Now().UTC()
	if _, err := store.EnqueueWebhookMessage(context.Background(), learnerID,
		models.WebhookKindDailyMotivation, "Keep the focus 🧠", now, now.Add(time.Hour), 1,
	); err != nil {
		t.Fatal(err)
	}
	scheduler.sendDailyMotivation()
	bodies := recorder.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("webhook count=%d, want 1", len(bodies))
	}
	if strings.Contains(string(bodies[0]), "☀") || strings.Contains(string(bodies[0]), "🧠") ||
		!strings.Contains(string(bodies[0]), "Keep the focus") {
		t.Fatalf("emoji-bearing webhook content was not made accessible: %s", bodies[0])
	}
}

func TestSchedulerBlocksUnreviewedHighStakesSuggestion(t *testing.T) {
	allowAnyURL(t)
	_, store, learnerID := rawTestSetup(t, "https://discord.com/api/webhooks/1/test")
	domain, err := store.CreateDomain(context.Background(), learnerID, "Clinical", "", models.KnowledgeSpace{
		Concepts: []string{"triage"}, Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDomainHighStakes(context.Background(), domain.ID, learnerID); err != nil {
		t.Fatal(err)
	}
	brief := models.WebhookBrief{
		Kind: "reminder", DomainID: domain.ID,
		WhyNow: "A case is ready.", LearningGain: "Review safely.",
		OpenLoop: "A decision remains.", NextAction: "Open the tutor when ready.",
	}
	content, err := models.EncodeWebhookBrief(brief)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.EnqueueWebhookMessage(context.Background(), learnerID, "reminder", content, now, now.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingRoundTripper{}
	scheduler := schedulerForTest(store)
	scheduler.client = &http.Client{Transport: recorder, Timeout: time.Second}
	scheduler.dispatchQueued("reminder", "REMINDER", nil)
	if got := len(recorder.snapshot()); got != 0 {
		t.Fatalf("unreviewed high-stakes suggestion was sent: %d", got)
	}
	pending, err := store.GetPendingWebhookMessages(context.Background(), learnerID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("policy-blocked item should remain pending: len=%d err=%v", len(pending), err)
	}
}

func optedInEngineAvailability(learnerID string) *models.Availability {
	return &models.Availability{
		LearnerID: learnerID, Timezone: "UTC", WindowsJSON: "[]",
		AvgDuration: 30, SessionsWeek: 3, NotificationConsent: true,
		NotificationFrequency:  models.NotificationFrequencyAsScheduled,
		MaxNotificationsPerDay: 10, AccessibilityJSON: "{}",
	}
}
