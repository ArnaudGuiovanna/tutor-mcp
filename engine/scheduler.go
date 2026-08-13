// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	"tutor-mcp/observability"
	storeport "tutor-mcp/store"
	"tutor-mcp/webhookurl"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	store  storeport.Store
	cron   *cron.Cron
	logger *slog.Logger
	client *http.Client
	// integrationClient is isolated from the learner/Discord client because
	// tenant integrations never follow redirects: the signed destination is
	// immutable for one delivery attempt.
	integrationClient *http.Client

	// stopTimeout bounds how long Stop() waits for in-flight cron jobs to
	// drain before forcing shutdown. Defaults to 25s (5s of margin within
	// the 30s SIGTERM handler in main.go for database.Close() etc.).
	// Overridable via test-only constructors so timeout-path tests don't
	// need to wait the full production budget.
	stopTimeout time.Duration

	// stopCh is closed by Stop() to signal in-flight work — notably the
	// synchronous backoff sleeps in doWithRetry — that the scheduler is
	// shutting down. A nil stopCh (e.g. in struct-literal test fixtures)
	// is a never-fires channel, so the abort path is simply inert. See #2.
	stopCh chan struct{}

	// distributed lets at most one fleet instance claim a scheduled run slot.
	// Default false keeps every tick local.
	distributed bool

	// owner is an opaque per-process identity used only for compare-and-set
	// lease transitions. It deliberately contains no hostname, PID, or tenant
	// information that could leak through operational tables.
	owner string

	jobLeaseDuration      time.Duration
	jobHeartbeatInterval  time.Duration
	jobRetryDelay         time.Duration
	jobMaxAttempts        int
	jobRetryBatchSize     int
	registeredDistributed map[string]scheduledJobFunc

	learnerPageSize     int
	learnerWorkerCount  int
	learnerWorkDeadline time.Duration

	tenantStore        tenantSchedulerStore
	worker             models.WorkerPrincipal
	currentTenantScope *models.TenantScope
}

type tenantSchedulerStore interface {
	storeport.Store
	ListWorkerTenantScopes(context.Context, models.WorkerPrincipal, string, int) ([]models.TenantScope, string, error)
	RecordWorkerTenantRun(context.Context, models.WorkerPrincipal, models.TenantScope, string, string, string) error
}

type scheduledJobResult struct {
	FailureCode string
}

type scheduledJobFunc func() scheduledJobResult

func scheduledJobSucceeded() scheduledJobResult {
	return scheduledJobResult{}
}

func scheduledJobFailed(code string) scheduledJobResult {
	return scheduledJobResult{FailureCode: code}
}

func (r *scheduledJobResult) recordFailure(code string) {
	if r.FailureCode == "" {
		r.FailureCode = code
	}
}

func (r scheduledJobResult) failed() bool {
	return r.FailureCode != ""
}

// defaultStopTimeout is the production budget for Stop() to wait for
// in-flight jobs. Sized so that main.go's 30s SIGTERM handler still has
// 5s of margin to close the database and finish other shutdown work.
const defaultStopTimeout = 25 * time.Second

const schedulerDBTimeout = 20 * time.Second

const (
	defaultJobLeaseDuration     = 2 * time.Minute
	defaultJobHeartbeatInterval = 30 * time.Second
	defaultJobRetryDelay        = 30 * time.Second
	defaultJobMaxAttempts       = 3
	defaultJobRetryBatchSize    = 100
	defaultLearnerPageSize      = 128
	defaultLearnerWorkerCount   = 8
	defaultLearnerWorkDeadline  = 30 * time.Second
)

func NewScheduler(store storeport.Store, logger *slog.Logger) *Scheduler {
	// cron.New() ships no recover middleware: an unrecovered panic in any
	// scheduled job is a goroutine panic, which Go terminates the whole
	// process for. The HTTP recoveryMiddleware does not cover scheduler
	// goroutines, so without WithChain(Recover(...)) a single bad-data
	// learner could DoS the entire tutor at scheduled job times. See #35.
	cl := slogCronLogger{l: logger}
	return &Scheduler{
		store: store,
		cron: cron.New(cron.WithChain(
			cron.Recover(cl),
			cron.SkipIfStillRunning(cl),
		)),
		logger: logger,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many webhook redirects")
				}
				if !safeWebhookURL(req.URL.String()) {
					return fmt.Errorf("unsafe webhook redirect")
				}
				return nil
			},
		},
		integrationClient: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		stopTimeout: defaultStopTimeout,
		stopCh:      make(chan struct{}),
		owner:       newSchedulerOwner(),

		jobLeaseDuration:      defaultJobLeaseDuration,
		jobHeartbeatInterval:  defaultJobHeartbeatInterval,
		jobRetryDelay:         defaultJobRetryDelay,
		jobMaxAttempts:        defaultJobMaxAttempts,
		jobRetryBatchSize:     defaultJobRetryBatchSize,
		registeredDistributed: make(map[string]scheduledJobFunc),
		learnerPageSize:       defaultLearnerPageSize,
		learnerWorkerCount:    defaultLearnerWorkerCount,
		learnerWorkDeadline:   defaultLearnerWorkDeadline,
	}
}

func (s *Scheduler) learnerPoolConfig() (int, int, time.Duration) {
	pageSize := s.learnerPageSize
	if pageSize < 1 || pageSize > 1000 {
		pageSize = defaultLearnerPageSize
	}
	workers := s.learnerWorkerCount
	if workers < 1 || workers > 128 {
		workers = defaultLearnerWorkerCount
	}
	deadline := s.learnerWorkDeadline
	if deadline <= 0 || deadline > 10*time.Minute {
		deadline = defaultLearnerWorkDeadline
	}
	return pageSize, workers, deadline
}

// processWebhookTargets keyset-pages a narrow scheduler read model into a
// bounded worker pool. Only one page plus the small channel buffer is live at
// once. Per-target deadlines propagate to DB and HTTP calls, while Stop()
// cancels production and lets workers drain or abort without accepting more
// plaintext webhook targets.
func (s *Scheduler) processWebhookTargets(
	jobName, listFailureCode, partialFailureCode string,
	work func(context.Context, models.WebhookDispatchTarget) bool,
) scheduledJobResult {
	pageSize, workerCount, workDeadline := s.learnerPoolConfig()
	ctx, cancel := context.WithCancel(context.Background())
	stopWatchDone := make(chan struct{})
	go func() {
		defer close(stopWatchDone)
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-stopWatchDone
	}()

	jobs := make(chan models.WebhookDispatchTarget, workerCount)
	var workers sync.WaitGroup
	var processed atomic.Int64
	var failed atomic.Bool
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case target, ok := <-jobs:
					if !ok {
						return
					}
					targetCtx, targetCancel := context.WithTimeout(ctx, workDeadline)
					workFailed := func() (result bool) {
						defer func() {
							if recover() != nil {
								result = true
								if s.logger != nil {
									s.logger.Error("scheduler learner work panic recovered", "job", jobName)
								}
							}
						}()
						return work(targetCtx, target)
					}()
					targetCancel()
					if workFailed {
						failed.Store(true)
					}
					processed.Add(1)
				}
			}
		}()
	}

	result := scheduledJobSucceeded()
	afterID := ""
	pages := 0
produce:
	for {
		pageCtx, pageCancel := context.WithTimeout(ctx, schedulerDBTimeout)
		page, err := s.store.ListWebhookDispatchTargetsPage(pageCtx, afterID, pageSize)
		pageCancel()
		if err != nil {
			if ctx.Err() == nil {
				result.recordFailure(listFailureCode)
				if s.logger != nil {
					s.logger.Error("scheduler target page failed", "job", jobName, "error_type", fmt.Sprintf("%T", err))
				}
			} else {
				result.recordFailure(partialFailureCode)
			}
			break
		}
		pages++
		for _, target := range page {
			select {
			case jobs <- target:
			case <-ctx.Done():
				result.recordFailure(partialFailureCode)
				break produce
			}
		}
		if s.logger != nil {
			s.logger.Info("scheduler target page queued", "job", jobName, "page", pages, "page_size", len(page), "has_more", len(page) == pageSize)
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1].LearnerID
	}
	close(jobs)
	workers.Wait()
	if failed.Load() {
		result.recordFailure(partialFailureCode)
	}
	if s.logger != nil {
		s.logger.Info("scheduler target run finished", "job", jobName, "pages", pages, "processed", processed.Load(), "failed", result.failed())
	}
	return result
}

