// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
)

// schedule registers a cron job. In distributed mode it first claims a lease
// keyed by (name, current time bucket of size period) so at most one fleet
// instance wins the slot; in in-process mode it always runs.
func (s *Scheduler) schedule(name, spec string, period time.Duration, fn scheduledJobFunc) error {
	if s.distributed {
		if s.registeredDistributed == nil {
			s.registeredDistributed = make(map[string]scheduledJobFunc)
		}
		if _, exists := s.registeredDistributed[name]; exists {
			return fmt.Errorf("distributed scheduler job %q is already registered", name)
		}
		s.registeredDistributed[name] = fn
	}
	_, err := s.cron.AddFunc(spec, func() {
		if s.distributed {
			key := fmt.Sprintf("%d", time.Now().UTC().Truncate(period).Unix())
			s.runDistributedJob(name, key, fn)
			return
		}
		result := fn()
		if result.failed() && s.logger != nil {
			s.logger.Error("scheduler local job failed", "job", name, "failure_code", result.FailureCode)
		}
	})
	return err
}

func newSchedulerOwner() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	// crypto/rand failure is extraordinarily rare. Keep the fallback opaque and
	// process-local; uniqueness, not secrecy, is the lease requirement.
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return hex.EncodeToString(digest[:16])
}

func (s *Scheduler) leaseConfig() (time.Duration, time.Duration, time.Duration, int, int) {
	lease := s.jobLeaseDuration
	if lease <= 0 {
		lease = defaultJobLeaseDuration
	}
	heartbeat := s.jobHeartbeatInterval
	if heartbeat <= 0 || heartbeat >= lease {
		heartbeat = lease / 3
		if heartbeat <= 0 {
			heartbeat = time.Millisecond
		}
	}
	retry := s.jobRetryDelay
	if retry <= 0 {
		retry = defaultJobRetryDelay
	}
	maxAttempts := s.jobMaxAttempts
	if maxAttempts < 1 || maxAttempts > 100 {
		maxAttempts = defaultJobMaxAttempts
	}
	batchSize := s.jobRetryBatchSize
	if batchSize < 1 || batchSize > 1000 {
		batchSize = defaultJobRetryBatchSize
	}
	return lease, heartbeat, retry, maxAttempts, batchSize
}

// runDistributedJob owns the complete lease lifecycle around a registered
// function. Panics are persisted as retryable failures and then re-panicked so
// cron.Recover retains the process-safety behavior and stack fingerprinting.
func (s *Scheduler) runDistributedJob(name, windowKey string, fn scheduledJobFunc) {
	runCtx := context.Background()
	if name == "saas_async_jobs" && s.saasDrain != nil {
		s.saasDrain.mu.Lock()
		if s.saasDrain.stopped || s.saasDrain.scheduledRunning || s.saasDrain.running {
			s.saasDrain.mu.Unlock()
			return
		}
		s.saasDrain.runs.Add(1)
		s.saasDrain.scheduledRunning = true
		s.saasDrain.mu.Unlock()
		defer func() {
			s.saasDrain.mu.Lock()
			s.saasDrain.scheduledRunning = false
			s.saasDrain.mu.Unlock()
			s.saasDrain.runs.Done()
		}()
		runCtx = s.saasDrain.ctx
	}
	lease, heartbeatEvery, retryDelay, maxAttempts, _ := s.leaseConfig()
	if s.owner == "" {
		s.owner = newSchedulerOwner()
	}
	// A unique fencing token per attempt prevents a late goroutine from the
	// same process completing a lease that this process has since reacquired.
	leaseOwner := s.owner + "." + newSchedulerOwner()
	now := time.Now().UTC()
	claimCtx, claimCancel := context.WithTimeout(runCtx, schedulerDBTimeout)
	acquired, err := s.store.AcquireJobRunLease(
		claimCtx, name, windowKey, leaseOwner, now, lease, maxAttempts,
	)
	claimCancel()
	if err != nil {
		s.logJobStoreFailure("claim", name, err)
		return
	}
	if !acquired {
		return
	}

	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	leaseLost := false
	jobResult := scheduledJobSucceeded()
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case tick := <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(runCtx, schedulerDBTimeout)
				renewed, renewErr := s.store.RenewJobRunLease(
					renewCtx, name, windowKey, leaseOwner, tick.UTC(), lease,
				)
				renewCancel()
				if renewErr != nil {
					s.logJobStoreFailure("heartbeat", name, renewErr)
					continue
				}
				if !renewed {
					leaseLost = true
					return
				}
			}
		}
	}()

	stopAndJoinHeartbeat := func() {
		close(stopHeartbeat)
		<-heartbeatDone
	}
	defer func() {
		panicValue := recover()
		stopAndJoinHeartbeat()
		finishedAt := time.Now().UTC()

		if panicValue != nil {
			failCtx, failCancel := context.WithTimeout(runCtx, schedulerDBTimeout)
			accepted, failErr := s.store.FailJobRun(
				failCtx, name, windowKey, leaseOwner, finishedAt,
				finishedAt.Add(retryDelay), "panic_recovered",
			)
			failCancel()
			if failErr != nil {
				s.logJobStoreFailure("fail", name, failErr)
			} else if !accepted && s.logger != nil {
				s.logger.Warn("scheduler job failure lost lease", "job", name)
			}
			panic(panicValue)
		}

		if leaseLost {
			if s.logger != nil {
				s.logger.Warn("scheduler job completion skipped after lease loss", "job", name)
			}
			return
		}
		if jobResult.failed() {
			failCtx, failCancel := context.WithTimeout(runCtx, schedulerDBTimeout)
			accepted, failErr := s.store.FailJobRun(
				failCtx, name, windowKey, leaseOwner, finishedAt,
				finishedAt.Add(retryDelay), jobResult.FailureCode,
			)
			failCancel()
			if failErr != nil {
				s.logJobStoreFailure("fail", name, failErr)
			} else if !accepted && s.logger != nil {
				s.logger.Warn("scheduler job failure rejected after lease loss", "job", name)
			}
			return
		}
		completeCtx, completeCancel := context.WithTimeout(runCtx, schedulerDBTimeout)
		completed, completeErr := s.store.CompleteJobRun(
			completeCtx, name, windowKey, leaseOwner, finishedAt,
		)
		completeCancel()
		if completeErr != nil {
			s.logJobStoreFailure("complete", name, completeErr)
		} else if !completed && s.logger != nil {
			s.logger.Warn("scheduler job completion rejected after lease expiry", "job", name)
		}
	}()

	jobResult = fn()
}

