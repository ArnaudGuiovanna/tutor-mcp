package engine

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"tutor-mcp/models"
	"tutor-mcp/observability"
	storeport "tutor-mcp/store"
)

const saasConsumerWorkers = 4
const saasJobTimeout = 45 * time.Second // less than lease, including claim overhead

// One pointer survives Scheduler child copies and all cron/retry windows.
type saasDrainState struct {
	mu                                               sync.Mutex
	runs                                             sync.WaitGroup // admitted scheduled-run lifecycles, including preclaim/finalization
	ctx                                              context.Context
	cancel                                           context.CancelFunc
	running, stopped, retryRunning, scheduledRunning bool
	done, retryDone                                  chan struct{}
}

func newSaaSDrainState() *saasDrainState {
	ctx, cancel := context.WithCancel(context.Background())
	return &saasDrainState{ctx: ctx, cancel: cancel}
}

func (state *saasDrainState) stop() {
	state.mu.Lock()
	state.stopped = true
	state.cancel()
	done, retryDone := state.done, state.retryDone
	state.mu.Unlock()
	if done != nil {
		<-done
	}
	if retryDone != nil {
		<-retryDone
	}
	state.runs.Wait()
}

// A retry poll must not be held forever by a healthy continuously busy queue.
// Its one dedicated runner keeps the scheduled-run heartbeat around the drain.
func (s *Scheduler) launchSaaSRetry(window string, fn scheduledJobFunc) {
	state := s.saasDrain
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.stopped || state.running || state.retryRunning || state.scheduledRunning {
		state.mu.Unlock()
		return
	}
	state.retryRunning = true
	state.retryDone = make(chan struct{})
	done := state.retryDone
	state.mu.Unlock()
	go func() {
		defer func() {
			if recover() != nil && s.logger != nil {
				s.logger.Error("SaaS retry runner recovered", "error_type", "panic_recovered")
			}
			state.mu.Lock()
			state.retryRunning = false
			close(done)
			state.mu.Unlock()
		}()
		s.runDistributedJob("saas_async_jobs", window, fn)
	}()
}

type saasScopeWork struct {
	scope models.TenantScope
}
type saasScopeResult struct {
	scope             models.TenantScope
	claimed, finished bool
	err               error
	outcome           models.AsyncJobOutcome
}

// consumeSaaSJobs admits at most one job per tenant at a time. Each page is
// visited before the next sweep, with no barrier waiting for slow HTTP at its
// end. Resource use is bounded by one page, four workers and four results.
func (s *Scheduler) consumeSaaSJobs() scheduledJobResult {
	backend, ok := s.store.(saasWorkerStore)
	state := s.saasDrain
	if !ok || state == nil || (s.currentTenantScope == nil && (s.tenantStore == nil || !s.worker.Validate())) {
		return scheduledJobFailed("saas_worker_store_unavailable")
	}
	state.mu.Lock()
	if state.running || state.stopped {
		state.mu.Unlock()
		return scheduledJobSucceeded()
	}
	state.running = true
	state.done = make(chan struct{})
	done := state.done
	ctx, cancel := context.WithCancel(state.ctx)
	state.mu.Unlock()
	defer func() { state.mu.Lock(); state.running = false; close(done); state.mu.Unlock() }()
	work := make(chan saasScopeWork)
	results := make(chan saasScopeResult, saasConsumerWorkers)
	var workers sync.WaitGroup
	for range saasConsumerWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range work {
				s.consumeSaaSScope(ctx, backend, item, results)
			}
		}()
	}
	// Cancellation unblocks result sends before joining the workers.
	defer func() { cancel(); close(work); workers.Wait() }()
	result := scheduledJobSucceeded()
	inflight := make(map[string]bool, saasConsumerWorkers) // false until claim resolved
	claimedThisPass := false
	accept := func(item saasScopeResult) {
		// A completion from a previous sweep can expose another job for a
		// tenant skipped while it was busy. It must also force a fresh sweep.
		if item.claimed {
			claimedThisPass = true
		}
		if item.finished {
			delete(inflight, item.scope.TenantID)
		} else {
			inflight[item.scope.TenantID] = true
		}
		if item.err != nil && !errors.Is(item.err, storeport.ErrTenantDSARBlocked) {
			result.recordFailure("saas_job_partial_failure")
		}
	}
	waitResult := func() bool {
		select {
		case item := <-results:
			accept(item)
			return true
		case <-ctx.Done():
			return false
		}
	}
	for ctx.Err() == nil {
		claimedThisPass = false
		after := ""
		for {
			var scopes []models.TenantScope
			next := ""
			if s.currentTenantScope != nil {
				scopes = []models.TenantScope{*s.currentTenantScope}
			} else {
				listCtx, stop := context.WithTimeout(ctx, schedulerDBTimeout)
				var err error
				scopes, next, err = s.tenantStore.ListWorkerTenantScopes(listCtx, s.worker, after, 100)
				stop()
				if err != nil {
					return scheduledJobFailed("worker_tenant_enumeration_failed")
				}
				if len(scopes) > 100 || next != "" && next <= after {
					return scheduledJobFailed("worker_tenant_page_invalid")
				}
			}
			for _, scope := range scopes {
				// Consume completions, then prioritize the next unseen scope of
				// this page rather than enqueueing local tenant continuations.
			drain:
				for {
					select {
					case item := <-results:
						accept(item)
					default:
						break drain
					}
				}
				if _, busy := inflight[scope.TenantID]; busy {
					continue
				}
				for len(inflight) >= saasConsumerWorkers {
					if !waitResult() {
						return result
					}
				}
				inflight[scope.TenantID] = false // capacity acquired BEFORE claim
				select {
				case work <- saasScopeWork{scope: scope}:
				case <-ctx.Done():
					return result
				}
			}
			if next == "" {
				break
			}
			after = next
		}
		// Once any claim is known, start another sweep without waiting for
		// slow claim DB calls either. If no work was claimed, resolve empty
		// probes before waiting on live executions so they cannot cause spin.
		for !claimedThisPass {
			pending := false
			for _, claimed := range inflight {
				if !claimed {
					pending = true
					break
				}
			}
			if !pending {
				break
			}
			if !waitResult() {
				return result
			}
		}
		if !claimedThisPass {
			if len(inflight) == 0 {
				return result
			}
			// Only live executions remain; waiting for one completion prevents
			// an empty tenant from causing a busy rescan behind a slow tenant.
			if !waitResult() {
				return result
			}
		}
	}
	return result
}

