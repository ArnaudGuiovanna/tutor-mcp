package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type watchedSaaSStore struct {
	*db.Store
	mu          sync.Mutex
	claims      map[string]int
	sequence    []string
	lists       int
	beforeClaim func(context.Context, models.TenantScope, int) error
	afterClaim  func(models.TenantScope, int, error)
}

func (s *watchedSaaSStore) ClaimAsyncJob(ctx context.Context, scope models.TenantScope, owner string, now time.Time, lease time.Duration) (*models.AsyncJob, error) {
	s.mu.Lock()
	s.claims[scope.TenantID]++
	count := s.claims[scope.TenantID]
	s.sequence = append(s.sequence, scope.TenantID)
	s.mu.Unlock()
	if s.beforeClaim != nil {
		if err := s.beforeClaim(ctx, scope, count); err != nil {
			return nil, err
		}
	}
	job, err := s.Store.ClaimAsyncJob(ctx, scope, owner, now, lease)
	if s.afterClaim != nil {
		s.afterClaim(scope, count, err)
	}
	return job, err
}
func (s *watchedSaaSStore) ListWorkerTenantScopes(ctx context.Context, worker models.WorkerPrincipal, after string, limit int) ([]models.TenantScope, string, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.ListWorkerTenantScopes(ctx, worker, after, limit)
}

type gatedSaaSFinalizer struct {
	*watchedSaaSStore
	entered chan struct{}
	release <-chan struct{}
}

func (s *gatedSaaSFinalizer) FinalizeAsyncJobAttempt(ctx context.Context, scope models.TenantScope, id, owner string, attempt int, result models.AsyncJobAttemptResult, delivery *models.AsyncJobDeliveryResult, now time.Time) (models.AsyncJobOutcome, error) {
	close(s.entered)
	// Hold the real claimed job after HTTP acceptance, independently of the
	// HTTP deadline. The test always releases this gate before Scheduler.Stop.
	<-s.release
	return s.Store.FinalizeAsyncJobAttempt(ctx, scope, id, owner, attempt, result, delivery, now)
}

