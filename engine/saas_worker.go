// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"tutor-mcp/models"
	"tutor-mcp/observability"
	storeport "tutor-mcp/store"
)

const (
	saasWorkerBatchSize          = 100
	saasWorkerLease              = 2 * time.Minute
	institutionStatisticsTimeout = 10 * time.Minute
)

type saasWorkerStore interface {
	RelayOutboxEvent(context.Context, models.TenantScope, string, time.Time, time.Duration) (bool, error)
	ClaimAsyncJob(context.Context, models.TenantScope, string, time.Time, time.Duration) (*models.AsyncJob, error)
	storeport.AsyncJobAttemptStore
	BeginIntegrationDelivery(context.Context, models.TenantScope, models.TenantWebhookJob, int, time.Time) (*models.IntegrationDelivery, error)
	PrepareTenantWebhook(context.Context, models.TenantScope, string, string, string, []byte, time.Time) (*models.SignedWebhook, error)
	ExpireEntitlementReservations(context.Context, models.TenantScope, time.Time, int) (int, error)
	CompleteTenantDSARExport(context.Context, models.TenantScope, models.WorkerPrincipal, string) error
	ProcessTenantDSARErasureBatch(context.Context, models.TenantScope, models.WorkerPrincipal, string, int, time.Time) (bool, int64, error)
	ParkTenantDSARJob(context.Context, models.TenantScope, string, string, string, int, time.Time) error
}

func (s *Scheduler) relaySaaSOutbox() scheduledJobResult {
	backend, ok := s.store.(saasWorkerStore)
	if !ok || s.currentTenantScope == nil {
		return scheduledJobFailed("saas_worker_store_unavailable")
	}
	for i := 0; i < saasWorkerBatchSize; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
		claimed, err := backend.RelayOutboxEvent(ctx, *s.currentTenantScope, s.owner, time.Now().UTC(), saasWorkerLease)
		cancel()
		if err != nil {
			return scheduledJobFailed("saas_outbox_relay_failed")
		}
		if !claimed {
			return scheduledJobSucceeded()
		}
	}
	return scheduledJobSucceeded()
}

