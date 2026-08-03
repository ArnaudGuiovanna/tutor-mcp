// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func optInQueueNotifications(t *testing.T, store interface {
	UpsertAvailability(context.Context, *models.Availability) error
}) {
	t.Helper()
	if err := store.UpsertAvailability(context.Background(), &models.Availability{
		LearnerID:              "L_owner",
		Timezone:               "UTC",
		WindowsJSON:            "[]",
		AvgDuration:            30,
		SessionsWeek:           3,
		NotificationConsent:    true,
		NotificationFrequency:  models.NotificationFrequencyAsScheduled,
		MaxNotificationsPerDay: 10,
		AccessibilityJSON:      "{}",
	}); err != nil {
		t.Fatalf("opt in notifications: %v", err)
	}
}

func TestQueueWebhookMessage_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerQueueWebhookMessage, "", "queue_webhook_message", map[string]any{
		"kind":          "daily_recap",
		"scheduled_for": "2026-05-02T08:00:00Z",
		"content":       "hi",
	})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestQueueWebhookMessage_InvalidKind(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "spam",
		"scheduled_for": "2026-05-02T08:00:00Z",
		"content":       "hi",
	})
	if !res.IsError || !strings.Contains(resultText(res), "invalid kind") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestQueueWebhookMessage_MissingScheduledFor(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "daily_recap",
		"scheduled_for": "",
		"content":       "hi",
	})
	if !res.IsError || !strings.Contains(resultText(res), "scheduled_for is required") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestQueueWebhookMessage_MissingContent(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "daily_recap",
		"scheduled_for": "2026-05-02T08:00:00Z",
		"content":       "",
	})
	if !res.IsError || !strings.Contains(resultText(res), "content or brief is required") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestQueueWebhookMessage_ContentTooLong(t *testing.T) {
	_, deps := setupToolsTest(t)
	long := strings.Repeat("x", maxWebhookContentLen+1)
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "daily_recap",
		"scheduled_for": "2026-05-02T08:00:00Z",
		"content":       long,
	})
	if !res.IsError || !strings.Contains(resultText(res), "content too long") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestQueueWebhookMessage_BadScheduledFormat(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "daily_recap",
		"scheduled_for": "not-a-date",
		"content":       "hi",
	})
	if !res.IsError || !strings.Contains(resultText(res), "scheduled_for must be RFC3339") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestQueueWebhookMessage_BadExpiresFormat(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "daily_recap",
		"scheduled_for": "2026-05-02T08:00:00Z",
		"expires_at":    "not-a-date",
		"content":       "hi",
	})
	if !res.IsError || !strings.Contains(resultText(res), "expires_at must be RFC3339") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestQueueWebhookMessage_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)
	optInQueueNotifications(t, store)
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "daily_motivation",
		"scheduled_for": "2026-05-03T08:00:00Z",
		"expires_at":    "2026-05-03T20:00:00Z",
		"content":       "Good morning, stay the course.",
		"priority":      5,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["queue_id"] == nil {
		t.Fatalf("expected queue_id, got %v", out)
	}
	if out["kind"] != "daily_motivation" {
		t.Fatalf("expected kind=daily_motivation, got %v", out["kind"])
	}

	// DB state — message persisted as pending.
	pending, err := store.GetPendingWebhookMessages(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending message, got %d", len(pending))
	}
	if pending[0].Content != "Good morning, stay the course." {
		t.Fatalf("content mismatch: %q", pending[0].Content)
	}
	if pending[0].Priority != 5 {
		t.Fatalf("priority mismatch: %d", pending[0].Priority)
	}
}

func TestQueueWebhookMessageEnforcesConsentAndWeeklyWindow(t *testing.T) {
	store, deps := setupToolsTest(t)
	args := map[string]any{
		"kind":          "daily_recap",
		"scheduled_for": "2026-05-04T09:30:00Z", // Monday
		"content":       "Short recap",
	}
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", args)
	if !res.IsError || !strings.Contains(resultText(res), "consent") {
		t.Fatalf("no-consent response: %q", resultText(res))
	}
	a := &models.Availability{
		LearnerID:              "L_owner",
		Timezone:               "UTC",
		WindowsJSON:            `[{"day":"monday","start":"10:00","end":"11:00"}]`,
		AvgDuration:            30,
		SessionsWeek:           3,
		NotificationConsent:    true,
		NotificationFrequency:  models.NotificationFrequencyAsScheduled,
		MaxNotificationsPerDay: 2,
		AccessibilityJSON:      "{}",
	}
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	res = callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", args)
	if !res.IsError || !strings.Contains(resultText(res), "outside") {
		t.Fatalf("outside-window response: %q", resultText(res))
	}
	a.DoNotDisturb = true
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	args["scheduled_for"] = "2026-05-04T10:30:00Z"
	res = callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", args)
	if !res.IsError || !strings.Contains(resultText(res), "do_not_disturb") {
		t.Fatalf("DND response: %q", resultText(res))
	}
}