// retryRunnableJobs is intentionally bounded. Every replica may poll the same
// page; AcquireJobRunLease remains the authoritative fleet-wide CAS.
func (s *Scheduler) retryRunnableJobs() {
	if !s.distributed {
		return
	}
	lease, _, retryDelay, maxAttempts, batchSize := s.leaseConfig()
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
	refs, err := s.store.ListRunnableJobRuns(ctx, now, batchSize)
	cancel()
	if err != nil {
		s.logJobStoreFailure("list_runnable", "retry_poller", err)
		return
	}
	for _, ref := range refs {
		fn, ok := s.registeredDistributed[ref.Name]
		if ok {
			if ref.Name == "saas_async_jobs" {
				s.launchSaaSRetry(ref.WindowKey, fn)
				continue
			}
			s.runDistributedJob(ref.Name, ref.WindowKey, fn)
			continue
		}
		// A rolling deploy can remove a registered job while an older window is
		// waiting. Consume its normal retry budget instead of leaking it forever.
		leaseOwner := s.owner + "." + newSchedulerOwner()
		claimCtx, claimCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
		acquired, claimErr := s.store.AcquireJobRunLease(
			claimCtx, ref.Name, ref.WindowKey, leaseOwner, now, lease, maxAttempts,
		)
		claimCancel()
		if claimErr != nil {
			s.logJobStoreFailure("claim_unknown", ref.Name, claimErr)
			continue
		}
		if !acquired {
			continue
		}
		failCtx, failCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
		_, failErr := s.store.FailJobRun(
			failCtx, ref.Name, ref.WindowKey, leaseOwner, now,
			now.Add(retryDelay), "unknown_job",
		)
		failCancel()
		if failErr != nil {
			s.logJobStoreFailure("fail_unknown", ref.Name, failErr)
		}
	}
}

func (s *Scheduler) logJobStoreFailure(operation, name string, err error) {
	if s.logger == nil {
		return
	}
	s.logger.Error("scheduler job store failure",
		"operation", operation,
		"job", name,
		"error_type", fmt.Sprintf("%T", err),
	)
}

// slogCronLogger adapts robfig/cron's Logger interface onto an *slog.Logger.
// cron.Recover supplies both the recovered value as an error and the raw stack.
// Neither is safe for standard logs: panic/error text can contain learner data
// or credentials, and stacks contain filesystem paths. Retain only safe types
// and a correlation fingerprint.
type slogCronLogger struct {
	l *slog.Logger
}

func (s slogCronLogger) Info(msg string, keysAndValues ...interface{}) {
	if s.l == nil {
		return
	}
	s.l.Info("cron: "+msg, keysAndValues...)
}

func (s slogCronLogger) Error(err error, msg string, keysAndValues ...interface{}) {
	if s.l == nil {
		return
	}
	event := "error"
	if msg == "panic" {
		event = "panic_recovered"
	}
	args := []interface{}{
		"component", "scheduler_cron",
		"event", event,
		"error_type", fmt.Sprintf("%T", err),
	}
	if fingerprint := cronStackFingerprint(keysAndValues); fingerprint != "" {
		args = append(args, "stack_fingerprint", fingerprint)
	}
	s.l.Error("scheduler cron failure", args...)
}

func cronStackFingerprint(keysAndValues []interface{}) string {
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		key, ok := keysAndValues[i].(string)
		if !ok || key != "stack" {
			continue
		}
		stack, ok := keysAndValues[i+1].(string)
		if !ok || stack == "" {
			return ""
		}
		digest := sha256.Sum256([]byte(stack))
		return hex.EncodeToString(digest[:8])
	}
	return ""
}