func (s *Scheduler) processConsolidationLearners(
	work func(context.Context, string) bool,
) scheduledJobResult {
	pageSize, workerCount, workDeadline := s.learnerPoolConfig()
	ctx, cancel := context.WithCancel(context.Background())
	stopWatchDone := make(chan struct{})
	go func() {
		defer close(stopWatchDone)
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-stopWatchDone
	}()

	jobs := make(chan string, workerCount)
	var workers sync.WaitGroup
	var processed atomic.Int64
	var failed atomic.Bool
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case learnerID, ok := <-jobs:
					if !ok {
						return
					}
					targetCtx, targetCancel := context.WithTimeout(ctx, workDeadline)
					workFailed := func() (result bool) {
						defer func() {
							if recover() != nil {
								result = true
								if s.logger != nil {
									s.logger.Error("scheduler consolidation work panic recovered")
								}
							}
						}()
						return work(targetCtx, learnerID)
					}()
					if workFailed {
						failed.Store(true)
					}
					targetCancel()
					processed.Add(1)
				}
			}
		}()
	}

	result := scheduledJobSucceeded()
	afterID := ""
	pages := 0
produce:
	for {
		pageCtx, pageCancel := context.WithTimeout(ctx, schedulerDBTimeout)
		page, err := s.store.ListLearnerIDsForConsolidationPage(pageCtx, afterID, pageSize)
		pageCancel()
		if err != nil {
			if ctx.Err() == nil {
				result.recordFailure("consolidation_list_learners_failed")
				if s.logger != nil {
					s.logger.Error("scheduler consolidation page failed", "error_type", fmt.Sprintf("%T", err))
				}
			} else {
				result.recordFailure("consolidation_partial_failure")
			}
			break
		}
		pages++
		for _, learnerID := range page {
			select {
			case jobs <- learnerID:
			case <-ctx.Done():
				result.recordFailure("consolidation_partial_failure")
				break produce
			}
		}
		if s.logger != nil {
			s.logger.Info("scheduler consolidation page queued", "page", pages, "page_size", len(page), "has_more", len(page) == pageSize)
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1]
	}
	close(jobs)
	workers.Wait()
	if failed.Load() {
		result.recordFailure("consolidation_partial_failure")
	}
	if s.logger != nil {
		s.logger.Info("scheduler consolidation run finished", "pages", pages, "processed", processed.Load(), "failed", result.failed())
	}
	return result
}

// NewDistributedScheduler builds the same scheduler as NewScheduler but in
// distributed mode: at most one fleet instance owns each scheduled run slot.
// Live workers heartbeat, expired leases are recovered, and poison jobs become
// explicit dead letters after a bounded number of attempts.
func NewDistributedScheduler(store storeport.Store, logger *slog.Logger) *Scheduler {
	s := NewScheduler(store, logger)
	s.distributed = true
	return s
}

// NewTenantDistributedScheduler keeps the global lease election but executes
// every business job in one explicit tenant transaction. The worker role never
// owns tables and never receives BYPASSRLS.
func NewTenantDistributedScheduler(store storeport.Store, logger *slog.Logger, actorID string) *Scheduler {
	s := NewDistributedScheduler(store, logger)
	tenantStore, ok := store.(tenantSchedulerStore)
	if ok {
		s.tenantStore = tenantStore
	}
	s.worker = models.WorkerPrincipal{ActorID: actorID}
	return s
}

