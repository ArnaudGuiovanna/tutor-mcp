// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var (
	ErrQuotaExceeded = errors.New("tenant quota exceeded")
	ErrLeaseLost     = errors.New("worker lease lost")
)

func (s *Store) appendOutboxEvent(ctx context.Context, event models.OutboxEvent) error {
	if event.TenantID == "" || event.AggregateType == "" || event.AggregateID == "" ||
		event.EventType == "" || event.IdempotencyKey == "" || !json.Valid([]byte(event.PayloadJSON)) {
		return fmt.Errorf("append outbox event: invalid event")
	}
	if event.ID == "" {
		id, err := generateID()
		if err != nil {
			return err
		}
		event.ID = id
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if event.AvailableAt.IsZero() {
		event.AvailableAt = event.CreatedAt
	}
	_, err := s.exec(ctx, `INSERT INTO outbox_events
		(id, tenant_id, aggregate_type, aggregate_id, event_type, idempotency_key,
		 payload_json, status, available_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
		ON CONFLICT DO NOTHING`, event.ID, event.TenantID, event.AggregateType,
		event.AggregateID, event.EventType, event.IdempotencyKey, event.PayloadJSON,
		event.AvailableAt.UTC(), event.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("append outbox event: %w", err)
	}
	return nil
}

// AppendOutboxEvent is the public scoped boundary. Business mutations should
// call appendOutboxEvent on their existing transaction so state and event
// commit atomically.
func (s *Store) AppendOutboxEvent(ctx context.Context, scope models.TenantScope, event models.OutboxEvent) error {
	if err := scope.Validate(); err != nil || event.TenantID != scope.TenantID {
		return fmt.Errorf("append outbox event: %w", storeport.ErrInvalidPrincipal)
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		return scoped.(*Store).appendOutboxEvent(txCtx, event)
	})
}

