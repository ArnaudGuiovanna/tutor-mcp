// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"tutor-mcp/models"
	"tutor-mcp/observability"
)

const (
	saasWorkerBatchSize       = 100
	saasWorkerLease           = 2 * time.Minute
	maxIntegrationResponseLen = 64 << 10
)

type saasWorkerStore interface {
	RelayOutboxEvent(context.Context, models.TenantScope, string, time.Time, time.Duration) (bool, error)
	ClaimAsyncJob(context.Context, models.TenantScope, string, time.Time, time.Duration) (*models.AsyncJob, error)
	FinishAsyncJob(context.Context, models.TenantScope, string, string, bool, time.Time, string) error
	BeginIntegrationDelivery(context.Context, models.TenantScope, models.TenantWebhookJob, int, time.Time) (*models.IntegrationDelivery, error)
	FinishIntegrationDelivery(context.Context, models.TenantScope, string, string, *int, string, string) error
	PrepareTenantWebhook(context.Context, models.TenantScope, string, string, string, []byte, time.Time) (*models.SignedWebhook, error)
	ExpireEntitlementReservations(context.Context, models.TenantScope, time.Time, int) (int, error)
	CompleteTenantDSARExport(context.Context, models.TenantScope, models.WorkerPrincipal, string) error
	ProcessTenantDSARErasureBatch(context.Context, models.TenantScope, models.WorkerPrincipal, string, int, time.Time) (bool, int64, error)
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

func (s *Scheduler) consumeSaaSJobs() scheduledJobResult {
	backend, ok := s.store.(saasWorkerStore)
	if !ok || s.currentTenantScope == nil {
		return scheduledJobFailed("saas_worker_store_unavailable")
	}
	result := scheduledJobSucceeded()
	for i := 0; i < saasWorkerBatchSize; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
		job, err := backend.ClaimAsyncJob(ctx, *s.currentTenantScope, s.owner, time.Now().UTC(), saasWorkerLease)
		cancel()
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			result.recordFailure("saas_job_claim_failed")
			break
		}
		if err := s.consumeSaaSJob(backend, job); err != nil {
			result.recordFailure("saas_job_partial_failure")
		}
	}
	return result
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

func (s *Scheduler) consumeSaaSJob(backend saasWorkerStore, job *models.AsyncJob) (resultErr error) {
	scope := *s.currentTenantScope
	if job == nil {
		return fmt.Errorf("async worker returned a nil job")
	}
	queueName := "tenant_async_job"
	if job.Kind == "tenant_dsar" || job.Kind == "tenant_webhook_delivery" {
		queueName = job.Kind
	}
	defer func() {
		observability.RecordQueueEvent(context.Background(), scope.TenantID, queueName, queueOutcome(resultErr))
	}()
	observability.RecordQueueLag(context.Background(), scope.TenantID, queueName, time.Since(job.CreatedAt))
	if job.Kind == "tenant_dsar" {
		return s.consumeTenantDSARJob(backend, scope, job)
	}
	if job.Kind != "tenant_webhook_delivery" {
		return backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, false,
			time.Now().UTC().Add(integrationRetryDelay(job.AttemptCount)), "unsupported_job_kind")
	}
	var payload models.TenantWebhookJob
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil || payload.IntegrationID == "" ||
		payload.EventID == "" || payload.EventType == "" || !json.Valid(payload.Payload) {
		return backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, false,
			time.Now().UTC().Add(integrationRetryDelay(job.AttemptCount)), "invalid_job_payload")
	}

	now := time.Now().UTC()
	delivery, err := backend.BeginIntegrationDelivery(context.Background(), scope, payload, job.AttemptCount, now)
	if err != nil {
		_ = backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, false,
			now.Add(integrationRetryDelay(job.AttemptCount)), "delivery_begin_failed")
		return err
	}
	// A prior attempt was durably accepted. This closes the crash window
	// between recording the 2xx delivery and completing the async job.
	if delivery.Status == "delivered" {
		return backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, true, time.Time{}, "")
	}

	signed, err := backend.PrepareTenantWebhook(context.Background(), scope, payload.IntegrationID,
		payload.EventID, payload.EventType, payload.Payload, now)
	if err != nil {
		return s.failIntegrationAttempt(backend, scope, job, delivery, nil, "integration_unavailable")
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, signed.EndpointURL, bytes.NewReader(signed.Payload))
	if err != nil {
		return s.failIntegrationAttempt(backend, scope, job, delivery, nil, "request_build_failed")
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
		return s.failIntegrationAttempt(backend, scope, job, delivery, nil, "http_transport_failed")
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxIntegrationResponseLen+1))
	responseHash := sha256.Sum256(responseBody)
	hash := hex.EncodeToString(responseHash[:])
	code := response.StatusCode
	if readErr != nil {
		return s.failIntegrationAttemptWithHash(backend, scope, job, delivery, &code, hash, "response_read_failed")
	}
	if len(responseBody) > maxIntegrationResponseLen {
		return s.failIntegrationAttemptWithHash(backend, scope, job, delivery, &code, hash, "response_too_large")
	}
	if code < http.StatusOK || code >= http.StatusMultipleChoices {
		return s.failIntegrationAttemptWithHash(backend, scope, job, delivery, &code, hash, "http_non_2xx")
	}
	if err := backend.FinishIntegrationDelivery(context.Background(), scope, delivery.ID, "delivered", &code, hash, ""); err != nil {
		return err
	}
	return backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, true, time.Time{}, "")
}