func (s *Scheduler) scheduledTenantJob(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobFunc {
	if s.tenantStore == nil {
		return func() scheduledJobResult { return fn(s) }
	}
	return func() scheduledJobResult { return s.runAcrossTenants(name, fn) }
}

// scheduledIndependentTenantJob is reserved for work that crosses a network
// boundary. It enumerates and audits tenants exactly like scheduledTenantJob,
// while each durable store transition opens its own short RLS transaction.
// Consequently an HTTP timeout never pins a database transaction or row lock.
func (s *Scheduler) scheduledIndependentTenantJob(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobFunc {
	return func() scheduledJobResult { return s.runAcrossTenantRoots(name, fn) }
}

func (s *Scheduler) runAcrossTenantRoots(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobResult {
	if s.tenantStore == nil || !s.worker.Validate() {
		return scheduledJobFailed("worker_identity_invalid")
	}
	result := scheduledJobSucceeded()
	after := ""
	for {
		listCtx, listCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
		scopes, next, err := s.tenantStore.ListWorkerTenantScopes(listCtx, s.worker, after, 100)
		listCancel()
		if err != nil {
			result.recordFailure("worker_tenant_enumeration_failed")
			break
		}
		for _, scope := range scopes {
			startedAt := time.Now()
			runCtx, finishSpan := observability.StartWorkerRun(context.Background(), scope.TenantID, name)
			if err := s.tenantStore.RecordWorkerTenantRun(runCtx, s.worker, scope, name, "started", ""); err != nil {
				finishSpan("failed")
				result.recordFailure("worker_tenant_audit_failed")
				continue
			}
			child := *s
			child.tenantStore = nil
			copyScope := scope
			child.currentTenantScope = &copyScope
			tenantResult := fn(&child)
			status, errorCode := "succeeded", ""
			if tenantResult.failed() {
				status, errorCode = "failed", tenantResult.FailureCode
				result.recordFailure(errorCode)
			}
			if err := s.tenantStore.RecordWorkerTenantRun(runCtx, s.worker, scope, name, status, errorCode); err != nil {
				result.recordFailure("worker_tenant_audit_failed")
			}
			observability.RecordWorkerRun(runCtx, scope.TenantID, name, status, time.Since(startedAt))
			finishSpan(status)
		}
		if next == "" {
			break
		}
		after = next
	}
	return result
}

func (s *Scheduler) runAcrossTenants(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobResult {
	if !s.worker.Validate() {
		return scheduledJobFailed("worker_identity_invalid")
	}
	result := scheduledJobSucceeded()
	after := ""
	for {
		listCtx, listCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
		scopes, next, err := s.tenantStore.ListWorkerTenantScopes(listCtx, s.worker, after, 100)
		listCancel()
		if err != nil {
			result.recordFailure("worker_tenant_enumeration_failed")
			break
		}
		for _, scope := range scopes {
			startedAt := time.Now()
			runCtx, finishSpan := observability.StartWorkerRun(context.Background(), scope.TenantID, name)
			if err := s.tenantStore.RecordWorkerTenantRun(runCtx, s.worker, scope, name, "started", ""); err != nil {
				finishSpan("failed")
				result.recordFailure("worker_tenant_audit_failed")
				continue
			}
			tenantResult := scheduledJobSucceeded()
			err := s.tenantStore.WithTenantTx(runCtx, scope, func(txCtx context.Context, scoped storeport.Store) error {
				child := *s
				child.store = scoped
				child.tenantStore = nil
				tenantResult = fn(&child)
				// Business methods persist their own durable retry state. A
				// partial job result must commit those transitions.
				return nil
			})
			status, errorCode := "succeeded", ""
			if err != nil {
				status, errorCode = "failed", "worker_tenant_transaction_failed"
				result.recordFailure(errorCode)
			} else if tenantResult.failed() {
				status, errorCode = "failed", tenantResult.FailureCode
				result.recordFailure(errorCode)
			}
			if auditErr := s.tenantStore.RecordWorkerTenantRun(runCtx, s.worker, scope, name, status, errorCode); auditErr != nil {
				result.recordFailure("worker_tenant_audit_failed")
			}
			observability.RecordWorkerRun(runCtx, scope.TenantID, name, status, time.Since(startedAt))
			finishSpan(status)
		}
		if next == "" {
			break
		}
		after = next
	}
	return result
}

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
	lease, heartbeatEvery, retryDelay, maxAttempts, _ := s.leaseConfig()
	if s.owner == "" {
		s.owner = newSchedulerOwner()
	}
	// A unique fencing token per attempt prevents a late goroutine from the
	// same process completing a lease that this process has since reacquired.
	leaseOwner := s.owner + "." + newSchedulerOwner()
	now := time.Now().UTC()
	claimCtx, claimCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
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
				renewCtx, renewCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
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
			failCtx, failCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
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
			failCtx, failCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
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
		completeCtx, completeCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
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

func (s *Scheduler) Start() error {
	// OLM: once a day at 13h UTC. Replaces the previous critical-alerts
	// (every 30 min) and review-reminders (3x/day) jobs — see
	// docs/superpowers/specs/2026-05-03-webhook-olm-design.md.
	if s.tenantStore != nil && !s.worker.Validate() {
		return fmt.Errorf("tenant scheduler worker identity is invalid")
	}
	if s.tenantStore == nil && s.worker.Validate() {
		return fmt.Errorf("tenant scheduler store does not support tenant enumeration")
	}
	if err := s.schedule("olm", "0 13 * * *", 24*time.Hour,
		s.scheduledTenantJob("olm", func(child *Scheduler) scheduledJobResult { return child.sendOLM() })); err != nil {
		return fmt.Errorf("add olm job: %w", err)
	}
	if err := s.schedule("consolidation", "30 13 * * *", 24*time.Hour,
		s.scheduledTenantJob("consolidation", func(child *Scheduler) scheduledJobResult { return child.runConsolidationCycle() })); err != nil {
		return fmt.Errorf("add consolidation job: %w", err)
	}
	if err := s.schedule("requeue_stale", "*/5 * * * *", 5*time.Minute,
		s.scheduledTenantJob("requeue_stale", func(child *Scheduler) scheduledJobResult { return child.requeueStaleConsolidations() })); err != nil {
		return fmt.Errorf("add consolidation timeout job: %w", err)
	}
	// Daily motivation: once at 8h UTC.
	if err := s.schedule("motivation", "0 8 * * *", 24*time.Hour,
		s.scheduledTenantJob("motivation", func(child *Scheduler) scheduledJobResult { return child.sendDailyMotivation() })); err != nil {
		return fmt.Errorf("add daily motivation job: %w", err)
	}
	// End-of-day recap: once at 21h UTC.
	if err := s.schedule("recap", "0 21 * * *", 24*time.Hour,
		s.scheduledTenantJob("recap", func(child *Scheduler) scheduledJobResult { return child.sendDailyRecap() })); err != nil {
		return fmt.Errorf("add daily recap job: %w", err)
	}
	// Mirror messages: once at 12h UTC. Dispatches any metacognitive mirror
	// nudges queued during sessions (#59). One per learner per day, dedup'd
	// at the alert layer (MIRROR_MESSAGE) so an in-session enqueue + the
	// scheduler tick can't double-fire.
	if err := s.schedule("mirror", "0 12 * * *", 24*time.Hour,
		s.scheduledTenantJob("mirror", func(child *Scheduler) scheduledJobResult { return child.sendMirrorMessages() })); err != nil {
		return fmt.Errorf("add mirror messages job: %w", err)
	}
	// Cleanup: hourly.
	if err := s.schedule("cleanup", "0 * * * *", time.Hour,
		s.scheduledTenantJob("cleanup", func(child *Scheduler) scheduledJobResult { return child.cleanupExpiredData() })); err != nil {
		return fmt.Errorf("add cleanup job: %w", err)
	}
	// Metacognitive alerts: every 30 minutes. Each alert kind is fired at
	// most once per learner per UTC day via WasAlertSentToday so the cron
	// cadence only controls *latency* (worst case 30 min between the state
	// being met and the webhook firing), not frequency-of-spam.
	if err := s.schedule("metacog_alerts", "*/30 * * * *", 30*time.Minute,
		s.scheduledTenantJob("metacog_alerts", func(child *Scheduler) scheduledJobResult { return child.dispatchMetacognitiveAlerts() })); err != nil {
		return fmt.Errorf("add metacognitive alerts job: %w", err)
	}
	if s.tenantStore != nil {
		if err := s.schedule("saas_outbox_relay", "*/1 * * * *", time.Minute,
			s.scheduledIndependentTenantJob("saas_outbox_relay", func(child *Scheduler) scheduledJobResult {
				return child.relaySaaSOutbox()
			})); err != nil {
			return fmt.Errorf("add SaaS outbox relay: %w", err)
		}
		if err := s.schedule("saas_async_jobs", "*/1 * * * *", time.Minute,
			s.scheduledIndependentTenantJob("saas_async_jobs", func(child *Scheduler) scheduledJobResult {
				return child.consumeSaaSJobs()
			})); err != nil {
			return fmt.Errorf("add SaaS async consumer: %w", err)
		}
		if err := s.schedule("saas_entitlement_expiry", "*/1 * * * *", time.Minute,
			s.scheduledIndependentTenantJob("saas_entitlement_expiry", func(child *Scheduler) scheduledJobResult {
				return child.expireSaaSReservations()
			})); err != nil {
			return fmt.Errorf("add SaaS entitlement expiry: %w", err)
		}
	}
	if s.distributed {
		if _, err := s.cron.AddFunc("*/1 * * * *", s.retryRunnableJobs); err != nil {
			return fmt.Errorf("add distributed retry poller: %w", err)
		}
	}

	s.cron.Start()
	s.logger.Info("scheduler started", "jobs", "olm(13h), consolidation(13h30), consolidation_timeout(5m), motivation(8h), recap(21h), mirror(12h), cleanup(1h), metacog(30m), saas relay/async(1m worker)", "distributed", s.distributed)
	return nil
}

// Stop halts the cron scheduler and waits for any in-flight jobs to drain
// before returning, capped by s.stopTimeout (default 25s).
//
// robfig/cron/v3's Stop() returns a context.Context that is cancelled only
// once all running jobs have finished. Discarding that context (the previous
// behaviour) made Stop() effectively non-blocking, so the deferred chain in
// main.go could run database.Close() while a cron tick was mid-iteration —
// producing "sql: database is closed" mid-loop and silent data loss on
// webhook_message_queue + scheduled_alerts. See issue #123.
func (s *Scheduler) Stop() {
	// Signal in-flight webhook backoff sleeps to abort before we start the
	// drain timer, so a job parked in doWithRetry's exponential schedule
	// doesn't burn the Stop() budget waiting out 0+1+5+25s. Guard against a
	// double Stop() (and a nil stopCh from test fixtures).
	if s.stopCh != nil {
		select {
		case <-s.stopCh:
			// already closed
		default:
			close(s.stopCh)
		}
	}

	ctx := s.cron.Stop()
	timeout := s.stopTimeout
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		s.logger.Warn("scheduler: in-flight jobs did not finish within budget — forcing shutdown",
			"timeout", timeout.String())
	}
}

// ─── Daily Motivation (8h) ──────────────────────────────────────────────────
//
// The scheduler no longer composes motivational text. Claude authors messages
// during sessions via queue_webhook_message, the scheduler dispatches from the
// queue. If the queue is empty for a learner, we fall back to a sober one-liner
// (no KPIs, no analytics bulletin tone).

func (s *Scheduler) sendDailyMotivation() scheduledJobResult {
	return s.dispatchQueued("daily_motivation", "DAILY_MOTIVATION", fallbackDailyMotivation)
}

// ─── Daily Recap (21h) ─────────────────────────────────────────────────────

func (s *Scheduler) sendDailyRecap() scheduledJobResult {
	return s.dispatchQueued("daily_recap", "DAILY_RECAP", fallbackDailyRecap)
}

// dispatchQueued pulls the best pending webhook message for each active learner
// (kind + ±30min scheduling window) and posts it. Falls back to a sober template
// when the queue is empty.
func (s *Scheduler) dispatchQueued(kind, alertTag string, fallback func(*models.Learner) discordPayload) scheduledJobResult {
	now := time.Now().UTC()
	return s.processWebhookTargets(
		"dispatch_"+kind, "dispatch_list_learners_failed", "dispatch_partial_failure",
		func(ctx context.Context, target models.WebhookDispatchTarget) bool {
			return s.dispatchQueuedTarget(ctx, target, kind, alertTag, fallback, now)
		},
	)
}

func (s *Scheduler) dispatchQueuedTarget(
	ctx context.Context,
	target models.WebhookDispatchTarget,
	kind, alertTag string,
	fallback func(*models.Learner) discordPayload,
	now time.Time,
) bool {
	learner, avail := &models.Learner{ID: target.LearnerID}, target.Availability
	if learner.ID == "" || avail == nil {
		return true
	}
	allowed, err := avail.AllowsNotificationAt(now)
	if err != nil {
		s.logger.Error("scheduler: invalid notification policy", "err", err, "learner", learner.ID, "kind", kind)
		return true
	}
	if !allowed {
		return false
	}

	item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, kind, now, 30*time.Minute)
	if err != nil {
		s.logger.Error("scheduler: claim webhook", "err", err, "learner", learner.ID, "kind", kind)
		return true
	}

	var payload discordPayload
	var source string
	var briefForLog *models.WebhookBrief
	domainID := ""
	payloadAlreadyAccessible := false
	if item != nil && item.Content != "" {
		if item.ContentFormat == models.WebhookContentFormatDiscordPayload {
			if err := json.Unmarshal([]byte(item.Content), &payload); err != nil || len(payload.Embeds) == 0 {
				if _, recordErr := s.store.RecordWebhookFailure(ctx, item.ID, learner.ID, "payload_invalid", now); recordErr != nil {
					s.logger.Error("scheduler: persist invalid fallback payload", "error_type", fmt.Sprintf("%T", recordErr), "learner", learner.ID, "queue_id", item.ID)
				}
				s.logger.Error("scheduler: invalid durable fallback payload", "learner", learner.ID, "queue_id", item.ID)
				return true
			}
			domainID = item.DomainID
			payloadAlreadyAccessible = true
			source = "fallback"
		} else {
			embed, brief := embedFromWebhookContent(kind, item.Content, models.UrgencyInfo)
			payload = discordPayload{Embeds: []discordEmbed{embed}}
			briefForLog = brief
			if brief != nil {
				domainID = brief.DomainID
			}
			source = "queue"
		}
		if domainID == "" && strings.HasPrefix(kind, "olm:") {
			domainID = strings.TrimPrefix(kind, "olm:")
		}
	} else if fallback != nil {
		live, liveErr := s.store.HasLiveWebhookMessageKind(ctx, learner.ID, kind, now)
		if liveErr != nil {
			s.logger.Error("scheduler: check live webhook before fallback", "error_type", fmt.Sprintf("%T", liveErr), "learner", learner.ID, "kind", kind)
			return true
		}
		if live {
			return false
		}
		payload = fallback(learner)
		source = "fallback"
	} else {
		return false
	}

	accessibility, err := avail.Accessibility()
	if err != nil {
		if item != nil {
			if releaseErr := s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID); releaseErr != nil {
				s.logger.Error("scheduler: release webhook after invalid accessibility", "error_type", fmt.Sprintf("%T", releaseErr), "learner", learner.ID)
			}
		}
		s.logger.Error("scheduler: invalid accessibility policy", "err", err, "learner", learner.ID)
		return true
	}
	if !payloadAlreadyAccessible {
		payload = applyAccessibilityPreferences(payload, accessibility)
	}
	if item == nil {
		encoded, encodeErr := json.Marshal(payload)
		if encodeErr != nil {
			s.logger.Error("scheduler: encode fallback payload", "error_type", fmt.Sprintf("%T", encodeErr), "learner", learner.ID, "kind", kind)
			return true
		}
		var enqueued bool
		item, enqueued, err = s.store.EnqueueClaimedWebhookPayloadOncePerDay(
			ctx, learner.ID, kind, alertTag, domainID, string(encoded),
			models.IsIntrusiveWebhookKind(kind), now, now.Add(12*time.Hour), 0,
		)
		if err != nil {
			s.logger.Error("scheduler: enqueue durable fallback", "error_type", fmt.Sprintf("%T", err), "learner", learner.ID, "kind", kind)
			return true
		}
		if !enqueued || item == nil {
			return false
		}
	}
	if err := s.dispatchDurableWebhook(
		ctx, learner.ID, alertTag, domainID,
		models.IsIntrusiveWebhookKind(kind), []int64{item.ID}, payload, now,
	); err != nil {
		s.logger.Error("scheduler: durable webhook delivery",
			"error_type", fmt.Sprintf("%T", err), "learner", learner.ID, "kind", kind,
		)
		return true
	}
	if briefForLog != nil {
		if _, err := s.store.CreateWebhookPushLog(ctx, learner.ID, item.ID, briefForLog, now); err != nil {
			s.logger.Warn("scheduler: push log", "err", err, "learner", learner.ID, "kind", kind)
			return true
		}
	}
	s.logger.Info("scheduler: dispatched", "learner", learner.ID, "kind", kind, "source", source)
	return false
}

// queueKindTitle returns the embed title for a given webhook kind.
func queueKindTitle(kind string) string {
	switch kind {
	case "daily_motivation":
		return "☀️ Good morning"
	case "daily_recap":
		return "🌙 Tonight"
	case "reactivation":
		return "👋 Come back whenever you want"
	case "reminder":
		return "📚 Note"
	}
	return "✉️ Message"
}

func queueKindColor(kind string) int {
	switch kind {
	case "daily_motivation":
		return 0x5865F2
	case "daily_recap":
		return 0x57F287
	case "reactivation":
		return 0xFEE75C
	}
	return 0x99AAB5
}

func embedFromWebhookContent(kind, content string, urgency models.AlertUrgency) (discordEmbed, *models.WebhookBrief) {
	if brief, ok := models.DecodeWebhookBrief(content, kind); ok {
		return discordEmbedFromBrief(*brief, urgency), brief
	}
	return discordEmbed{
		Title:       queueKindTitle(kind),
		Description: trimDiscord(content, 1400),
		Color:       queueKindColor(kind),
	}, nil
}

func discordEmbedFromBrief(brief models.WebhookBrief, urgency models.AlertUrgency) discordEmbed {
	brief.Normalize(brief.Kind)
	title := briefTitle(brief, urgency)
	labels := briefLabels(brief.Language)
	description := brief.OpenLoop
	if description == "" {
		description = brief.WhyNow
	}
	if description == "" {
		description = brief.LearningGain
	}
	if description == "" {
		description = "A useful signal is ready for your next session."
	}

	fields := []discordField{}
	addField := func(name, value string, inline bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		fields = append(fields, discordField{
			Name:   name,
			Value:  trimDiscord(value, 900),
			Inline: inline,
		})
	}
	addField(labels.WhyNow, brief.WhyNow, false)
	addField(labels.LearningGain, brief.LearningGain, false)
	addField(labels.Evidence, formatEvidence(brief.Evidence), false)
	addField(labels.GoalLink, brief.GoalLink, false)
	addField(labels.NextAction, actionWithMinutes(brief.NextAction, brief.EstimatedMinutes), false)

	footer := &discordFooter{Text: labels.Footer}
	return discordEmbed{
		Title:       title,
		Description: trimDiscord(description, 900),
		Color:       colorForUrgencyOrKind(urgency, brief.Kind),
		Fields:      fields,
		Footer:      footer,
	}
}

type webhookBriefLabels struct {
	WhyNow       string
	LearningGain string
	Evidence     string
	GoalLink     string
	NextAction   string
	Footer       string
}

func briefLabels(_ string) webhookBriefLabels {
	return webhookBriefLabels{
		WhyNow:       "Why now",
		LearningGain: "Learning gain",
		Evidence:     "Useful signals",
		GoalLink:     "Goal link",
		NextAction:   "Next action",
		Footer:       "Short nudge: solve the challenge in the tutor session, not in Discord.",
	}
}

func briefTitle(brief models.WebhookBrief, urgency models.AlertUrgency) string {
	subject := brief.Concept
	if subject == "" {
		subject = brief.DomainName
	}
	if subject == "" {
		subject = brief.Trigger
	}
	if subject == "" {
		subject = "next session"
	}
	switch urgency {
	case models.UrgencyCritical:
		return "Review soon - " + subject
	case models.UrgencyWarning:
		return "Useful focus - " + subject
	default:
		if brief.Trigger != "" {
			return brief.Trigger + " - " + subject
		}
		return "Learning nudge - " + subject
	}
}

func colorForUrgencyOrKind(urgency models.AlertUrgency, kind string) int {
	switch urgency {
	case models.UrgencyCritical:
		return colorCritical
	case models.UrgencyWarning:
		return colorWarning
	}
	if strings.HasPrefix(kind, "olm") {
		return colorInfo
	}
	return queueKindColor(kind)
}

func formatEvidence(evidence []string) string {
	if len(evidence) == 0 {
		return ""
	}
	if len(evidence) > 3 {
		evidence = evidence[:3]
	}
	lines := make([]string, 0, len(evidence))
	for _, e := range evidence {
		e = strings.TrimSpace(e)
		if e != "" {
			lines = append(lines, "- "+e)
		}
	}
	return strings.Join(lines, "\n")
}

func actionWithMinutes(action string, minutes int) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return ""
	}
	if minutes <= 0 {
		return action
	}
	return fmt.Sprintf("%s (~%d min)", action, minutes)
}

