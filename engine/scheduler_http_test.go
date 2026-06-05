// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/models"
)

// quietLogger returns an slog.Logger that swallows output, so failed/retry log
// lines don't spam test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestScheduler returns a Scheduler with no DB hooked up (nil store) and a
// real *http.Client whose timeout is small enough to keep tests fast. The
// safeWebhookURL guard is bypassed for the duration of the test by the caller.
func newTestScheduler() *Scheduler {
	return &Scheduler{
		store:  nil,
		logger: quietLogger(),
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// withoutBackoff zeroes retryDelays and restores them on cleanup, so tests
// don't pay the production 1+5+25s exponential schedule.
func withoutBackoff(t *testing.T) {
	t.Helper()
	orig := retryDelays
	retryDelays = []time.Duration{0, 0, 0, 0}
	t.Cleanup(func() { retryDelays = orig })
}

// allowAnyURL replaces the SSRF guard with a permissive function for the test,
// so httptest URLs (http://127.0.0.1:...) make it past the gate.
func allowAnyURL(t *testing.T) {
	t.Helper()
	orig := safeWebhookURL
	safeWebhookURL = func(string) bool { return true }
	t.Cleanup(func() { safeWebhookURL = orig })
}

// ─── doWithRetry: SSRF guard ────────────────────────────────────────────────

func TestDoWithRetry_RejectsUnsafeURL(t *testing.T) {
	// Default safeWebhookURL is db.IsSafeWebhookURL; an http URL must be rejected
	// without ever opening a connection.
	withoutBackoff(t)
	s := newTestScheduler()

	err := s.doWithRetry("http://example.com/hook", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for unsafe URL, got nil")
	}
	if !strings.Contains(err.Error(), "unsafe webhook url") {
		t.Errorf("error message = %q, want it to mention 'unsafe webhook url'", err.Error())
	}
}

// ─── doWithRetry: success and retries ───────────────────────────────────────

func TestDoWithRetry_SucceedsImmediately(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	if err := s.doWithRetry(srv.URL, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hit count = %d, want exactly 1 (first attempt should succeed)", got)
	}
}

func TestDoWithRetry_RetriesUntilSuccess(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		// Fail with 500 on attempts 1 and 2, succeed on 3.
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	if err := s.doWithRetry(srv.URL, []byte(`{}`)); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("hit count = %d, want 3 (2 retries then success)", got)
	}
}

func TestDoWithRetry_ExhaustsAndReturnsLastError(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	err := s.doWithRetry(srv.URL, []byte(`{}`))
	if err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %q, want it to reference status 500", err.Error())
	}
	// 4 attempts in retryDelays
	if got := atomic.LoadInt32(&hits); got != 4 {
		t.Errorf("hit count = %d, want 4 (one per retryDelays entry)", got)
	}
}

func TestDoWithRetry_StopsOn4xx(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	err := s.doWithRetry(srv.URL, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for 4xx response")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hit count = %d, want exactly 1 (4xx must not retry)", got)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %q, want it to reference status 400", err.Error())
	}
}

func TestDoWithRetry_RetriesOn429WithRetryAfter(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			// Use an out-of-range Retry-After (>60) so the in-loop sleep is skipped
			// — the retry must still happen because 429 doesn't short-circuit like 4xx.
			w.Header().Set("Retry-After", "9999")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	if err := s.doWithRetry(srv.URL, []byte(`{}`)); err != nil {
		t.Fatalf("expected success after 429, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hit count = %d, want 2 (429 then success)", got)
	}
}

