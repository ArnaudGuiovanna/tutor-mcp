// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"database/sql"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

func TestRunDistributedJobCompletesWindowExactlyOnce(t *testing.T) {
	raw, store, _ := rawTestSetup(t, "")
	s := distributedSchedulerForLeaseTest(store)
	var calls atomic.Int64

	s.runDistributedJob("test_job", "window-1", func() scheduledJobResult { calls.Add(1); return scheduledJobSucceeded() })
	s.runDistributedJob("test_job", "window-1", func() scheduledJobResult { calls.Add(1); return scheduledJobSucceeded() })

	if got := calls.Load(); got != 1 {
		t.Fatalf("job calls = %d, want exactly 1", got)
	}
	assertEngineJobRun(t, raw, "test_job", "window-1", "succeeded", 1)
}

func TestRunDistributedJobHeartbeatsLongWork(t *testing.T) {
	raw, store, _ := rawTestSetup(t, "")
	s := distributedSchedulerForLeaseTest(store)
	s.jobLeaseDuration = 80 * time.Millisecond
	s.jobHeartbeatInterval = 15 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runDistributedJob("long_job", "window", func() scheduledJobResult {
			close(started)
			<-release
			return scheduledJobSucceeded()
		})
	}()
	<-started
	time.Sleep(130 * time.Millisecond) // longer than the original lease

	ok, err := store.AcquireJobRunLease(
		context.Background(), "long_job", "window", "other-worker",
		time.Now().UTC(), 80*time.Millisecond, 3,
	)
	if err != nil {
		t.Fatalf("contending acquire: %v", err)
	}
	if ok {
		t.Fatal("contender stole a live heartbeat-renewed job")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("long job did not finish")
	}
	assertEngineJobRun(t, raw, "long_job", "window", "succeeded", 1)
}

func TestRetryPollerRecoversExpiredProcessLease(t *testing.T) {
	raw, store, _ := rawTestSetup(t, "")
	s := distributedSchedulerForLeaseTest(store)
	s.jobLeaseDuration = 60 * time.Millisecond
	s.jobHeartbeatInterval = 15 * time.Millisecond

	now := time.Now().UTC()
	ok, err := store.AcquireJobRunLease(
		context.Background(), "recoverable", "window", "dead-process",
		now, 40*time.Millisecond, 3,
	)
	if err != nil || !ok {
		t.Fatalf("seed crashed lease: ok=%v err=%v", ok, err)
	}
	var recovered atomic.Int64
	s.registeredDistributed["recoverable"] = func() scheduledJobResult { recovered.Add(1); return scheduledJobSucceeded() }
	time.Sleep(60 * time.Millisecond)
	s.retryRunnableJobs()

	if got := recovered.Load(); got != 1 {
		t.Fatalf("recovered executions = %d, want 1", got)
	}
	assertEngineJobRun(t, raw, "recoverable", "window", "succeeded", 2)
}

func TestDistributedPanicsRetryThenDeadLetter(t *testing.T) {
	raw, store, _ := rawTestSetup(t, "")
	s := distributedSchedulerForLeaseTest(store)
	s.jobMaxAttempts = 2
	s.jobRetryDelay = 5 * time.Millisecond

	if !invokePanickingJob(func() {
		s.runDistributedJob("poison", "window", func() scheduledJobResult { panic("secret payload") })
	}) {
		t.Fatal("runDistributedJob did not preserve panic for cron recovery")
	}
	assertEngineJobRun(t, raw, "poison", "window", "retry", 1)
	time.Sleep(10 * time.Millisecond)
	if !invokePanickingJob(func() {
		s.runDistributedJob("poison", "window", func() scheduledJobResult { panic("secret payload") })
	}) {
		t.Fatal("retry did not preserve panic for cron recovery")
	}
	assertEngineJobRun(t, raw, "poison", "window", "dead", 2)

	// A poison tombstone cannot execute a third time.
	var calls atomic.Int64
	s.runDistributedJob("poison", "window", func() scheduledJobResult { calls.Add(1); return scheduledJobSucceeded() })
	if calls.Load() != 0 {
		t.Fatal("dead-letter job executed after exhausting its budget")
	}
}

func TestDistributedBusinessFailureRetriesThenSucceeds(t *testing.T) {
	raw, store, _ := rawTestSetup(t, "")
	s := distributedSchedulerForLeaseTest(store)
	s.jobRetryDelay = 5 * time.Millisecond
	var calls atomic.Int64

	s.runDistributedJob("business", "window", func() scheduledJobResult {
		calls.Add(1)
		return scheduledJobFailed("dispatch_partial_failure")
	})
	assertEngineJobRunWithError(t, raw, "business", "window", "retry", 1, "dispatch_partial_failure")
	time.Sleep(10 * time.Millisecond)
	s.runDistributedJob("business", "window", func() scheduledJobResult {
		calls.Add(1)
		return scheduledJobSucceeded()
	})

	if calls.Load() != 2 {
		t.Fatalf("business executions=%d, want 2", calls.Load())
	}
	assertEngineJobRun(t, raw, "business", "window", "succeeded", 2)
}