func trimDiscord(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return strings.TrimSpace(s[:max-1]) + "…"
}

// ─── Sober fallback templates (no KPI, no analytics) ─────────────────────────

func fallbackDailyMotivation(_ *models.Learner) discordPayload {
	return discordPayload{Embeds: []discordEmbed{{
		Title:       "☀️ Good morning",
		Description: "Even 5 minutes today keeps the trajectory. Come back whenever you want.",
		Color:       0x5865F2,
	}}}
}

func fallbackDailyRecap(_ *models.Learner) discordPayload {
	return discordPayload{Embeds: []discordEmbed{{
		Title:       "🌙 Tonight",
		Description: "If you stop by, we keep going. Otherwise, see you tomorrow.",
		Color:       0x57F287,
	}}}
}

// ─── Cleanup (hourly) ─────────────────────────────────────────────────────

func (s *Scheduler) cleanupExpiredData() scheduledJobResult {
	result := scheduledJobSucceeded()
	ctx, cancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
	defer cancel()
	now := time.Now().UTC()
	codes, err := s.store.CleanupExpiredCodes(ctx)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup codes", "err", err)
	} else if codes > 0 {
		s.logger.Info("scheduler: cleaned expired codes", "count", codes)
	}

	tokens, err := s.store.CleanupExpiredRefreshTokens(ctx)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup tokens", "err", err)
	} else if tokens > 0 {
		s.logger.Info("scheduler: cleaned expired refresh tokens", "count", tokens)
	}

	accountTokens, err := s.store.CleanupExpiredAccountTokens(ctx)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup account tokens", "err", err)
	} else if accountTokens > 0 {
		s.logger.Info("scheduler: cleaned account tokens", "count", accountTokens)
	}

	oauthClients, err := s.store.CleanupExpiredOAuthClients(ctx)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup OAuth clients", "err", err)
	} else if oauthClients > 0 {
		s.logger.Info("scheduler: cleaned OAuth clients", "count", oauthClients)
	}

	rateBuckets, loginFailures, err := s.store.CleanupRateLimitState(
		ctx, now.Add(-24*time.Hour), now.Add(-24*time.Hour),
	)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: cleanup shared auth limits", "err", err)
	} else if rateBuckets > 0 || loginFailures > 0 {
		s.logger.Info("scheduler: cleaned shared auth limits", "rate_buckets", rateBuckets, "login_failures", loginFailures)
	}

	requeued, err := s.store.RequeueStaleWebhookClaims(ctx, now.Add(-15*time.Minute), now)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: requeue stale webhook claims", "err", err)
	} else if requeued > 0 {
		s.logger.Info("scheduler: requeued stale webhook claims", "count", requeued)
	}

	expired, err := s.store.ExpirePastWebhookMessages(ctx, now)
	if err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: expire webhook queue", "err", err)
	} else if expired > 0 {
		s.logger.Info("scheduler: expired webhook messages", "count", expired)
	}

	// Housekeeping for the distributed-scheduler lease table so it doesn't
	// grow unbounded. Best-effort: a no-op on single-node deployments (the
	// table is simply empty) and never fails the cleanup tick.
	if purged, err := s.store.PurgeJobRunsBefore(ctx, time.Now().Add(-7*24*time.Hour)); err != nil {
		result.recordFailure("cleanup_partial_failure")
		s.logger.Error("scheduler: purge job runs", "err", err)
	} else if purged > 0 {
		s.logger.Info("scheduler: purged old job runs", "count", purged)
	}
	return result
}