func TestQueueWebhookMessageBlocksUnreviewedHighStakesSuggestion(t *testing.T) {
	store, deps := setupToolsTest(t)
	optInQueueNotifications(t, store)
	domain, err := store.CreateDomain(context.Background(), "L_owner", "Clinical", "", models.KnowledgeSpace{
		Concepts: []string{"triage"}, Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDomainHighStakes(context.Background(), domain.ID, "L_owner"); err != nil {
		t.Fatal(err)
	}
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "reminder",
		"scheduled_for": "2026-05-04T10:30:00Z",
		"brief": map[string]any{
			"domain_id":     domain.ID,
			"why_now":       "A safety topic is ready.",
			"learning_gain": "Review the decision process.",
			"open_loop":     "A case remains to inspect.",
			"next_action":   "Open the tutor when you choose.",
		},
	})
	if !res.IsError || !strings.Contains(resultText(res), "human-reviewed") {
		t.Fatalf("high-stakes response: %q", resultText(res))
	}
}

func TestQueueWebhookMessageRejectsForeignDomainEvenForNonIntrusiveRecap(t *testing.T) {
	store, deps := setupToolsTest(t)
	optInQueueNotifications(t, store)
	if _, err := store.CreateDomain(context.Background(), "L_attacker", "Other", "", models.KnowledgeSpace{
		Concepts: []string{"private"}, Prerequisites: map[string][]string{},
	}); err != nil {
		t.Fatal(err)
	}
	foreign, err := store.GetDomainByLearner(context.Background(), "L_attacker")
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "daily_recap",
		"scheduled_for": "2026-05-04T10:30:00Z",
		"brief": map[string]any{
			"domain_id":     foreign.ID,
			"why_now":       "A recap is ready.",
			"open_loop":     "Review the day.",
			"next_action":   "Open the tutor.",
			"learning_gain": "Consolidate learning.",
		},
	})
	if !res.IsError || !strings.Contains(resultText(res), "domain not found") {
		t.Fatalf("foreign-domain response: %q", resultText(res))
	}
}

func TestQueueWebhookMessage_StructuredBrief(t *testing.T) {
	store, deps := setupToolsTest(t)
	optInQueueNotifications(t, store)
	domain, err := store.CreateDomain(context.Background(), "L_owner", "Python", "", models.KnowledgeSpace{
		Concepts: []string{"loops"}, Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "olm:" + domain.ID,
		"scheduled_for": "2026-05-03T13:00:00Z",
		"brief": map[string]any{
			"domain_id":         domain.ID,
			"domain_name":       "Python",
			"concept":           "loops",
			"why_now":           "Retention is dropping on loops, so a short review is more valuable now.",
			"learning_gain":     "Stabilize the concept before moving to longer exercises.",
			"open_loop":         "I kept a small loop bug for the next session.",
			"next_action":       "Open Claude and start with the loop bug.",
			"estimated_minutes": 8,
			"language":          "en",
		},
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	pending, err := store.GetPendingWebhookMessages(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending message, got %d", len(pending))
	}
	if !strings.Contains(pending[0].Content, `"why_now"`) || !strings.Contains(pending[0].Content, "loops") {
		t.Fatalf("structured brief was not persisted as JSON: %q", pending[0].Content)
	}
}

func TestQueueWebhookMessage_StructuredBriefRejectsInternalToolNames(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerQueueWebhookMessage, "L_owner", "queue_webhook_message", map[string]any{
		"kind":          "daily_motivation",
		"scheduled_for": "2026-05-03T08:00:00Z",
		"brief": map[string]any{
			"why_now":       "We will call calibration_check tomorrow.",
			"learning_gain": "Calibrate your level better.",
			"open_loop":     "I kept a mini-test.",
			"next_action":   "Open Claude.",
		},
	})
	if !res.IsError || !strings.Contains(resultText(res), "internal tool names") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestValidWebhookKind(t *testing.T) {
	cases := map[string]bool{
		"daily_motivation": true,
		"daily_recap":      true,
		"reactivation":     true,
		"reminder":         true,
		"mirror_message":   true,
		"olm:d1":           true,
		"":                 false,
		"spam":             false,
	}
	for k, want := range cases {
		got := validWebhookKind(k)
		if got != want {
			t.Errorf("validWebhookKind(%q): want %v got %v", k, want, got)
		}
	}
}

func TestValidWebhookKind_AcceptsOLMPrefix(t *testing.T) {
	if !validWebhookKind("olm:abc123") {
		t.Errorf("validWebhookKind('olm:abc123') = false, want true")
	}
	if validWebhookKind("olm:") {
		t.Errorf("validWebhookKind('olm:') = true, want false (empty domain id)")
	}
	if validWebhookKind("olm") {
		t.Errorf("validWebhookKind('olm') = true, want false (no colon)")
	}
}
