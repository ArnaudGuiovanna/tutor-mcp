// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureHandler is an slog.Handler that records every Record into an
// internal slice (string-formatted, just enough for substring asserts).
// Used to verify that cron's Recover middleware actually emitted a
// "panic" error line after a panicking job fired.
type captureHandler struct {
	mu   sync.Mutex
	logs []string
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Level.String())
	sb.WriteString(" ")
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(" ")
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
		return true
	})
	h.mu.Lock()
	h.logs = append(h.logs, sb.String())
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *captureHandler) contains(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, line := range h.logs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func TestSlogCronLoggerRedactsRecoveredErrorAndStack(t *testing.T) {
	const webhookSecret = "https://discord.com/api/webhooks/789/cron-panic-token"
	cap := &captureHandler{}
	logger := slogCronLogger{l: slog.New(cap)}

	logger.Error(
		errors.New("delivery panic at "+webhookSecret),
		"panic",
		"stack", "goroutine 42 [running]:\n/private/source/path\n"+webhookSecret,
		"unexpected", webhookSecret,
	)

	for _, secret := range []string{webhookSecret, "delivery panic", "goroutine 42", "/private/source/path"} {
		if cap.contains(secret) {
			t.Fatalf("cron recovery leaked %q in standard logs", secret)
		}
	}
	for _, diagnostic := range []string{"scheduler cron failure", "component=scheduler_cron", "event=panic_recovered", "error_type=*errors.errorString", "stack_fingerprint="} {
		if !cap.contains(diagnostic) {
			t.Fatalf("cron recovery omitted safe diagnostic %q", diagnostic)
		}
	}
}

// TestScheduler_PanickingJobDoesNotCrashLoop is the issue #35 reproducer:
// a job that panics must NOT take down the whole process. With the fix
// (cron.WithChain(cron.Recover(...))) the panic is caught, logged via
// the slog adapter, and a sibling sentinel job continues to fire.
//
// On unfixed code (cron.New() with no Recover), the goroutine panic
// terminates the test process — the test cannot even report failure,
// the binary exits non-zero. That's still a clear regression signal.
func TestScheduler_PanickingJobDoesNotCrashLoop(t *testing.T) {
	cap := &captureHandler{}
	logger := slog.New(cap)

	s := NewScheduler(nil, logger)

	// Sentinel: keeps incrementing if the cron loop is alive.
	var sentinelHits int64
	if _, err := s.cron.AddFunc("@every 50ms", func() {
		atomic.AddInt64(&sentinelHits, 1)
	}); err != nil {
		t.Fatalf("add sentinel job: %v", err)
	}

	// Panicking job: should be neutralised by cron.Recover.
	if _, err := s.cron.AddFunc("@every 50ms", func() {
		panic("boom from issue-35 reproducer")
	}); err != nil {
		t.Fatalf("add panicking job: %v", err)
	}

	s.cron.Start()
	defer s.cron.Stop()

	// Run long enough to observe at least one sentinel tick AFTER the
	// panicking job has fired and been recovered. robfig/cron's @every
	// schedule rolls forward (it does not preserve sub-second granularity
	// reliably across versions), so 1.5s is the safe floor for "we saw the
	// loop survive multiple wakeups".
	time.Sleep(1500 * time.Millisecond)

	hits := atomic.LoadInt64(&sentinelHits)
	if hits < 1 {
		t.Fatalf("sentinel job never fired; cron loop appears to have died (panic took it out)")
	}

	// And the slog bridge should have surfaced the panic via cron.Recover.
	if !cap.contains("panic") {
		t.Fatalf("expected captured slog output to mention 'panic' (cron.Recover bridge), got logs=%v", cap.logs)
	}
}