// ─── Message Formatting ─────────────────────────────────────────────────────

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordFooter struct {
	Text string `json:"text"`
}

type discordPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

func applyAccessibilityPreferences(payload discordPayload, prefs models.AccessibilityPreferences) discordPayload {
	for i := range payload.Embeds {
		if prefs.AvoidEmojis || prefs.ScreenReaderOptimized {
			payload.Embeds[i].Title = stripEmoji(payload.Embeds[i].Title)
			payload.Embeds[i].Description = stripEmoji(payload.Embeds[i].Description)
			for j := range payload.Embeds[i].Fields {
				payload.Embeds[i].Fields[j].Name = stripEmoji(payload.Embeds[i].Fields[j].Name)
				payload.Embeds[i].Fields[j].Value = stripEmoji(payload.Embeds[i].Fields[j].Value)
			}
			if payload.Embeds[i].Footer != nil {
				payload.Embeds[i].Footer.Text = stripEmoji(payload.Embeds[i].Footer.Text)
			}
		}
		if prefs.ScreenReaderOptimized {
			for j := range payload.Embeds[i].Fields {
				payload.Embeds[i].Fields[j].Inline = false
			}
		}
	}
	return payload
}

func stripEmoji(value string) string {
	value = strings.Map(func(r rune) rune {
		switch {
		case r == '\u200d', r == '\u20e3', r == '\ufe0e', r == '\ufe0f':
			return -1
		case r >= 0x1f000 && r <= 0x1faff:
			return -1
		case r >= 0x2300 && r <= 0x23ff:
			return -1
		case r >= 0x2600 && r <= 0x27bf:
			return -1
		case r >= 0x2b00 && r <= 0x2bff:
			return -1
		case r == 0x00a9, r == 0x00ae, r == 0x203c, r == 0x2049,
			r == 0x2122, r == 0x2139, r == 0x3030, r == 0x303d,
			r == 0x3297, r == 0x3299:
			return -1
		default:
			return r
		}
	}, value)
	return strings.TrimSpace(value)
}

// Note: formatDailyMotivation / formatDailyRecap / formatInactivityNudge have been
// removed. Daily motivation and recap are now authored by Claude via queue_webhook_message
// and dispatched by dispatchQueued (with sober Go fallbacks when the queue is empty).

// ─── Discord Webhook ─────────────────────────────────────────────────────────

func (s *Scheduler) sendDiscordEmbed(url string, payload discordPayload) error {
	body, _ := json.Marshal(payload)
	return s.doWithRetry(url, body)
}

// sendDiscordEmbedDurable performs exactly one HTTP request. Its caller has
// claimed a durable queue row, so failures are persisted with a later
// next_attempt_at rather than retried synchronously in the scheduler tick.
func (s *Scheduler) sendDiscordEmbedDurable(url string, payload discordPayload) error {
	body, err := marshalDiscordRequest(url, payload)
	if err != nil {
		return err
	}
	result := s.doOnceClassified(url, body)
	return result.err
}

type webhookHTTPOutcome int

const (
	webhookHTTPDelivered webhookHTTPOutcome = iota
	webhookHTTPRejected
	webhookHTTPUnknown
)

type webhookHTTPResult struct {
	outcome    webhookHTTPOutcome
	statusCode int
	err        error
}

func marshalDiscordRequest(url string, payload discordPayload) ([]byte, error) {
	if !safeWebhookURL(url) {
		return nil, fmt.Errorf("unsafe webhook url")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal webhook payload: %w", err)
	}
	return body, nil
}