func TestSaaSConsumerVisitsTenant101BeforeRevisitingPrefix(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	for i := range 101 {
		jobs := 1
		if i == 0 {
			jobs = 2
		}
		seedSaaSConsumerTenant(t, raw, store, fmt.Sprintf("paged_%03d", i), jobs)
	}
	backend := &watchedSaaSStore{Store: store, claims: map[string]int{}}
	s := NewTenantDistributedScheduler(backend, quietLogger(), "consumer-test")
	s.integrationClient = &http.Client{Transport: webhookRoundTripFunc(func(*http.Request) (*http.Response, error) { return webhookResponse(204), nil })}
	done := startSaaSTestCallback(t, s)
	select {
	case result := <-done:
		if result.failed() {
			t.Fatal(result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("paged drain did not complete")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	last, secondPrefix := -1, -1
	prefixCount := 0
	for i, id := range backend.sequence {
		if id == "paged_100" && last < 0 {
			last = i
		}
		if id == "paged_000" {
			prefixCount++
			if prefixCount == 2 {
				secondPrefix = i
			}
		}
	}
	if last < 0 || secondPrefix < 0 || last >= secondPrefix {
		t.Fatalf("prefix revisited before tenant101: last=%d prefix2=%d sequence=%v", last, secondPrefix, backend.sequence)
	}
}

func TestSaaSConsumerDoesNotSpinBehindOnlyInflightJob(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	seedSaaSConsumerTenant(t, raw, store, "a_wait", 1)
	seedSaaSConsumerTenant(t, raw, store, "b_empty", 0)
	// Enumerate only A/B: a legacy tenant's real SQL probe could otherwise
	// occupy the pending-claim loop throughout the spin observation window.
	result, err := raw.ExecContext(t.Context(), `UPDATE tenants SET status='suspended' WHERE id=? AND status='active'`, models.LegacyTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("suspend legacy fixture tenant: affected=%d err=%v", affected, err)
	}
	backend := &watchedSaaSStore{Store: store, claims: map[string]int{}}
	probed := make(chan struct{})
	excessProbes := make(chan struct{})
	var probeOnce sync.Once
	backend.beforeClaim = func(ctx context.Context, scope models.TenantScope, count int) error {
		if scope.TenantID == "b_empty" && count > 1 {
			// B has no producer. Its first probe uses real SQL; later empty
			// probes return immediately so SQL latency cannot hide a spin.
			if count == 3 {
				close(excessProbes)
			}
			return sql.ErrNoRows
		}
		return nil
	}
	backend.afterClaim = func(scope models.TenantScope, count int, err error) {
		if scope.TenantID == "b_empty" && errors.Is(err, sql.ErrNoRows) {
			probeOnce.Do(func() { close(probed) })
		}
	}
	release := make(chan struct{})
	var once sync.Once
	releaseGate := func() { once.Do(func() { close(release) }) }
	defer releaseGate()
	gate := &gatedSaaSFinalizer{watchedSaaSStore: backend, entered: make(chan struct{}), release: release}
	s := NewTenantDistributedScheduler(gate, quietLogger(), "consumer-test")
	s.integrationClient = &http.Client{Transport: webhookRoundTripFunc(func(*http.Request) (*http.Response, error) { return webhookResponse(204), nil })}
	done := startSaaSTestCallback(t, s)
	t.Cleanup(releaseGate) // LIFO: release before the callback helper's Stop.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	waitSaaSTest(t, ctx, gate.entered, "claimed job did not reach finalization gate")
	waitSaaSTest(t, ctx, probed, "empty probe not reached")
	select {
	case <-excessProbes:
		t.Fatal("empty probes caused rescan spin: third probe before gate release")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case result := <-done:
		t.Fatalf("consumer ended before explicit gate release: %+v", result)
	default:
	}
	var status string
	if err := raw.QueryRowContext(ctx, `SELECT status FROM async_jobs WHERE tenant_id='a_wait'`).Scan(&status); err != nil || status != "processing" {
		t.Fatalf("job did not remain in flight: status=%q err=%v", status, err)
	}
	backend.mu.Lock()
	lists := backend.lists
	bClaims := backend.claims["b_empty"]
	backend.mu.Unlock()
	if bClaims < 1 || bClaims > 2 || lists > 2 {
		t.Fatalf("empty probes caused rescan spin: %d probes, %d sweeps", bClaims, lists)
	}
	releaseGate()
	select {
	case result := <-done:
		if result.failed() {
			t.Fatal(result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("drain did not finish")
	}
	if err := raw.QueryRowContext(t.Context(), `SELECT status FROM async_jobs WHERE tenant_id='a_wait'`).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("released job did not complete: status=%q err=%v", status, err)
	}
	before := lists
	if result := s.consumeSaaSJobs(); result.failed() {
		t.Fatal(result)
	}
	backend.mu.Lock()
	after := backend.lists
	backend.mu.Unlock()
	if after-before > 3 {
		t.Fatalf("empty drain rescanned: %d pages", after-before)
	}
}

func TestSaaSConsumerPriorSweepCompletionDoesNotStrandNextJob(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	seedSaaSConsumerTenant(t, raw, store, "a_race", 2)
	seedSaaSConsumerTenant(t, raw, store, "b_probe", 0)
	backend := &watchedSaaSStore{Store: store, claims: map[string]int{}}
	release := make(chan struct{})
	secondClaim := make(chan struct{})
	var releaseOnce, secondOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	backend.beforeClaim = func(ctx context.Context, scope models.TenantScope, count int) error {
		if scope.TenantID == "a_race" && count == 2 {
			secondOnce.Do(func() { close(secondClaim) })
		}
		if scope.TenantID == "b_probe" && count == 2 {
			// Pass2 skipped A while A1 was in HTTP. Complete A1 now, but
			// hold B's empty claim until the coordinator revisits A for A2.
			releaseOnce.Do(func() { close(release) })
			select {
			case <-secondClaim:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	s := NewTenantDistributedScheduler(backend, quietLogger(), "consumer-test")
	s.integrationClient = &http.Client{Transport: webhookRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-Tutor-Event-ID") == "a_race-event-000" {
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return webhookResponse(204), nil
	})}
	done := startSaaSTestCallback(t, s)
	select {
	case result := <-done:
		if result.failed() {
			t.Fatal(result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prior-sweep completion stranded A2 or waited for an unrelated claim")
	}
	var completed int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM async_jobs WHERE tenant_id='a_race' AND status='completed'`).Scan(&completed); err != nil || completed != 2 {
		t.Fatalf("A2 stranded: %d %v", completed, err)
	}
}

func TestSaaSConsumerWorkerPanicReleasesSlotAndRecoversLease(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	seedSaaSConsumerTenant(t, raw, store, "a_panic", 1)
	seedSaaSConsumerTenant(t, raw, store, "b_healthy", 3)
	s := NewTenantDistributedScheduler(store, quietLogger(), "consumer-test")
	var panicOnce atomic.Bool
	panicOnce.Store(true)
	s.integrationClient = &http.Client{Transport: webhookRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/a_panic" && panicOnce.Swap(false) {
			panic("sensitive panic must be sanitized")
		}
		return webhookResponse(204), nil
	})}
	if result := s.consumeSaaSJobs(); !result.failed() {
		t.Fatal("worker panic was not reported")
	}
	var healthy int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM async_jobs WHERE tenant_id='b_healthy' AND status='completed'`).Scan(&healthy); err != nil || healthy != 3 {
		t.Fatalf("panic leaked worker slot: %d %v", healthy, err)
	}
	s.saasNow = func() time.Time { return time.Now().UTC().Add(3 * time.Minute) }
	if result := s.consumeSaaSJobs(); result.failed() {
		t.Fatal("panic prevented subsequent drain", result)
	}
	var status string
	var attempt int
	if err := raw.QueryRow(`SELECT status,attempt_count FROM async_jobs WHERE tenant_id='a_panic'`).Scan(&status, &attempt); err != nil || status != "completed" || attempt != 2 {
		t.Fatalf("panic lease not recovered: %s %d %v", status, attempt, err)
	}
	s.Stop()
}

type heldScheduledSaaSStore struct {
	*db.Store
	entered, canceled, release chan struct{}
	acquires                   atomic.Int32
	stage                      string
	refs                       []storeport.ScheduledJobRunRef
}

func (s *heldScheduledSaaSStore) hold(ctx context.Context) {
	close(s.entered)
	<-ctx.Done()
	close(s.canceled)
	<-s.release
}
func (s *heldScheduledSaaSStore) AcquireJobRunLease(ctx context.Context, name, window, owner string, now time.Time, lease time.Duration, max int) (bool, error) {
	if name == "saas_async_jobs" {
		s.acquires.Add(1)
		if s.stage == "claim" {
			s.hold(ctx)
			return false, ctx.Err()
		}
	}
	return s.Store.AcquireJobRunLease(ctx, name, window, owner, now, lease, max)
}
func (s *heldScheduledSaaSStore) CompleteJobRun(ctx context.Context, name, window, owner string, now time.Time) (bool, error) {
	if name == "saas_async_jobs" && s.stage == "completion" {
		s.hold(ctx)
		return false, ctx.Err()
	}
	return s.Store.CompleteJobRun(ctx, name, window, owner, now)
}
func (s *heldScheduledSaaSStore) ListRunnableJobRuns(context.Context, time.Time, int) ([]storeport.ScheduledJobRunRef, error) {
	return s.refs, nil
}

func TestSaaSConsumerStopJoinsScheduledRunBeforeAndAfterDrain(t *testing.T) {
	for _, stage := range []string{"claim", "completion"} {
		t.Run(stage, func(t *testing.T) {
			_, store := saasConsumerTestStore(t)
			backend := &heldScheduledSaaSStore{Store: store, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), stage: stage}
			var release sync.Once
			defer release.Do(func() { close(backend.release) })
			s := NewTenantDistributedScheduler(backend, quietLogger(), "consumer-test")
			s.stopTimeout = 10 * time.Millisecond
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			s.cron.Stop()
			runDone := make(chan struct{})
			go func() {
				defer close(runDone)
				s.runDistributedJob("saas_async_jobs", "held-window", s.registeredDistributed["saas_async_jobs"])
			}()
			select {
			case <-backend.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("scheduled-run stage not reached")
			}
			for i := range 20 {
				s.launchSaaSRetry(fmt.Sprint("retry-", i), s.registeredDistributed["saas_async_jobs"])
				s.runDistributedJob("saas_async_jobs", fmt.Sprint("overlap-", i), s.registeredDistributed["saas_async_jobs"])
			}
			if backend.acquires.Load() != 1 {
				t.Fatal("pre-drain guard admitted extra scheduled runs", backend.acquires.Load())
			}
			stopped := make(chan struct{})
			go func() { s.Stop(); close(stopped) }()
			select {
			case <-backend.canceled:
			case <-time.After(time.Second):
				t.Fatal("Stop did not cancel scheduled-run SQL")
			}
			select {
			case <-stopped:
				t.Fatal("Stop returned before non-cooperative scheduled SQL finished")
			case <-time.After(30 * time.Millisecond):
			}
			release.Do(func() { close(backend.release) })
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("Stop did not join released SQL")
			}
			<-runDone
		})
	}
}

func TestSaaSConsumerRetryRunnerDoesNotBlockOtherRetryJobs(t *testing.T) {
	_, store := saasConsumerTestStore(t)
	backend := &heldScheduledSaaSStore{Store: store, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), stage: "claim", refs: []storeport.ScheduledJobRunRef{{Name: "saas_async_jobs", WindowKey: "retry-window"}, {Name: "other-job", WindowKey: "other-window"}}}
	s := NewTenantDistributedScheduler(backend, quietLogger(), "consumer-test")
	other := make(chan struct{})
	s.registeredDistributed["saas_async_jobs"] = s.consumeSaaSJobs
	s.registeredDistributed["other-job"] = func() scheduledJobResult { close(other); return scheduledJobSucceeded() }
	s.retryRunnableJobs()
	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("SaaS retry runner not admitted")
	}
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("SaaS drain blocked unrelated retry")
	}
	for i := range 20 {
		s.launchSaaSRetry(fmt.Sprint(i), s.consumeSaaSJobs)
	}
	if backend.acquires.Load() != 1 {
		t.Fatal("retry runner guard acquired after spawn")
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	<-backend.canceled
	close(backend.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("retry runner leaked after Stop")
	}
}

func TestSaaSConsumerStopCancelsAndJoinsHTTP(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	seedSaaSConsumerTenant(t, raw, store, "stop_http", 1)
	s := NewTenantDistributedScheduler(store, quietLogger(), "consumer-test")
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s.integrationClient = &http.Client{Transport: webhookRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		close(canceled)
		<-release
		return nil, req.Context().Err()
	})}
	done := startSaaSTestCallback(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP not started")
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP context not canceled")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while HTTP was still active")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("HTTP worker not joined")
	}
	<-done
}
