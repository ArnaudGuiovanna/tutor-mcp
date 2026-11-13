// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"

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
	saasDrain         *saasDrainState
	saasNow           func() time.Time
	saasJitter        func() float64
	stopSignal        *sync.Once

	// stopTimeout caps the legacy cron drain after the strict SaaS join.
	// It defaults to 25s and does not bound a non-cooperative SaaS backend.
	// Overridable via test-only constructors so timeout-path tests don't
	// need to wait the full production budget.
	stopTimeout time.Duration

	// stopCh signals the webhook and consolidation pools to stop admitting
	// work. A nil channel in struct-literal test fixtures leaves this signal
	// inactive; it does not affect persisted delivery backoff.
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
	// tenantDBPhase forbids network dispatch from a child holding an RLS
	// transaction, including future jobs accidentally registered that way.
	tenantDBPhase bool
	// Constructor identity survives tenant child copies. Nil tenantStore is
	// also used by institutional children and must never imply legacy scope.
	legacyNarrativeScope bool

	// options is the runtime configuration supplied once by the caller
	// (see Configure). Zero fields fall back to their documented defaults.
	options SchedulerOptions
}

// DefaultConsolidationDeliveryTimeout is how long a delivered consolidation
// request may stay unanswered before the scheduler requeues it.
const DefaultConsolidationDeliveryTimeout = 30 * time.Minute

// SchedulerOptions is the runtime configuration of the scheduler. The engine
// reads no environment variable: the process entry point parses its
// configuration once and passes it here.
type SchedulerOptions struct {
	// ConsolidationDeliveryTimeout bounds how long a delivered consolidation
	// may remain unanswered before it is requeued. Zero or negative selects
	// DefaultConsolidationDeliveryTimeout.
	ConsolidationDeliveryTimeout time.Duration
}