// doOnceClassified performs exactly one outbound attempt after the caller has
// durably entered the HTTP boundary. A response is known only when the client
// returns it without a transport/redirect error. Any such non-2xx response is
// safe to retry through the durable backoff policy; every transport error is
// ambiguous and must be quarantined rather than replayed automatically.
func (s *Scheduler) doOnceClassified(url string, body []byte) webhookHTTPResult {
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		// net/url errors can contain the credential-bearing webhook path.
		s.logger.Warn("webhook network error", "error_type", fmt.Sprintf("%T", err))
		return webhookHTTPResult{outcome: webhookHTTPUnknown, err: fmt.Errorf("webhook transport outcome unknown")}
	}
	resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return webhookHTTPResult{
			outcome: webhookHTTPRejected, statusCode: resp.StatusCode,
			err: fmt.Errorf("webhook returned %d", resp.StatusCode),
		}
	}
	return webhookHTTPResult{outcome: webhookHTTPDelivered, statusCode: resp.StatusCode}
}

func (s *Scheduler) releaseWebhookClaims(ctx context.Context, learnerID string, queueIDs []int64) {
	for _, queueID := range queueIDs {
		if err := s.store.ReleaseWebhookClaim(ctx, queueID, learnerID); err != nil && s.logger != nil {
			s.logger.Error("scheduler: release pre-http webhook claim",
				"error_type", fmt.Sprintf("%T", err), "learner", learnerID, "queue_id", queueID,
			)
		}
	}
}

// dispatchDurableWebhook owns the complete queue/reservation/HTTP lifecycle.
// No caller may issue the HTTP request itself once a durable queue row has been
// claimed: this helper is the single point that classifies every crash boundary.
func (s *Scheduler) dispatchDurableWebhook(
	ctx context.Context,
	learnerID, alertType, domainID string,
	intrusive bool,
	queueIDs []int64,
	payload discordPayload,
	now time.Time,
) error {
	// Resolve plaintext only after all caller-side computation, policy checks,
	// queue claims, and claim revalidation have completed. This is the single
	// runtime boundary where an integration credential enters memory.
	webhookURL, err := s.store.GetWebhookDispatchURL(ctx, learnerID)
	if err != nil {
		s.releaseWebhookClaims(ctx, learnerID, queueIDs)
		return fmt.Errorf("resolve webhook dispatch credential: %w", err)
	}
	body, err := marshalDiscordRequest(webhookURL, payload)
	if err != nil {
		s.releaseWebhookClaims(ctx, learnerID, queueIDs)
		return err
	}

	reservationID, prepared, err := s.store.PrepareWebhookDelivery(
		ctx, learnerID, alertType, domainID, intrusive, queueIDs, now,
	)
	if err != nil {
		s.releaseWebhookClaims(ctx, learnerID, queueIDs)
		return fmt.Errorf("prepare durable webhook delivery: %w", err)
	}
	if !prepared {
		s.releaseWebhookClaims(ctx, learnerID, queueIDs)
		return fmt.Errorf("webhook delivery blocked by notification policy")
	}

	if err := s.store.BeginWebhookDelivery(ctx, learnerID, queueIDs, reservationID, now); err != nil {
		if releaseErr := s.store.ReleasePreparedWebhookDelivery(
			ctx, learnerID, queueIDs, reservationID, "begin_delivery_failed", now,
		); releaseErr != nil && s.logger != nil {
			s.logger.Error("scheduler: release delivery after begin failure",
				"error_type", fmt.Sprintf("%T", releaseErr), "learner", learnerID,
			)
		}
		return fmt.Errorf("begin durable webhook delivery: %w", err)
	}

	result := s.doOnceClassified(webhookURL, body)
	switch result.outcome {
	case webhookHTTPDelivered:
		if err := s.store.CompleteWebhookDelivery(ctx, learnerID, queueIDs, reservationID, time.Now().UTC()); err != nil {
			// The persisted dispatching state is intentional: reconciliation will
			// quarantine it because Discord accepted the request but the local
			// completion transaction did not commit.
			return fmt.Errorf("complete durable webhook delivery after 2xx: %w", err)
		}
		return nil
	case webhookHTTPRejected:
		reason := fmt.Sprintf("http_%d", result.statusCode)
		if _, err := s.store.RecordKnownWebhookDeliveryFailure(
			ctx, learnerID, queueIDs, reservationID, reason, time.Now().UTC(),
		); err != nil {
			return fmt.Errorf("record known webhook delivery failure: %w", err)
		}
		return result.err
	case webhookHTTPUnknown:
		if err := s.store.MarkWebhookDeliveryUnknown(
			ctx, learnerID, queueIDs, reservationID, "transport_outcome_unknown", time.Now().UTC(),
		); err != nil {
			return fmt.Errorf("quarantine unknown webhook delivery: %w", err)
		}
		return result.err
	default:
		return fmt.Errorf("unknown webhook delivery outcome")
	}
}

// retryDelays defines the exponential backoff schedule for doWithRetry.
// Promoted to a package-level var so tests can shrink delays without
// changing behavior in production.
var retryDelays = []time.Duration{0, 1 * time.Second, 5 * time.Second, 25 * time.Second}

// maxRetryAfter caps how long a single 429 Retry-After header is honored. A
// hostile or misconfigured endpoint can return Retry-After up to 60s; sleeping
// that long synchronously in the cron goroutine would stall the tick and eat
// the Stop() drain budget. We honor the header only up to this small bound. (#2)
const maxRetryAfter = 5 * time.Second

// safeWebhookURL is the SSRF guard used by doWithRetry. It defaults to the
// production Discord-only allowlist and is overridden in tests so that
// httptest.NewServer URLs (http://127.0.0.1:...) can be exercised end-to-end.
var safeWebhookURL = webhookurl.IsSafeWebhookURL

// doWithRetry posts body to url with exponential backoff.
// 4 attempts: immediate, +1s, +5s, +25s.
// Stops on 4xx (except 429). Respects Discord Retry-After header on 429.
func (s *Scheduler) doWithRetry(url string, body []byte) error {
	if !safeWebhookURL(url) {
		// Discord webhook paths contain credentials. Never emit the rejected
		// value: even an invalid URL can still carry a real token in its path.
		s.logger.Error("webhook blocked: unsafe url")
		return fmt.Errorf("unsafe webhook url")
	}
	delays := retryDelays
	var lastErr error
	for attempt, delay := range delays {
		if delay > 0 {
			if !s.sleepOrStop(delay) {
				// Scheduler is shutting down — abandon remaining attempts.
				return lastErr
			}
		}
		resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			// net/url errors include the full request URL, including Discord's
			// secret webhook token. Preserve only a stable public category.
			lastErr = fmt.Errorf("webhook transport error")
			s.logger.Warn("webhook network error", "attempt", attempt+1, "error_type", fmt.Sprintf("%T", err))
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			return nil
		}
		lastErr = fmt.Errorf("webhook returned %d", resp.StatusCode)
		// 429: respect Retry-After
		if resp.StatusCode == 429 {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 && secs <= 60 {
					wait := time.Duration(secs) * time.Second
					if wait > maxRetryAfter {
						wait = maxRetryAfter
					}
					s.logger.Warn("webhook rate limited, waiting", "retry_after", secs, "honored", wait.String())
					if !s.sleepOrStop(wait) {
						return lastErr
					}
				}
			}
			continue
		}
		// 4xx (not 429): client error, don't retry
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr
		}
		s.logger.Warn("webhook retry", "attempt", attempt+1, "status", resp.StatusCode)
	}
	return lastErr
}

// sleepOrStop blocks for d, but returns false early (without finishing the
// sleep) if the scheduler's stopCh is closed first. This keeps doWithRetry's
// synchronous backoff interruptible so Stop() isn't held hostage by an
// in-flight webhook retry. A nil stopCh never fires, so the sleep runs in full.
func (s *Scheduler) sleepOrStop(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.stopCh:
		return false
	}
}

// ─── OLM (Open Learner Model) Daily Dispatch ────────────────────────────────

