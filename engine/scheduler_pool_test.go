// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"tutor-mcp/models"
)

type schedulerRapidSchedule struct{ delay time.Duration }

func (s schedulerRapidSchedule) Next(now time.Time) time.Time { return now.Add(s.delay) }

func TestSchedulerWebhookPoolPagesAndBoundsConcurrency(t *testing.T) {
	allowAnyURL(t)
	raw, store, _ := rawTestSetup(t, "")
	const targets = 7
	now := time.Now().UTC()
	for i := 0; i < targets; i++ {
		learnerID := fmt.Sprintf("pool-%02d", i)
		if _, err := raw.Exec(
			`INSERT INTO learners
			    (id, email, password_hash, objective, webhook_url, created_at, email_verified_at)
			 VALUES (?, ?, 'h', '', 'https://example.invalid/hook', ?, ?)`,
			learnerID, learnerID+"@example.com", now, now,
		); err != nil {
			t.Fatal(err)
		}
		optInSchedulerNotifications(t, store, learnerID)
	}

	scheduler := schedulerForTest(store)
	scheduler.stopCh = make(chan struct{})
	scheduler.learnerPageSize = 2
	scheduler.learnerWorkerCount = 2
	scheduler.learnerWorkDeadline = 2 * time.Second
	entered := make(chan struct{}, targets)
	release := make(chan struct{})
	var current, maximum, requests atomic.Int64
	scheduler.client = &http.Client{Transport: webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		active := current.Add(1)
		for {
			observed := maximum.Load()
			if active <= observed || maximum.CompareAndSwap(observed, active) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		current.Add(-1)
		return webhookResponse(http.StatusNoContent), nil
	})}
	done := make(chan scheduledJobResult, 1)
	go func() { done <- scheduler.sendDailyMotivation() }()
	<-entered
	<-entered
	select {
	case <-entered:
		t.Fatal("a third HTTP request entered while both worker slots were occupied")
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	select {
	case result := <-done:
		if result.failed() {
			t.Fatalf("pooled dispatch failed: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pooled dispatch did not finish")
	}
	if requests.Load() != targets {
		t.Fatalf("requests=%d, want %d", requests.Load(), targets)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency=%d, want exactly 2", maximum.Load())
	}
}

func TestSchedulerWebhookPoolDeadlineAndStopAreGraceful(t *testing.T) {
	_, store, _ := rawTestSetup(t, "https://example.invalid/hook")
	for _, tc := range []struct {
		name           string
		deadline       time.Duration
		cancelWithStop bool
	}{
		{name: "target deadline", deadline: 25 * time.Millisecond},
		{name: "scheduler stop", deadline: 5 * time.Second, cancelWithStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := schedulerForTest(store)
			scheduler.stopCh = make(chan struct{})
			scheduler.learnerPageSize = 1
			scheduler.learnerWorkerCount = 1
			scheduler.learnerWorkDeadline = tc.deadline
			entered := make(chan struct{})
			done := make(chan scheduledJobResult, 1)
			go func() {
				done <- scheduler.processWebhookTargets(
					"cancellation_test", "list_failed", "partial_failure",
					func(ctx context.Context, _ models.WebhookDispatchTarget) bool {
						close(entered)
						<-ctx.Done()
						return true
					},
				)
			}()
			<-entered
			if tc.cancelWithStop {
				close(scheduler.stopCh)
			}
			select {
			case result := <-done:
				if !result.failed() || result.FailureCode != "partial_failure" {
					t.Fatalf("canceled result=%+v", result)
				}
			case <-time.After(time.Second):
				t.Fatal("worker did not finish within the graceful cancellation budget")
			}
		})
	}
}

func TestSchedulerWebhookPoolSlowTargetDoesNotBlockNeighbors(t *testing.T) {
	_, store, _ := rawTestSetup(t, "")
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		learnerID := fmt.Sprintf("neighbor-%02d", i)
		if _, err := store.RawDB().Exec(
			`INSERT INTO learners
			    (id, email, password_hash, objective, webhook_url, created_at, email_verified_at)
			 VALUES (?, ?, 'h', '', 'https://example.invalid/hook', ?, ?)`,
			learnerID, learnerID+"@example.com", now, now,
		); err != nil {
			t.Fatal(err)
		}
	}

	scheduler := schedulerForTest(store)
	scheduler.stopCh = make(chan struct{})
	scheduler.learnerPageSize = 3
	scheduler.learnerWorkerCount = 2
	scheduler.learnerWorkDeadline = 2 * time.Second
	slowEntered := make(chan struct{})
	fastFinished := make(chan struct{}, 2)
	releaseSlow := make(chan struct{})
	defer func() {
		select {
		case <-releaseSlow:
		default:
			close(releaseSlow)
		}
	}()

	done := make(chan scheduledJobResult, 1)
	go func() {
		done <- scheduler.processWebhookTargets(
			"noisy_neighbor_test", "list_failed", "partial_failure",
			func(ctx context.Context, target models.WebhookDispatchTarget) bool {
				if target.LearnerID == "neighbor-00" {
					close(slowEntered)
					select {
					case <-releaseSlow:
					case <-ctx.Done():
						return true
					}
					return false
				}
				fastFinished <- struct{}{}
				return false
			},
		)
	}()

	select {
	case <-slowEntered:
	case <-time.After(time.Second):
		t.Fatal("slow target never entered the worker pool")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-fastFinished:
		case <-time.After(time.Second):
			t.Fatal("a slow target starved a neighboring target")
		}
	}
	close(releaseSlow)
	select {
	case result := <-done:
		if result.failed() {
			t.Fatalf("noisy-neighbor run failed: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("noisy-neighbor run did not drain")
	}
}

func TestSchedulerCronSkipsOverlappingRuns(t *testing.T) {
	scheduler := NewScheduler(nil, quietLogger())
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls atomic.Int64
	scheduler.cron.Schedule(schedulerRapidSchedule{delay: 10 * time.Millisecond}, cron.FuncJob(func() {
		calls.Add(1)
		entered <- struct{}{}
		<-release
	}))
	scheduler.cron.Start()
	defer scheduler.Stop()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("scheduled run did not start")
	}
	time.Sleep(75 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("overlapping scheduled calls=%d, want 1", got)
	}
	close(release)
}