func (s *Store) ClaimOutboxEvent(ctx context.Context, scope models.TenantScope, workerID string, now time.Time, lease time.Duration) (*models.OutboxEvent, error) {
	if err := scope.Validate(); err != nil || workerID == "" || lease <= 0 {
		return nil, fmt.Errorf("claim outbox event: invalid scope, worker or lease")
	}
	var claimed *models.OutboxEvent
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		query := `SELECT id, aggregate_type, aggregate_id, event_type, idempotency_key,
			payload_json, attempt_count, available_at, created_at
			FROM outbox_events
			WHERE tenant_id = ? AND available_at <= ?
			  AND (status = 'pending' OR (status = 'processing' AND lease_expires_at <= ?))
			ORDER BY created_at, id LIMIT 1`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE SKIP LOCKED`
		}
		candidate := &models.OutboxEvent{TenantID: scope.TenantID}
		if err := txs.queryRow(txCtx, query, scope.TenantID, now.UTC(), now.UTC()).Scan(
			&candidate.ID, &candidate.AggregateType, &candidate.AggregateID,
			&candidate.EventType, &candidate.IdempotencyKey, &candidate.PayloadJSON,
			&candidate.AttemptCount, &candidate.AvailableAt, &candidate.CreatedAt); err != nil {
			return err
		}
		expires := now.UTC().Add(lease)
		result, err := txs.exec(txCtx, `UPDATE outbox_events
			SET status = 'processing', attempt_count = attempt_count + 1,
			    lease_owner = ?, lease_expires_at = ?, last_error = ''
			WHERE tenant_id = ? AND id = ?
			  AND (status = 'pending' OR (status = 'processing' AND lease_expires_at <= ?))`,
			workerID, expires, scope.TenantID, candidate.ID, now.UTC())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
		candidate.Status = "processing"
		candidate.AttemptCount++
		candidate.LeaseOwner = workerID
		candidate.LeaseExpiresAt = &expires
		claimed = candidate
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *Store) FinishOutboxEvent(ctx context.Context, scope models.TenantScope, eventID, workerID string, delivered bool, retryAt time.Time, cause string) error {
	if err := scope.Validate(); err != nil || eventID == "" || workerID == "" {
		return fmt.Errorf("finish outbox event: invalid input")
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		now := time.Now().UTC()
		var result sql.Result
		var err error
		if delivered {
			result, err = txs.exec(txCtx, `UPDATE outbox_events
				SET status = 'delivered', delivered_at = ?, lease_owner = '', lease_expires_at = NULL
				WHERE tenant_id = ? AND id = ? AND status = 'processing' AND lease_owner = ?`,
				now, scope.TenantID, eventID, workerID)
		} else {
			if retryAt.IsZero() {
				retryAt = now.Add(time.Minute)
			}
			result, err = txs.exec(txCtx, `UPDATE outbox_events
				SET status = CASE WHEN attempt_count >= 8 THEN 'dead_letter' ELSE 'pending' END,
				    available_at = ?, last_error = ?, lease_owner = '', lease_expires_at = NULL
				WHERE tenant_id = ? AND id = ? AND status = 'processing' AND lease_owner = ?`,
				retryAt.UTC(), cause, scope.TenantID, eventID, workerID)
		}
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}

func (s *Store) EnqueueAsyncJob(ctx context.Context, scope models.TenantScope, job models.AsyncJob) error {
	if err := scope.Validate(); err != nil || job.TenantID != scope.TenantID || job.Kind == "" ||
		job.IdempotencyKey == "" || !json.Valid([]byte(job.PayloadJSON)) {
		return fmt.Errorf("enqueue job: invalid job")
	}
	if job.ID == "" {
		id, err := generateID()
		if err != nil {
			return err
		}
		job.ID = id
	}
	if job.MaxAttempts <= 0 {
		job.MaxAttempts = 8
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if job.AvailableAt.IsZero() {
		job.AvailableAt = job.CreatedAt
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		_, err := scoped.(*Store).exec(txCtx, `INSERT INTO async_jobs
			(id, tenant_id, kind, idempotency_key, payload_json, status,
			 max_attempts, available_at, created_at)
			VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)
			ON CONFLICT DO NOTHING`, job.ID, scope.TenantID, job.Kind,
			job.IdempotencyKey, job.PayloadJSON, job.MaxAttempts,
			job.AvailableAt.UTC(), job.CreatedAt.UTC())
		return err
	})
}

func (s *Store) ClaimAsyncJob(ctx context.Context, scope models.TenantScope, workerID string, now time.Time, lease time.Duration) (*models.AsyncJob, error) {
	if err := scope.Validate(); err != nil || workerID == "" || lease <= 0 {
		return nil, fmt.Errorf("claim job: invalid scope, worker or lease")
	}
	var claimed *models.AsyncJob
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		query := `SELECT id, kind, idempotency_key, payload_json, attempt_count,
			max_attempts, available_at, created_at FROM async_jobs
			WHERE tenant_id = ? AND available_at <= ?
			  AND (status = 'pending' OR (status = 'processing' AND lease_expires_at <= ?))
			  AND attempt_count < max_attempts ORDER BY created_at, id LIMIT 1`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE SKIP LOCKED`
		}
		candidate := &models.AsyncJob{TenantID: scope.TenantID}
		if err := txs.queryRow(txCtx, query, scope.TenantID, now.UTC(), now.UTC()).Scan(
			&candidate.ID, &candidate.Kind, &candidate.IdempotencyKey, &candidate.PayloadJSON,
			&candidate.AttemptCount, &candidate.MaxAttempts, &candidate.AvailableAt,
			&candidate.CreatedAt); err != nil {
			return err
		}
		expires := now.UTC().Add(lease)
		result, err := txs.exec(txCtx, `UPDATE async_jobs
			SET status = 'processing', attempt_count = attempt_count + 1,
			    lease_owner = ?, lease_expires_at = ?, heartbeat_at = ?, last_error = ''
			WHERE tenant_id = ? AND id = ?
			  AND (status = 'pending' OR (status = 'processing' AND lease_expires_at <= ?))`,
			workerID, expires, now.UTC(), scope.TenantID, candidate.ID, now.UTC())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
		candidate.Status, candidate.LeaseOwner = "processing", workerID
		candidate.AttemptCount++
		candidate.LeaseExpiresAt, candidate.HeartbeatAt = &expires, &now
		claimed = candidate
		return nil
	})
	return claimed, err
}

func (s *Store) HeartbeatAsyncJob(ctx context.Context, scope models.TenantScope, jobID, workerID string, now time.Time, lease time.Duration) error {
	if err := scope.Validate(); err != nil || jobID == "" || workerID == "" || lease <= 0 {
		return fmt.Errorf("heartbeat job: invalid input")
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		result, err := scoped.(*Store).exec(txCtx, `UPDATE async_jobs
			SET heartbeat_at = ?, lease_expires_at = ?
			WHERE tenant_id = ? AND id = ? AND status = 'processing'
			  AND lease_owner = ? AND lease_expires_at > ?`, now.UTC(), now.UTC().Add(lease),
			scope.TenantID, jobID, workerID, now.UTC())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}

func (s *Store) FinishAsyncJob(ctx context.Context, scope models.TenantScope, jobID, workerID string, succeeded bool, retryAt time.Time, cause string) error {
	if err := scope.Validate(); err != nil || jobID == "" || workerID == "" {
		return fmt.Errorf("finish job: invalid input")
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		now := time.Now().UTC()
		var result sql.Result
		var err error
		if succeeded {
			result, err = txs.exec(txCtx, `UPDATE async_jobs
				SET status = 'completed', completed_at = ?, lease_owner = '', lease_expires_at = NULL
				WHERE tenant_id = ? AND id = ? AND status = 'processing' AND lease_owner = ?`,
				now, scope.TenantID, jobID, workerID)
		} else {
			if retryAt.IsZero() {
				retryAt = now.Add(time.Minute)
			}
			result, err = txs.exec(txCtx, `UPDATE async_jobs
				SET status = CASE WHEN attempt_count >= max_attempts THEN 'dead_letter' ELSE 'pending' END,
				    available_at = ?, last_error = ?, lease_owner = '', lease_expires_at = NULL
				WHERE tenant_id = ? AND id = ? AND status = 'processing' AND lease_owner = ?`,
				retryAt.UTC(), cause, scope.TenantID, jobID, workerID)
		}
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}

func (s *Store) ConfigureEntitlement(ctx context.Context, actor models.Principal, key string, limit int64, start, end time.Time) error {
	if !actor.Authorize(models.PermissionBillingManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		key == "" || limit < 0 || !end.After(start) {
		return fmt.Errorf("configure entitlement: invalid authorization or input")
	}
	now := time.Now().UTC()
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		_, err := txs.exec(txCtx, `INSERT INTO tenant_entitlements
			(tenant_id, entitlement_key, hard_limit, used_value, reserved_value,
			 period_start, period_end, version, updated_at)
			VALUES (?, ?, ?, 0, 0, ?, ?, 1, ?)
			ON CONFLICT (tenant_id, entitlement_key) DO UPDATE SET
			 hard_limit = excluded.hard_limit, period_start = excluded.period_start,
			 period_end = excluded.period_end, version = tenant_entitlements.version + 1,
			 updated_at = excluded.updated_at`, actor.TenantID, key, limit, start.UTC(), end.UTC(), now)
		if err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "entitlement.configure", TargetType: "entitlement", TargetID: key,
		})
	})
}

