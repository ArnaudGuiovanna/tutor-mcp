// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireJobRunLeaseExactlyOneOwner(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	const contenders = 16
	var winners atomic.Int64
	var winnerMu sync.Mutex
	winner := ""
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(contenders)
	for i := 0; i < contenders; i++ {
		owner := fmt.Sprintf("worker-%02d", i)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.AcquireJobRunLease(ctx, "motivation", "window-1", owner, now, time.Minute, 3)
			if err != nil {
				t.Errorf("AcquireJobRunLease(%s): %v", owner, err)
				return
			}
			if ok {
				winners.Add(1)
				winnerMu.Lock()
				winner = owner
				winnerMu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("lease winners = %d, want 1", got)
	}
	if winner == "" {
		t.Fatal("winning owner was not recorded")
	}
	assertScheduledJobState(t, s, "motivation", "window-1", "processing", winner, 1, "")
}

func TestJobRunLeaseHeartbeatExpiryAndCompletion(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	ok, err := s.AcquireJobRunLease(ctx, "recap", "window", "worker-a", now, time.Minute, 3)
	if err != nil || !ok {
		t.Fatalf("initial acquire: ok=%v err=%v", ok, err)
	}
	renewedAt := now.Add(30 * time.Second)
	ok, err = s.RenewJobRunLease(ctx, "recap", "window", "worker-a", renewedAt, time.Minute)
	if err != nil || !ok {
		t.Fatalf("renew: ok=%v err=%v", ok, err)
	}

	// The original lease would have expired, but the heartbeat extended it.
	ok, err = s.AcquireJobRunLease(ctx, "recap", "window", "worker-b", now.Add(70*time.Second), time.Minute, 3)
	if err != nil {
		t.Fatalf("acquire during renewed lease: %v", err)
	}
	if ok {
		t.Fatal("second owner acquired a heartbeat-renewed lease")
	}

	takeoverAt := now.Add(91 * time.Second)
	ok, err = s.AcquireJobRunLease(ctx, "recap", "window", "worker-b", takeoverAt, time.Minute, 3)
	if err != nil || !ok {
		t.Fatalf("expired lease takeover: ok=%v err=%v", ok, err)
	}
	assertScheduledJobState(t, s, "recap", "window", "processing", "worker-b", 2, "")

	// A stale owner cannot complete the new owner's work.
	ok, err = s.CompleteJobRun(ctx, "recap", "window", "worker-a", takeoverAt.Add(time.Second))
	if err != nil {
		t.Fatalf("stale completion: %v", err)
	}
	if ok {
		t.Fatal("stale owner completed a taken-over job")
	}
	ok, err = s.CompleteJobRun(ctx, "recap", "window", "worker-b", takeoverAt.Add(time.Second))
	if err != nil || !ok {
		t.Fatalf("current completion: ok=%v err=%v", ok, err)
	}
	ok, err = s.AcquireJobRunLease(ctx, "recap", "window", "worker-c", takeoverAt.Add(2*time.Hour), time.Minute, 3)
	if err != nil {
		t.Fatalf("acquire completed window: %v", err)
	}
	if ok {
		t.Fatal("completed window was replayed")
	}
	assertScheduledJobState(t, s, "recap", "window", "succeeded", "", 2, "")
}