func queueOutcome(err error) string {
	if err != nil {
		return "failed"
	}
	return "succeeded"
}

func (s *Scheduler) consumeTenantDSARJob(backend saasWorkerStore, scope models.TenantScope, job *models.AsyncJob) error {
	var payload models.TenantDSARJob
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil || payload.RequestID == "" ||
		(payload.Kind != "export" && payload.Kind != "erase") {
		return backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, false,
			time.Now().UTC().Add(integrationRetryDelay(job.AttemptCount)), "invalid_dsar_job")
	}
	var err error
	if payload.Kind == "export" {
		err = backend.CompleteTenantDSARExport(context.Background(), scope, s.worker, payload.RequestID)
	} else {
		completed := false
		for batch := 0; batch < 256 && !completed; batch++ {
			completed, _, err = backend.ProcessTenantDSARErasureBatch(context.Background(), scope,
				s.worker, payload.RequestID, 500, time.Now().UTC())
			if err != nil {
				break
			}
		}
		if err == nil && !completed {
			err = fmt.Errorf("DSAR erasure batch budget exhausted")
		}
	}
	if err != nil {
		_ = backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, false,
			time.Now().UTC().Add(integrationRetryDelay(job.AttemptCount)), "dsar_processing_failed")
		return err
	}
	return backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, true, time.Time{}, "")
}

func (s *Scheduler) failIntegrationAttempt(backend saasWorkerStore, scope models.TenantScope, job *models.AsyncJob, delivery *models.IntegrationDelivery, code *int, cause string) error {
	return s.failIntegrationAttemptWithHash(backend, scope, job, delivery, code, "", cause)
}

func (s *Scheduler) failIntegrationAttemptWithHash(backend saasWorkerStore, scope models.TenantScope, job *models.AsyncJob, delivery *models.IntegrationDelivery, code *int, responseHash, cause string) error {
	deliveryStatus := "failed"
	if job.AttemptCount >= job.MaxAttempts {
		deliveryStatus = "dead_letter"
	}
	if err := backend.FinishIntegrationDelivery(context.Background(), scope, delivery.ID, deliveryStatus, code, responseHash, cause); err != nil {
		return err
	}
	if err := backend.FinishAsyncJob(context.Background(), scope, job.ID, s.owner, false,
		time.Now().UTC().Add(integrationRetryDelay(job.AttemptCount)), cause); err != nil {
		return err
	}
	return fmt.Errorf("integration delivery failed: %s", cause)
}

func integrationRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		attempt = 7
	}
	return time.Minute * time.Duration(1<<(attempt-1))
}