func TestDistributedBusinessFailureIsSanitizedAndBounded(t *testing.T) {
	raw, store, _ := rawTestSetup(t, "")
	s := distributedSchedulerForLeaseTest(store)
	s.jobMaxAttempts = 2
	s.jobRetryDelay = 5 * time.Millisecond
	failing := func() scheduledJobResult {
		return scheduledJobFailed("secret learner payload with spaces")
	}

	s.runDistributedJob("business_poison", "window", failing)
	assertEngineJobRunWithError(t, raw, "business_poison", "window", "retry", 1, "job_failed")
	time.Sleep(10 * time.Millisecond)
	s.runDistributedJob("business_poison", "window", failing)
	assertEngineJobRunWithError(t, raw, "business_poison", "window", "dead", 2, "job_failed")
}

func TestDistributedWebhookRetryDoesNotOvertakeDurableBackoffWithFallback(t *testing.T) {
	allowAnyURL(t)
	raw, store, learnerID := rawTestSetup(t, "https://example.invalid/hook")
	now := time.Now().UTC()
	if _, err := store.EnqueueWebhookMessage(
		context.Background(), learnerID, models.WebhookKindDailyMotivation,
		"authored message", now, now.Add(time.Hour), 1,
	); err != nil {
		t.Fatal(err)
	}
	s := distributedSchedulerForLeaseTest(store)
	s.jobRetryDelay = 5 * time.Millisecond
	var requests atomic.Int64
	s.client = &http.Client{Transport: webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return webhookResponse(http.StatusServiceUnavailable), nil
	})}

	s.runDistributedJob("motivation", "window", s.sendDailyMotivation)
	assertEngineJobRunWithError(t, raw, "motivation", "window", "retry", 1, "dispatch_partial_failure")
	time.Sleep(10 * time.Millisecond)
	s.runDistributedJob("motivation", "window", s.sendDailyMotivation)
	assertEngineJobRun(t, raw, "motivation", "window", "succeeded", 2)

	if requests.Load() != 1 {
		t.Fatalf("HTTP requests=%d, want one failed authored attempt and no fallback overtake", requests.Load())
	}
	var count, pending int
	if err := raw.QueryRow(`SELECT COUNT(*), SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END)
		FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, models.WebhookKindDailyMotivation,
	).Scan(&count, &pending); err != nil {
		t.Fatal(err)
	}
	if count != 1 || pending != 1 {
		t.Fatalf("queue count=%d pending=%d, want one durable pending message", count, pending)
	}
}

func distributedSchedulerForLeaseTest(store *db.Store) *Scheduler {
	s := NewDistributedScheduler(store, quietLogger())
	s.jobLeaseDuration = 250 * time.Millisecond
	s.jobHeartbeatInterval = 50 * time.Millisecond
	s.jobRetryDelay = 10 * time.Millisecond
	s.jobMaxAttempts = 3
	s.jobRetryBatchSize = 10
	return s
}

func invokePanickingJob(fn func()) (panicked bool) {
	defer func() {
		panicked = recover() != nil
	}()
	fn()
	return false
}

func assertEngineJobRun(t *testing.T, raw *sql.DB, name, windowKey, wantStatus string, wantAttempts int) {
	t.Helper()
	var status, lastError string
	var attempts int
	if err := raw.QueryRow(
		`SELECT status, attempts, last_error FROM scheduled_job_runs WHERE name = ? AND window_key = ?`,
		name, windowKey,
	).Scan(&status, &attempts, &lastError); err != nil {
		t.Fatalf("read scheduled job: %v", err)
	}
	if status != wantStatus || attempts != wantAttempts {
		t.Fatalf("job state = status=%q attempts=%d last_error=%q, want status=%q attempts=%d",
			status, attempts, lastError, wantStatus, wantAttempts)
	}
	if (status == "retry" || status == "dead") && lastError != "panic_recovered" {
		t.Fatalf("failure class = %q, want panic_recovered", lastError)
	}
}

func assertEngineJobRunWithError(t *testing.T, raw *sql.DB, name, windowKey, wantStatus string, wantAttempts int, wantError string) {
	t.Helper()
	var status, lastError string
	var attempts int
	if err := raw.QueryRow(
		`SELECT status, attempts, last_error FROM scheduled_job_runs WHERE name = ? AND window_key = ?`,
		name, windowKey,
	).Scan(&status, &attempts, &lastError); err != nil {
		t.Fatalf("read scheduled job: %v", err)
	}
	if status != wantStatus || attempts != wantAttempts || lastError != wantError {
		t.Fatalf("job state status=%q attempts=%d error=%q, want %q/%d/%q", status, attempts, lastError, wantStatus, wantAttempts, wantError)
	}
}