func (s *Scheduler) expireSaaSReservations() scheduledJobResult {
	backend, ok := s.store.(saasWorkerStore)
	if !ok || s.currentTenantScope == nil {
		return scheduledJobFailed("saas_worker_store_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
	defer cancel()
	if _, err := backend.ExpireEntitlementReservations(ctx, *s.currentTenantScope, time.Now().UTC(), saasWorkerBatchSize); err != nil {
		return scheduledJobFailed("saas_reservation_expiry_failed")
	}
	return scheduledJobSucceeded()
}

// refreshInstitutionStatistics recomputes the anonymous cohort snapshots. The
// store upserts per cohort, so a retry or a concurrent worker converges.
func (s *Scheduler) refreshInstitutionStatistics() scheduledJobResult {
	backend, ok := s.store.(storeport.StatisticsWorkerStore)
	if !ok || s.currentTenantScope == nil {
		return scheduledJobFailed("institution_statistics_store_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), institutionStatisticsTimeout)
	defer cancel()
	if _, err := backend.RecomputeInstitutionStatistics(ctx, *s.currentTenantScope, time.Now().UTC()); err != nil {
		return scheduledJobFailed("institution_statistics_failed")
	}
	return scheduledJobSucceeded()
}

// refreshCollectiveWeights publishes new collective BKT weight versions. The
// worker only reads learner state; weights reach learners in their own
// interaction transaction.
func (s *Scheduler) refreshCollectiveWeights() scheduledJobResult {
	backend, ok := s.store.(storeport.CollectiveWeightWorkerStore)
	if !ok || s.currentTenantScope == nil {
		return scheduledJobFailed("collective_weights_store_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), institutionStatisticsTimeout)
	defer cancel()
	if _, err := backend.RecomputeCollectiveWeights(ctx, *s.currentTenantScope, time.Now().UTC()); err != nil {
		return scheduledJobFailed("collective_weights_failed")
	}
	return scheduledJobSucceeded()
}

// Every active attempt is bounded well below the two-minute lease, including
// short database transitions. No goroutine may keep working after its context.
func (s *Scheduler) consumeSaaSJob(ctx context.Context, backend saasWorkerStore, scope models.TenantScope, job *models.AsyncJob) (outcome models.AsyncJobOutcome, resultErr error) {
	if job == nil || job.TenantID != scope.TenantID {
		return "", fmt.Errorf("invalid async worker job")
	}
	queueName := "tenant_async_job"
	if job.Kind == "tenant_dsar" || job.Kind == "tenant_webhook_delivery" {
		queueName = job.Kind
	}
	defer func() {
		state := string(outcome)
		if state == "" {
			state = "failed"
		}
		if errors.Is(resultErr, storeport.ErrTenantDSARBlocked) {
			state = "blocked"
		} else if resultErr != nil {
			state = "failed"
		} else if outcome == models.AsyncJobCompleted {
			state = "succeeded"
		}
		observability.RecordQueueEvent(ctx, scope.TenantID, queueName, state)
	}()
	observability.RecordQueueLag(ctx, scope.TenantID, queueName, s.saasTimeNow().Sub(job.CreatedAt))
	finalize := func(outcome models.AsyncJobOutcome, cause string, delivery *models.AsyncJobDeliveryResult, retryAfter string) (models.AsyncJobOutcome, error) {
		now := s.saasTimeNow()
		result := models.AsyncJobAttemptResult{Outcome: outcome, ErrorCode: cause}
		if outcome == models.AsyncJobRetry {
			result.AvailableAt = s.saasRetryAt(now, job.AttemptCount, retryAfter)
		}
		return backend.FinalizeAsyncJobAttempt(ctx, scope, job.ID, job.LeaseOwner, job.AttemptCount, result, delivery, now)
	}
	if job.Kind == "tenant_dsar" {
		return s.consumeTenantDSARJob(ctx, backend, scope, job, finalize)
	}
	if job.Kind != "tenant_webhook_delivery" {
		return finalize(models.AsyncJobDeadLetter, "unsupported_job_kind", nil, "")
	}
	var payload models.TenantWebhookJob
	if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.IntegrationID == "" || payload.EventID == "" || payload.EventType == "" || !json.Valid(payload.Payload) {
		return finalize(models.AsyncJobDeadLetter, "invalid_job_payload", nil, "")
	}
	delivery, err := backend.BeginIntegrationDelivery(ctx, scope, payload, job.AttemptCount, s.saasTimeNow())
	if err != nil {
		outcome := models.AsyncJobRetry
		if errors.Is(err, sql.ErrNoRows) {
			outcome = models.AsyncJobDeadLetter
		}
		return finalize(outcome, "delivery_begin_failed", nil, "")
	}
	result := &models.AsyncJobDeliveryResult{DeliveryID: delivery.ID}
	if delivery.Status == "delivered" {
		return finalize(models.AsyncJobCompleted, "", result, "")
	}
	signed, err := backend.PrepareTenantWebhook(ctx, scope, payload.IntegrationID, payload.EventID, payload.EventType, payload.Payload, s.saasTimeNow())
	if err != nil {
		outcome := models.AsyncJobRetry
		if errors.Is(err, storeport.ErrWebhookPermanent) {
			outcome = models.AsyncJobDeadLetter
		}
		return finalize(outcome, "integration_unavailable", result, "")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, signed.EndpointURL, bytes.NewReader(signed.Payload))
	if err != nil {
		return finalize(models.AsyncJobDeadLetter, "request_build_failed", result, "")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tutor-mcp-webhook/1")
	req.Header.Set("X-Tutor-Event-ID", payload.EventID)
	req.Header.Set("X-Tutor-Event-Type", payload.EventType)
	req.Header.Set("X-Tutor-Timestamp", strconv.FormatInt(signed.Timestamp, 10))
	req.Header.Set("X-Tutor-Signature", signed.Signature)
	req.Header.Set("X-Tutor-Secret-Version", strconv.FormatInt(signed.SecretVersion, 10))
	response, err := s.integrationClient.Do(req)
	if err != nil {
		return finalize(models.AsyncJobRetry, "http_transport_failed", result, "")
	}
	defer response.Body.Close()
	code := response.StatusCode
	if code < 100 || code > 599 {
		return finalize(models.AsyncJobDeadLetter, "http_invalid_status", result, "")
	}
	result.ResponseCode = &code
	// Acceptance is determined by the status line. Reading/hashing an optional
	// body can fail after acceptance and must never trigger another send.
	if code >= 200 && code < 300 {
		return finalize(models.AsyncJobCompleted, "", result, "")
	}
	if code == 408 || code == 425 || code == 429 || (code >= 500 && code <= 599) {
		return finalize(models.AsyncJobRetry, "http_retryable", result, response.Header.Get("Retry-After"))
	}
	return finalize(models.AsyncJobDeadLetter, "http_permanent", result, "")
}

type asyncAttemptFinalizer func(models.AsyncJobOutcome, string, *models.AsyncJobDeliveryResult, string) (models.AsyncJobOutcome, error)

func (s *Scheduler) consumeTenantDSARJob(ctx context.Context, backend saasWorkerStore, scope models.TenantScope, job *models.AsyncJob, finalize asyncAttemptFinalizer) (models.AsyncJobOutcome, error) {
	var payload models.TenantDSARJob
	if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.RequestID == "" || (payload.Kind != "export" && payload.Kind != "erase") || payload.RequestID != job.IdempotencyKey {
		return finalize(models.AsyncJobDeadLetter, "invalid_dsar_job", nil, "")
	}
	var err error
	completed := payload.Kind == "export"
	if payload.Kind == "export" {
		err = backend.CompleteTenantDSARExport(ctx, scope, s.worker, payload.RequestID)
	} else {
		// Yield normal progress after a small quantum; a healthy erasure of
		// arbitrary size must not consume the technical retry budget.
		for batch := 0; batch < 8 && !completed; batch++ {
			if err = ctx.Err(); err != nil {
				break
			}
			completed, _, err = backend.ProcessTenantDSARErasureBatch(ctx, scope, s.worker, payload.RequestID, 500, s.saasTimeNow())
			if err != nil {
				break
			}
		}
	}
	if errors.Is(err, storeport.ErrTenantDSARBlocked) {
		if parkErr := backend.ParkTenantDSARJob(ctx, scope, job.ID, payload.RequestID, job.LeaseOwner, job.AttemptCount, s.saasTimeNow()); parkErr != nil {
			return "", parkErr
		}
		return "", err
	}
	if err != nil {
		return finalize(models.AsyncJobRetry, "dsar_processing_failed", nil, "")
	}
	if !completed {
		return finalize(models.AsyncJobContinue, "", nil, "")
	}
	return finalize(models.AsyncJobCompleted, "", nil, "")
}