// sendOLM dispatches the daily Open Learner Model webhook at 13h UTC for each
// active learner with at least one actionable domain. Per-domain dispatch
// (cap 3 embeds) lets a learner with multiple domains see all relevant states
// in one Discord message. Falls back to FormatOLMEmbed when no LLM-authored
// message is queued.
func (s *Scheduler) sendOLM() scheduledJobResult {
	now := time.Now().UTC()
	return s.processWebhookTargets(
		"olm", "olm_list_learners_failed", "olm_partial_failure",
		func(ctx context.Context, target models.WebhookDispatchTarget) bool {
			return s.sendOLMTarget(ctx, target, now)
		},
	)
}

func (s *Scheduler) sendOLMTarget(ctx context.Context, target models.WebhookDispatchTarget, now time.Time) bool {
	learner, avail := &models.Learner{ID: target.LearnerID}, target.Availability
	if learner.ID == "" || avail == nil {
		return true
	}
	failed := false
	allowed, err := avail.AllowsNotificationAt(now)
	if err != nil {
		s.logger.Error("scheduler: olm invalid notification policy", "err", err, "learner", learner.ID)
		return true
	}
	if !allowed {
		return false
	}

	domains, err := s.store.GetDomainsByLearner(ctx, learner.ID, false /*includeArchived*/)
	if err != nil {
		s.logger.Error("scheduler: olm list domains", "err", err, "learner", learner.ID)
		return true
	}

	var prepared []preparedWebhookEmbed
	for _, d := range domains {
		if len(prepared) >= 3 {
			break
		}
		if d.HighStakes {
			reviewed, err := s.store.HasHumanReviewedEvaluationInDomain(ctx, learner.ID, d.ID)
			if err != nil {
				failed = true
				s.logger.Error("scheduler: olm high-stakes review", "err", err, "learner", learner.ID, "domain", d.ID)
				continue
			}
			if !reviewed {
				continue
			}
		}
		snap, err := BuildOLMSnapshotAt(ctx, s.store, learner.ID, d.ID, now)
		if err != nil {
			failed = true
			s.logger.Warn("scheduler: olm build", "err", err, "learner", learner.ID, "domain", d.ID)
			continue
		}
		if !snap.HasActionable {
			continue
		}
		var memCtx *memory.EpisodicContext
		if memory.Enabled() && snap.FocusReason == "next frontier" {
			if ctx, err := memory.LoadContextForDomain(learner.ID, d.ID, snap.FocusConcept, &memory.OLMView{FocusConcept: snap.FocusConcept}, nil); err == nil {
				memCtx = ctx
			} else {
				failed = true
				s.logger.Warn("scheduler: olm memory context", "err", err, "learner", learner.ID, "domain", d.ID)
			}
		}
		if !shouldPushDiscord(snap, memCtx) {
			continue
		}

		kind := "olm:" + d.ID
		item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, kind, now, 30*time.Minute)
		if err != nil {
			failed = true
			s.logger.Error("scheduler: olm claim", "err", err, "learner", learner.ID, "domain", d.ID)
			continue
		}
		if item != nil && item.Content != "" {
			if item.ContentFormat == models.WebhookContentFormatDiscordPayload {
				var stored discordPayload
				if err := json.Unmarshal([]byte(item.Content), &stored); err != nil || len(stored.Embeds) == 0 {
					failed = true
					if _, recordErr := s.store.RecordWebhookFailure(ctx, item.ID, learner.ID, "payload_invalid", now); recordErr != nil {
						s.logger.Error("scheduler: persist invalid olm payload", "error_type", fmt.Sprintf("%T", recordErr), "learner", learner.ID, "queue_id", item.ID)
					}
					s.logger.Error("scheduler: invalid durable olm fallback", "learner", learner.ID, "queue_id", item.ID)
					continue
				}
				for index, embed := range stored.Embeds {
					entry := preparedWebhookEmbed{DomainID: item.DomainID, Embed: embed}
					if index == 0 {
						entry.Item = item
					}
					prepared = append(prepared, entry)
				}
			} else {
				embed, brief := embedFromWebhookContent(kind, item.Content, snap.FocusUrgency)
				prepared = append(prepared, preparedWebhookEmbed{DomainID: d.ID, Embed: embed, Item: item, Brief: brief})
			}
		} else {
			brief := BuildOLMNudgeBrief(snap)
			prepared = append(prepared, preparedWebhookEmbed{
				DomainID: d.ID, Embed: discordEmbedFromBrief(brief, snap.FocusUrgency),
				Brief: &brief, NeedsPersistence: true,
			})
		}
	}

	if len(prepared) == 0 {
		return failed
	}
	embeds := make([]discordEmbed, 0, len(prepared))
	for _, p := range prepared {
		embeds = append(embeds, p.Embed)
	}
	accessibility, err := avail.Accessibility()
	if err != nil {
		failed = true
		for _, p := range prepared {
			if p.Item != nil {
				_ = s.store.ReleaseWebhookClaim(ctx, p.Item.ID, learner.ID)
			}
		}
		s.logger.Error("scheduler: olm invalid accessibility policy", "err", err, "learner", learner.ID)
		return true
	}
	payload := applyAccessibilityPreferences(discordPayload{Embeds: embeds}, accessibility)
	claimsActive := true
	queueIDs := make([]int64, 0, len(prepared))
	fallbackPayload := discordPayload{}
	fallbackDomainID := ""
	for index, p := range prepared {
		domain, domainErr := s.store.GetDomainByID(ctx, p.DomainID)
		if domainErr != nil || domain.LearnerID != learner.ID || domain.Archived {
			claimsActive = false
			if domainErr != nil {
				failed = true
				s.logger.Warn("scheduler: olm domain became inactive", "error_type", fmt.Sprintf("%T", domainErr), "learner", learner.ID, "domain", p.DomainID)
			}
		}
		if p.Item == nil {
			if p.NeedsPersistence {
				fallbackPayload.Embeds = append(fallbackPayload.Embeds, payload.Embeds[index])
				if fallbackDomainID == "" {
					fallbackDomainID = p.DomainID
				}
			}
			continue
		}
		queueIDs = append(queueIDs, p.Item.ID)
		active, activeErr := s.store.IsWebhookClaimActive(ctx, p.Item.ID, learner.ID)
		if activeErr != nil || !active {
			claimsActive = false
			if activeErr != nil {
				failed = true
				s.logger.Error("scheduler: olm revalidate claim", "err", activeErr, "learner", learner.ID, "queue_id", p.Item.ID)
			}
		}
	}
	if !claimsActive {
		s.releaseWebhookClaims(ctx, learner.ID, queueIDs)
		return failed
	}

	fallbackQueueID := int64(0)
	if len(fallbackPayload.Embeds) > 0 {
		encoded, encodeErr := json.Marshal(fallbackPayload)
		if encodeErr != nil {
			failed = true
			s.releaseWebhookClaims(ctx, learner.ID, queueIDs)
			s.logger.Error("scheduler: encode olm fallback", "error_type", fmt.Sprintf("%T", encodeErr), "learner", learner.ID)
			return true
		}
		fallbackItem, enqueued, enqueueErr := s.store.EnqueueClaimedWebhookPayloadOncePerDay(
			ctx, learner.ID, "olm:"+fallbackDomainID, alertKindOLM, fallbackDomainID,
			string(encoded), true, now, now.Add(24*time.Hour), 0,
		)
		if enqueueErr != nil || !enqueued || fallbackItem == nil {
			s.releaseWebhookClaims(ctx, learner.ID, queueIDs)
			if enqueueErr != nil {
				failed = true
				s.logger.Error("scheduler: enqueue durable olm fallback", "error_type", fmt.Sprintf("%T", enqueueErr), "learner", learner.ID)
			}
			return failed
		}
		fallbackQueueID = fallbackItem.ID
		queueIDs = append(queueIDs, fallbackItem.ID)
	}
	if err := s.dispatchDurableWebhook(
		ctx, learner.ID, alertKindOLM,
		prepared[0].DomainID, true, queueIDs, payload, now,
	); err != nil {
		failed = true
		s.logger.Error("scheduler: durable olm delivery",
			"error_type", fmt.Sprintf("%T", err), "learner", learner.ID,
		)
		return true
	}
	for _, p := range prepared {
		queueID := int64(0)
		if p.Item != nil {
			queueID = p.Item.ID
		} else if p.NeedsPersistence {
			queueID = fallbackQueueID
		}
		if p.Brief != nil {
			if _, err := s.store.CreateWebhookPushLog(ctx, learner.ID, queueID, p.Brief, now); err != nil {
				failed = true
				s.logger.Warn("scheduler: olm push log", "err", err, "learner", learner.ID)
			}
		}
	}
	s.logger.Info("scheduler: olm dispatched", "learner", learner.ID, "embeds", len(embeds))
	return failed
}