// TestDoWithRetry_CapsRetryAfter asserts an in-range Retry-After larger than
// maxRetryAfter is honored only up to the cap, not for the full header value —
// so a hostile endpoint can't park the cron goroutine for tens of seconds.
func TestDoWithRetry_CapsRetryAfter(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			// In-range (<=60) so it IS honored, but well above maxRetryAfter (5s).
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	start := time.Now()
	if err := s.doWithRetry(srv.URL, []byte(`{}`)); err != nil {
		t.Fatalf("expected success after 429, got %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= 30*time.Second {
		t.Fatalf("doWithRetry honored the full 60s Retry-After (%v) — cap not applied", elapsed)
	}
	// Should be ~maxRetryAfter (5s), allow generous slack for CI scheduling.
	if elapsed > maxRetryAfter+3*time.Second {
		t.Errorf("Retry-After wait = %v, want it bounded near maxRetryAfter (%v)", elapsed, maxRetryAfter)
	}
}

// TestDoWithRetry_AbortsOnStop verifies a closed stopCh interrupts the
// synchronous backoff sleep so a webhook retry can't hold the Stop() drain
// budget hostage. The server always 500s, so without an abort the call would
// pay the full retryDelays schedule.
func TestDoWithRetry_AbortsOnStop(t *testing.T) {
	allowAnyURL(t)

	// Use real (non-zero) delays so there is a sleep to interrupt.
	orig := retryDelays
	retryDelays = []time.Duration{0, 10 * time.Second, 10 * time.Second, 10 * time.Second}
	t.Cleanup(func() { retryDelays = orig })

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	s.stopCh = make(chan struct{})
	// Signal shutdown shortly after the first attempt enters its backoff sleep.
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(s.stopCh)
	}()

	start := time.Now()
	_ = s.doWithRetry(srv.URL, []byte(`{}`))
	elapsed := time.Since(start)

	if elapsed >= 5*time.Second {
		t.Fatalf("doWithRetry did not abort on stop: took %v (expected sub-second)", elapsed)
	}
	// Only the first attempt should have fired before the stop interrupted backoff.
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hit count = %d, want 1 (stop must abort before the second attempt)", got)
	}
}

func TestDoWithRetry_RetriesOn429WithoutRetryAfter(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			// No Retry-After header at all → no inner sleep.
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	if err := s.doWithRetry(srv.URL, []byte(`{}`)); err != nil {
		t.Fatalf("expected success after 429, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hit count = %d, want 2", got)
	}
}

func TestDoWithRetry_NetworkError(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	// Start a server, capture its URL, then close so connections are refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	s := newTestScheduler()
	err := s.doWithRetry(url, []byte(`{}`))
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
}

// ─── sendDiscordEmbed: payload shape ────────────────────────────────────────

func TestSendDiscordEmbed_PostsJSONPayload(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	type captured struct {
		method      string
		contentType string
		body        []byte
	}
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.contentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		got.body = b
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	payload := discordPayload{Embeds: []discordEmbed{{
		Title: "hello", Description: "world", Color: 0xABCDEF,
	}}}
	if err := s.sendDiscordEmbed(srv.URL, payload); err != nil {
		t.Fatalf("sendDiscordEmbed: %v", err)
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

// ─── sendWebhook: backwards-compat plain content payload ────────────────────

func TestSendWebhook_PostsContentField(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var body []byte
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	if err := s.sendWebhook(srv.URL, "ping"); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", contentType)
	}
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if m["content"] != "ping" {
		t.Errorf("content field = %q, want %q", m["content"], "ping")
	}
}

// ─── queueKindTitle / queueKindColor ────────────────────────────────────────

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

// ─── fallbackDailyMotivation / fallbackDailyRecap ──────────────────────────

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

// ─── NewScheduler / Stop ────────────────────────────────────────────────────

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

// Sanity guard: doWithRetry still returns the *last* status when all attempts fail.
// Differentiates 502 from 500 so we know the latest response is reflected.
func TestDoWithRetry_LastStatusReflected(t *testing.T) {
	allowAnyURL(t)
	withoutBackoff(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		// Vary status across attempts; the final one should win.
		switch n {
		case 1, 2, 3:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(srv.Close)

	s := newTestScheduler()
	err := s.doWithRetry(srv.URL, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error after exhaustion")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", http.StatusBadGateway)) {
		t.Errorf("err = %q, want it to mention final status %d", err.Error(), http.StatusBadGateway)
	}
}