// RecordUsage is an idempotent quota reservation and immutable usage append.
// The conditional UPDATE and event INSERT live in one transaction: retries do
// not charge twice, and an exceeded quota records nothing.
func (s *Store) RecordUsage(ctx context.Context, scope models.TenantScope, event models.UsageEvent) (bool, error) {
	if err := scope.Validate(); err != nil || event.TenantID != scope.TenantID ||
		event.EventKey == "" || event.Metric == "" || event.Quantity < 0 ||
		event.SourceType == "" || event.SourceID == "" || !json.Valid([]byte(event.DimensionsJSON)) {
		return false, fmt.Errorf("record usage: invalid event")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	inserted := false
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var existing int
		if err := txs.queryRow(txCtx, `SELECT COUNT(*) FROM usage_events
			WHERE tenant_id = ? AND event_key = ?`, scope.TenantID, event.EventKey).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return nil
		}
		result, err := txs.exec(txCtx, `UPDATE tenant_entitlements
			SET used_value = used_value + ?, version = version + 1, updated_at = ?
			WHERE tenant_id = ? AND entitlement_key = ? AND period_start <= ? AND period_end > ?
			  AND used_value + reserved_value + ? <= hard_limit`,
			event.Quantity, time.Now().UTC(), scope.TenantID, event.Metric,
			event.OccurredAt.UTC(), event.OccurredAt.UTC(), event.Quantity)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrQuotaExceeded
		}
		_, err = txs.exec(txCtx, `INSERT INTO usage_events
			(tenant_id, event_key, metric, quantity, source_type, source_id,
			 correction_of, dimensions_json, occurred_at, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, scope.TenantID, event.EventKey,
			event.Metric, event.Quantity, event.SourceType, event.SourceID,
			event.CorrectionOf, event.DimensionsJSON, event.OccurredAt.UTC(), time.Now().UTC())
		if err == nil {
			inserted = true
		}
		return err
	})
	return inserted, err
}

func (s *Store) ReconcileUsageRollup(ctx context.Context, scope models.TenantScope, metric string, start, end time.Time) (int64, error) {
	if err := scope.Validate(); err != nil || metric == "" || !end.After(start) {
		return 0, fmt.Errorf("reconcile usage rollup: invalid input")
	}
	var quantity int64
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var sourceMaxID int64
		if err := txs.queryRow(txCtx, `SELECT COALESCE(SUM(quantity), 0), COALESCE(MAX(id), 0)
			FROM usage_events WHERE tenant_id = ? AND metric = ?
			  AND occurred_at >= ? AND occurred_at < ?`, scope.TenantID, metric,
			start.UTC(), end.UTC()).Scan(&quantity, &sourceMaxID); err != nil {
			return err
		}
		var correctionQuantity int64
		if err := txs.queryRow(txCtx, `SELECT COALESCE(SUM(quantity_delta), 0)
			FROM usage_corrections WHERE tenant_id = ? AND metric = ?
			  AND occurred_at >= ? AND occurred_at < ?`, scope.TenantID, metric,
			start.UTC(), end.UTC()).Scan(&correctionQuantity); err != nil {
			return err
		}
		quantity += correctionQuantity
		_, err := txs.exec(txCtx, `INSERT INTO usage_rollups
			(tenant_id, metric, period_start, period_end, quantity, source_max_id, reconciled_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, metric, period_start, period_end) DO UPDATE SET
			 quantity = excluded.quantity, source_max_id = excluded.source_max_id,
			 reconciled_at = excluded.reconciled_at`, scope.TenantID, metric,
			start.UTC(), end.UTC(), quantity, sourceMaxID, time.Now().UTC())
		return err
	})
	return quantity, err
}