func (s *Scheduler) runConsolidationCycle() scheduledJobResult {
	return s.runConsolidationCycleAt(time.Now().UTC())
}

func (s *Scheduler) runConsolidationCycleAt(now time.Time) scheduledJobResult {
	if !memory.Enabled() {
		return scheduledJobSucceeded()
	}
	return s.processConsolidationLearners(func(ctx context.Context, learnerID string) bool {
		jobs, err := memory.PrepareJobs(learnerID, now)
		if err != nil {
			s.logger.Warn("scheduler: consolidation prepare", "err", err, "learner", learnerID)
			return true
		}
		failed := false
		for _, job := range jobs {
			if ctx.Err() != nil {
				return true
			}
			if err := s.store.UpsertPendingConsolidation(ctx, learnerID, string(job.Period), job.PeriodKey, now); err != nil {
				failed = true
				s.logger.Warn("scheduler: consolidation enqueue failed", "err", err, "learner", learnerID, "period", job.PeriodKey)
			} else {
				s.logger.Info("scheduler: consolidation pending", "learner", learnerID, "period_type", job.Period, "period", job.PeriodKey)
			}
		}
		return failed
	})
}

func (s *Scheduler) requeueStaleConsolidations() scheduledJobResult {
	if !memory.Enabled() {
		return scheduledJobSucceeded()
	}
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(-consolidationDeliveryTimeout())
	count, err := s.store.RequeueStaleDeliveredConsolidations(ctx, cutoff)
	if err != nil {
		s.logger.Warn("scheduler: consolidation timeout requeue failed", "err", err)
		return scheduledJobFailed("consolidation_requeue_failed")
	}
	if count > 0 {
		s.logger.Info("scheduler: consolidation timeout requeued", "count", count)
	}
	return scheduledJobSucceeded()
}

func consolidationDeliveryTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("TUTOR_MCP_CONSOLIDATION_DELIVERY_TIMEOUT_MINUTES"))
	if raw == "" {
		return 30 * time.Minute
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(minutes) * time.Minute
}

type preparedWebhookEmbed struct {
	DomainID         string
	Embed            discordEmbed
	Item             *models.WebhookQueueItem
	Brief            *models.WebhookBrief
	NeedsPersistence bool
}

func shouldPushDiscord(olm *OLMSnapshot, memCtx *memory.EpisodicContext) bool {
	if olm == nil || !olm.HasActionable {
		return false
	}
	if !memory.Enabled() {
		return true
	}
	if olm.FocusReason != "next frontier" {
		return true
	}
	return memCtx != nil && memCtx.HasRecentNarrativeSignal()
}

func allHighStakesDomainsHumanReviewed(ctx context.Context, persistence storeport.Store, learnerID string) (bool, error) {
	domains, err := persistence.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return false, err
	}
	for _, domain := range domains {
		if domain == nil || !domain.HighStakes {
			continue
		}
		reviewed, err := persistence.HasHumanReviewedEvaluationInDomain(ctx, learnerID, domain.ID)
		if err != nil {
			return false, err
		}
		if !reviewed {
			return false, nil
		}
	}
	return true, nil
}

// alertKindOLM is the alert tag used by sendOLM for daily-dedup checks
// (WasAlertSentToday + CreateScheduledAlert). Single source of truth so the
// two call sites cannot drift apart.
const alertKindOLM = "OLM"

// ─── Metacognitive Mirror Daily Dispatch ────────────────────────────────────

// sendMirrorMessages dispatches any pending metacognitive mirror nudges
// queued during sessions (engine.EnqueueMirrorWebhook). Mirror content is
// JSON (pattern + message + open question), decoded into a readable embed.
// Dedup at the alert layer (MirrorAlertKind) so an in-session enqueue won't
// race with the scheduler tick on the same day.
func (s *Scheduler) sendMirrorMessages() scheduledJobResult {
	now := time.Now().UTC()
	return s.processWebhookTargets(
		"mirror", "mirror_list_learners_failed", "mirror_partial_failure",
		func(ctx context.Context, target models.WebhookDispatchTarget) bool {
			return s.sendMirrorTarget(ctx, target, now)
		},
	)
}

func (s *Scheduler) sendMirrorTarget(ctx context.Context, target models.WebhookDispatchTarget, now time.Time) bool {
	learner, avail := &models.Learner{ID: target.LearnerID}, target.Availability
	if learner.ID == "" || avail == nil {
		return true
	}
	allowed, err := avail.AllowsNotificationAt(now)
	if err != nil {
		s.logger.Error("scheduler: mirror invalid notification policy", "err", err, "learner", learner.ID)
		return true
	}
	if !allowed {
		return false
	}
	highStakesAllowed, err := allHighStakesDomainsHumanReviewed(ctx, s.store, learner.ID)
	if err != nil {
		s.logger.Error("scheduler: mirror high-stakes review", "err", err, "learner", learner.ID)
		return true
	}
	if !highStakesAllowed {
		return false
	}

	item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, models.WebhookKindMirror, now, 30*time.Minute)
	if err != nil {
		s.logger.Error("scheduler: mirror claim", "err", err, "learner", learner.ID)
		return true
	}
	if item == nil || item.Content == "" {
		return false
	}

	embed := mirrorEmbedFromContent(item.Content)
	accessibility, err := avail.Accessibility()
	if err != nil {
		if releaseErr := s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID); releaseErr != nil {
			s.logger.Error("scheduler: mirror release invalid accessibility", "error_type", fmt.Sprintf("%T", releaseErr), "learner", learner.ID)
		}
		s.logger.Error("scheduler: mirror invalid accessibility policy", "err", err, "learner", learner.ID)
		return true
	}
	payload := applyAccessibilityPreferences(discordPayload{Embeds: []discordEmbed{embed}}, accessibility)
	active, activeErr := s.store.IsWebhookClaimActive(ctx, item.ID, learner.ID)
	if activeErr != nil {
		if releaseErr := s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID); releaseErr != nil {
			s.logger.Error("scheduler: mirror release after revalidation error", "error_type", fmt.Sprintf("%T", releaseErr), "learner", learner.ID)
		}
		s.logger.Error("scheduler: mirror revalidate claim", "err", activeErr, "learner", learner.ID, "queue_id", item.ID)
		return true
	}
	if !active {
		return false
	}
	if err := s.dispatchDurableWebhook(
		ctx, learner.ID, MirrorAlertKind, "", true,
		[]int64{item.ID}, payload, now,
	); err != nil {
		s.logger.Error("scheduler: durable mirror delivery",
			"error_type", fmt.Sprintf("%T", err), "learner", learner.ID,
		)
		return true
	}
	s.logger.Info("scheduler: mirror dispatched", "learner", learner.ID)
	return false
}

// mirrorEmbedFromContent renders a queued MirrorWebhookContent payload into
// a Discord embed. Falls back to using the raw content as the description
// when the payload isn't valid JSON (e.g. a manual queue_webhook_message call
// posted plain text under kind=mirror_message).
func mirrorEmbedFromContent(content string) discordEmbed {
	var payload MirrorWebhookContent
	desc := content
	if err := json.Unmarshal([]byte(content), &payload); err == nil && payload.Message != "" {
		desc = payload.Message
		if payload.OpenQuestion != "" {
			desc += "\n\n" + payload.OpenQuestion
		}
	}
	return discordEmbed{
		Title:       "🪞 Mirror of the day",
		Description: desc,
		Color:       0x9B59B6,
	}
}
