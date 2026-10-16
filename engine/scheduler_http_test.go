// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

// Shared by the active scheduler tests, including durable delivery paths.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestScheduler() *Scheduler {
	return &Scheduler{
		store:  nil,
		logger: quietLogger(),
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func allowAnyURL(t *testing.T) {
	t.Helper()
	orig := safeWebhookURL
	safeWebhookURL = func(string) bool { return true }
	t.Cleanup(func() { safeWebhookURL = orig })
}

func TestDispatchQueued_PostsJSONPayload(t *testing.T) {
	allowAnyURL(t)

	type captured struct {
		method      string
		contentType string
		body        []byte
	}
	received := make(chan captured, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case received <- captured{method: r.Method, contentType: r.Header.Get("Content-Type"), body: b}:
		default:
			t.Error("unexpected additional webhook request")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	raw, store, learnerID := rawTestSetup(t, srv.URL)
	payload := discordPayload{Embeds: []discordEmbed{{
		Title: "hello", Description: "world", Color: 0xABCDEF,
	}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id, err := store.EnqueueWebhookMessage(context.Background(), learnerID, "daily_motivation", string(encoded), now, now.Add(time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}
	// Persist the same structured-payload format used by durable fallbacks.
	// No fallback is supplied: only this real queued row can produce the POST.
	if _, err := raw.Exec(`UPDATE webhook_message_queue SET content_format=? WHERE id=?`, models.WebhookContentFormatDiscordPayload, id); err != nil {
		t.Fatal(err)
	}
	s := schedulerForTest(store)
	if result := s.dispatchQueued("daily_motivation", "DAILY_MOTIVATION", nil); result.failed() {
		t.Fatal(result)
	}
	var got captured
	select {
	case got = <-received:
	default:
		t.Fatal("queued delivery produced no HTTP request")
	}
	if len(received) != 0 {
		t.Fatal("queued delivery produced multiple HTTP requests")
	}
	var status string
	if err := raw.QueryRow(`SELECT status FROM webhook_message_queue WHERE id=?`, id).Scan(&status); err != nil || status != models.WebhookStatusSent {
		t.Fatalf("queue status=%q err=%v; want sent", status, err)
	}

	if got.method != "POST" {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", got.contentType)
	}

	var decoded discordPayload
	if err := json.Unmarshal(got.body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v (raw=%s)", err, got.body)
	}
	if len(decoded.Embeds) != 1 {
		t.Fatalf("embeds len = %d, want 1", len(decoded.Embeds))
	}
	emb := decoded.Embeds[0]
	if emb.Title != "hello" || emb.Description != "world" || emb.Color != 0xABCDEF {
		t.Errorf("decoded embed = %+v, want title/desc/color preserved", emb)
	}
}

func TestQueueKindTitleAndColor(t *testing.T) {
	cases := []struct {
		kind     string
		title    string
		color    int
		titleSub string
	}{
		{"daily_motivation", "", 0x5865F2, "Good morning"},
		{"daily_recap", "", 0x57F287, "Tonight"},
		{"reactivation", "", 0xFEE75C, "Come back"},
		{"reminder", "", 0x99AAB5, "Note"},
		{"unknown", "", 0x99AAB5, "Message"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			gotTitle := queueKindTitle(tc.kind)
			if !strings.Contains(gotTitle, tc.titleSub) {
				t.Errorf("queueKindTitle(%q) = %q, want it to contain %q", tc.kind, gotTitle, tc.titleSub)
			}
			gotColor := queueKindColor(tc.kind)
			if gotColor != tc.color {
				t.Errorf("queueKindColor(%q) = %#x, want %#x", tc.kind, gotColor, tc.color)
			}
		})
	}
}

func TestFallbackPayloads(t *testing.T) {
	p := fallbackDailyMotivation(&models.Learner{ID: "L1"})
	if len(p.Embeds) != 1 || !strings.Contains(p.Embeds[0].Title, "Good morning") {
		t.Errorf("fallbackDailyMotivation = %+v, want title with 'Good morning'", p)
	}
	if p.Embeds[0].Color != 0x5865F2 {
		t.Errorf("fallbackDailyMotivation color = %#x, want 0x5865F2", p.Embeds[0].Color)
	}
	if p.Embeds[0].Description == "" {
		t.Error("fallbackDailyMotivation description should be non-empty")
	}

	p2 := fallbackDailyRecap(&models.Learner{ID: "L1"})
	if len(p2.Embeds) != 1 || !strings.Contains(p2.Embeds[0].Title, "Tonight") {
		t.Errorf("fallbackDailyRecap = %+v, want title with 'Tonight'", p2)
	}
	if p2.Embeds[0].Color != 0x57F287 {
		t.Errorf("fallbackDailyRecap color = %#x, want 0x57F287", p2.Embeds[0].Color)
	}
}

func TestNewSchedulerAndStop(t *testing.T) {
	s := NewScheduler(nil, quietLogger())
	if s == nil {
		t.Fatal("NewScheduler returned nil")
	}
	if s.cron == nil {
		t.Error("expected non-nil cron")
	}
	if s.client == nil || s.client.Timeout == 0 {
		t.Errorf("expected http client with non-zero timeout, got %+v", s.client)
	}
	// Stop on a never-Started cron must not panic.
	s.Stop()
}

func TestNewSchedulerRejectsUnsafeWebhookRedirect(t *testing.T) {
	s := NewScheduler(nil, quietLogger())
	if s.client.CheckRedirect == nil {
		t.Fatal("webhook HTTP client has no redirect policy")
	}
	unsafe := httptest.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data", nil)
	if err := s.client.CheckRedirect(unsafe, []*http.Request{{}}); err == nil {
		t.Fatal("unsafe redirect was accepted")
	}
	safe := httptest.NewRequest(http.MethodGet, "https://discord.com/api/webhooks/123/token", nil)
	if err := s.client.CheckRedirect(safe, []*http.Request{{}}); err != nil {
		t.Fatalf("safe Discord redirect was rejected: %v", err)
	}
}