func TestJobRunRetryAndDeadLetterBudget(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	ok, err := s.AcquireJobRunLease(ctx, "olm", "window", "worker-a", now, time.Minute, 2)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	retryAt := now.Add(2 * time.Minute)
	ok, err = s.FailJobRun(ctx, "olm", "window", "worker-a", now.Add(time.Second), retryAt, "panic_recovered")
	if err != nil || !ok {
		t.Fatalf("first failure: ok=%v err=%v", ok, err)
	}
	assertScheduledJobState(t, s, "olm", "window", "retry", "", 1, "panic_recovered")

	refs, err := s.ListRunnableJobRuns(ctx, retryAt.Add(-time.Millisecond), 10)
	if err != nil {
		t.Fatalf("list before retry: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("job became runnable before retry time: %+v", refs)
	}
	ok, err = s.AcquireJobRunLease(ctx, "olm", "window", "worker-b", retryAt, time.Minute, 2)
	if err != nil || !ok {
		t.Fatalf("retry acquire: ok=%v err=%v", ok, err)
	}
	ok, err = s.FailJobRun(ctx, "olm", "window", "worker-b", retryAt.Add(time.Second), retryAt.Add(time.Minute), "handler_error")
	if err != nil || !ok {
		t.Fatalf("terminal failure: ok=%v err=%v", ok, err)
	}
	assertScheduledJobState(t, s, "olm", "window", "dead", "", 2, "handler_error")

	ok, err = s.AcquireJobRunLease(ctx, "olm", "window", "worker-c", retryAt.Add(time.Hour), time.Minute, 2)
	if err != nil {
		t.Fatalf("acquire dead letter: %v", err)
	}
	if ok {
		t.Fatal("dead-letter job became claimable")
	}
}

func TestJobRunStaleFinalAttemptTerminalizedAndErrorRedacted(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	ok, err := s.AcquireJobRunLease(ctx, "cleanup", "window", "worker", now, time.Second, 1)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	refs, err := s.ListRunnableJobRuns(ctx, now.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("list stale final attempt: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("exhausted job listed as runnable: %+v", refs)
	}
	assertScheduledJobState(t, s, "cleanup", "window", "dead", "", 1, "lease_expired_after_max_attempts")

	ok, err = s.AcquireJobRunLease(ctx, "cleanup", "redaction", "worker", now, time.Minute, 1)
	if err != nil || !ok {
		t.Fatalf("redaction acquire: ok=%v err=%v", ok, err)
	}
	ok, err = s.FailJobRun(ctx, "cleanup", "redaction", "worker", now.Add(time.Second), now.Add(time.Minute), "sql: secret /tmp/private")
	if err != nil || !ok {
		t.Fatalf("redaction fail: ok=%v err=%v", ok, err)
	}
	assertScheduledJobState(t, s, "cleanup", "redaction", "dead", "", 1, "job_failed")
}

func TestPurgeJobRunsRetainsNonTerminalWork(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := now.Add(-30 * 24 * time.Hour)

	if _, err := s.exec(ctx,
		`INSERT INTO scheduled_job_runs
		 (name, window_key, claimed_at, status, owner, leased_until, attempts, max_attempts, next_attempt_at, last_error, updated_at)
		 VALUES (?, ?, ?, 'succeeded', '', NULL, 1, 3, NULL, '', ?)`,
		"terminal", "old", old, old); err != nil {
		t.Fatalf("insert terminal: %v", err)
	}
	if _, err := s.exec(ctx,
		`INSERT INTO scheduled_job_runs
		 (name, window_key, claimed_at, status, owner, leased_until, attempts, max_attempts, next_attempt_at, last_error, updated_at)
		 VALUES (?, ?, ?, 'retry', '', NULL, 1, 3, ?, 'handler_error', ?)`,
		"retry", "old", old, now.Add(time.Hour), now); err != nil {
		t.Fatalf("insert retry: %v", err)
	}

	purged, err := s.PurgeJobRunsBefore(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged rows = %d, want 1 terminal row", purged)
	}
	assertScheduledJobState(t, s, "retry", "old", "retry", "", 1, "handler_error")
}

func assertScheduledJobState(
	t *testing.T,
	s *Store,
	name, windowKey, wantStatus, wantOwner string,
	wantAttempts int,
	wantError string,
) {
	t.Helper()
	var status, owner, lastError string
	var attempts int
	if err := s.queryRow(context.Background(),
		`SELECT status, owner, attempts, last_error
		 FROM scheduled_job_runs WHERE name = ? AND window_key = ?`,
		name, windowKey).Scan(&status, &owner, &attempts, &lastError); err != nil {
		t.Fatalf("read scheduled job %s/%s: %v", name, windowKey, err)
	}
	if status != wantStatus || owner != wantOwner || attempts != wantAttempts || lastError != wantError {
		t.Fatalf("scheduled job %s/%s = status=%q owner=%q attempts=%d last_error=%q; want %q %q %d %q",
			name, windowKey, status, owner, attempts, lastError,
			wantStatus, wantOwner, wantAttempts, wantError)
	}
}