// Configure applies runtime options. Call it before Start; tenant children
// created afterwards inherit the options.
func (s *Scheduler) Configure(options SchedulerOptions) {
	s.options = options
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
		store:                store,
		legacyNarrativeScope: true,
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
		saasDrain:   newSaaSDrainState(),
		stopSignal:  &sync.Once{},

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

// NewDistributedScheduler builds the same scheduler as NewScheduler but in
// distributed mode: at most one fleet instance owns each scheduled run slot.
// Live workers heartbeat, expired leases are recovered, and poison jobs become
// explicit dead letters after a bounded number of attempts.
func NewDistributedScheduler(store storeport.Store, logger *slog.Logger) *Scheduler {
	s := NewScheduler(store, logger)
	s.distributed = true
	return s
}

// NewTenantDistributedScheduler keeps global lease election and binds every
// database phase to an explicit tenant transaction. Network jobs commit each
// phase independently and hold no transaction during HTTP. The worker role
// never owns tables and never receives BYPASSRLS.
func NewTenantDistributedScheduler(store storeport.Store, logger *slog.Logger, actorID string) *Scheduler {
	s := NewDistributedScheduler(store, logger)
	s.legacyNarrativeScope = false
	tenantStore, ok := store.(tenantSchedulerStore)
	if ok {
		s.tenantStore = tenantStore
	}
	s.worker = models.WorkerPrincipal{ActorID: actorID}
	return s
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
		s.scheduledIndependentTenantJob("olm", func(child *Scheduler) scheduledJobResult { return child.sendOLM() })); err != nil {
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
		s.scheduledIndependentTenantJob("motivation", func(child *Scheduler) scheduledJobResult { return child.sendDailyMotivation() })); err != nil {
		return fmt.Errorf("add daily motivation job: %w", err)
	}
	// End-of-day recap: once at 21h UTC.
	if err := s.schedule("recap", "0 21 * * *", 24*time.Hour,
		s.scheduledIndependentTenantJob("recap", func(child *Scheduler) scheduledJobResult { return child.sendDailyRecap() })); err != nil {
		return fmt.Errorf("add daily recap job: %w", err)
	}
	// Mirror messages: once at 12h UTC. Dispatches any metacognitive mirror
	// nudges queued during sessions (#59). One per learner per day, dedup'd
	// at the alert layer (MIRROR_MESSAGE) so an in-session enqueue + the
	// scheduler tick can't double-fire.
	if err := s.schedule("mirror", "0 12 * * *", 24*time.Hour,
		s.scheduledIndependentTenantJob("mirror", func(child *Scheduler) scheduledJobResult { return child.sendMirrorMessages() })); err != nil {
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
		s.scheduledIndependentTenantJob("metacog_alerts", func(child *Scheduler) scheduledJobResult { return child.dispatchMetacognitiveAlerts() })); err != nil {
		return fmt.Errorf("add metacognitive alerts job: %w", err)
	}
	if s.tenantStore != nil {
		if err := s.schedule("saas_outbox_relay", "*/1 * * * *", time.Minute,
			s.scheduledIndependentTenantJob("saas_outbox_relay", func(child *Scheduler) scheduledJobResult {
				return child.relaySaaSOutbox()
			})); err != nil {
			return fmt.Errorf("add SaaS outbox relay: %w", err)
		}
		if err := s.schedule("saas_async_jobs", "*/1 * * * *", time.Minute, s.consumeSaaSJobs); err != nil {
			return fmt.Errorf("add SaaS async consumer: %w", err)
		}
		if err := s.schedule("institution_statistics", "7 * * * *", time.Hour,
			s.scheduledIndependentTenantJob("institution_statistics", func(child *Scheduler) scheduledJobResult {
				return child.refreshInstitutionStatistics()
			})); err != nil {
			return fmt.Errorf("add institution statistics: %w", err)
		}
		if err := s.schedule("institution_collective_weights", "21 */6 * * *", 6*time.Hour,
			s.scheduledIndependentTenantJob("institution_collective_weights", func(child *Scheduler) scheduledJobResult {
				return child.refreshCollectiveWeights()
			})); err != nil {
			return fmt.Errorf("add institution collective weights: %w", err)
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
	s.logger.Info("scheduler started", "jobs", "olm(13h), consolidation(13h30), consolidation_timeout(5m), motivation(8h), recap(21h), mirror(12h), cleanup(1h), metacog(30m), saas relay/async(1m worker), institution statistics(1h), collective weights(6h)", "distributed", s.distributed)
	return nil
}

// Stop cancels and joins the SaaS consumer before closing any database. Its
// drivers/transports must honor context cancellation; a non-cooperative backend
// can delay this join. The subsequent legacy cron drain has a 25s default cap.
//
// robfig/cron/v3's Stop() returns a context.Context that is cancelled only
// once all running jobs have finished. Discarding that context (the previous
// behaviour) made Stop() effectively non-blocking, so the deferred chain in
// main.go could run database.Close() while a cron tick was mid-iteration —
// producing "sql: database is closed" mid-loop and silent data loss on
// webhook_message_queue + scheduled_alerts. See issue #123.
func (s *Scheduler) Stop() {
	// Cancel first, then join even a drain invoked outside cron. No database
	// or HTTP work owned by this consumer may survive Store.Close.
	if s.saasDrain != nil {
		s.saasDrain.stop()
	}
	// Signal webhook/consolidation pool admission to stop. cron.Stop below
	// stops scheduling, including retry polling; the SaaS consumer has already
	// been canceled and joined. Guard against double Stop() and a nil stopCh.
	if s.stopCh != nil {
		if s.stopSignal != nil {
			s.stopSignal.Do(func() { close(s.stopCh) })
		} else {
			select {
			case <-s.stopCh:
				// already closed
			default:
				close(s.stopCh)
			}
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

//
// The scheduler no longer composes motivational text. Claude authors messages
// during sessions via queue_webhook_message, the scheduler dispatches from the
// queue. If the queue is empty for a learner, we fall back to a sober one-liner
// (no KPIs, no analytics bulletin tone).