func (s *Scheduler) consumeSaaSScope(ctx context.Context, backend saasWorkerStore, item saasScopeWork, results chan<- saasScopeResult) {
	result := saasScopeResult{scope: item.scope, finished: true}
	defer func() {
		if recover() != nil {
			result.err = errors.New("async worker panic recovered")
			if s.logger != nil {
				s.logger.Error("async worker recovered", "error_type", "panic_recovered")
			}
		}
		select {
		case results <- result:
		case <-ctx.Done():
		}
	}()
	claimCtx, cancel := context.WithTimeout(ctx, schedulerDBTimeout)
	job, err := backend.ClaimAsyncJob(claimCtx, item.scope, s.owner+"."+newSchedulerOwner(), s.saasTimeNow(), saasWorkerLease)
	cancel()
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		result.err = err
		return
	}
	result.claimed = true
	select {
	case results <- saasScopeResult{scope: item.scope, claimed: true}:
	case <-ctx.Done():
		return
	}
	jobCtx, stop := context.WithTimeout(ctx, saasJobTimeout)
	defer stop()
	started := time.Now()
	spanCtx, finishSpan := observability.StartWorkerRun(jobCtx, item.scope.TenantID, "saas_async_jobs")
	defer func() {
		p := recover()
		if p != nil {
			result.err = errors.New("async worker panic recovered")
		}
		status := "succeeded"
		if (result.err != nil && !errors.Is(result.err, storeport.ErrTenantDSARBlocked)) || result.outcome == models.AsyncJobDeadLetter || result.outcome == models.AsyncJobRetry {
			status = "failed"
		}
		observability.RecordWorkerRun(spanCtx, item.scope.TenantID, "saas_async_jobs", status, time.Since(started))
		finishSpan(status)
		if p != nil {
			panic(p)
		}
	}()
	// Empty scopes do not generate worker audit rows on every sweep.
	if s.tenantStore != nil {
		if err := s.tenantStore.RecordWorkerTenantRun(jobCtx, s.worker, item.scope, "saas_async_jobs", "started", ""); err != nil {
			result.err = err
			return
		}
	}
	result.outcome, result.err = s.consumeSaaSJob(spanCtx, backend, item.scope, job)
	if s.tenantStore != nil && jobCtx.Err() == nil {
		status, code := "succeeded", ""
		if result.err != nil && !errors.Is(result.err, storeport.ErrTenantDSARBlocked) {
			status, code = "failed", "saas_job_partial_failure"
		} else if result.outcome == models.AsyncJobDeadLetter || result.outcome == models.AsyncJobRetry {
			status, code = "failed", "saas_job_"+string(result.outcome)
		}
		if err := s.tenantStore.RecordWorkerTenantRun(jobCtx, s.worker, item.scope, "saas_async_jobs", status, code); err != nil {
			result.err = err
		}
	}
}
