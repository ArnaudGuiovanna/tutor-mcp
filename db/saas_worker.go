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

// RelayOutboxEvent materializes one immutable outbox event into one durable
// job per subscribed integration. Exact job idempotency makes lease recovery
// safe after a crash at any statement boundary.
func (s *Store) RelayOutboxEvent(ctx context.Context, scope models.TenantScope, workerID string, now time.Time, lease time.Duration) (bool, error) {
	event, err := s.ClaimOutboxEvent(ctx, scope, workerID, now, lease)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var integrationIDs []string
	err = s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		query := `SELECT id FROM tenant_integrations WHERE tenant_id = ? AND status = 'active'
			AND EXISTS (SELECT 1 FROM json_each(event_types_json) WHERE value = ?)
			ORDER BY id`
		args := []any{scope.TenantID, event.EventType}
		if txs.dialect == DialectPostgres {
			query = `SELECT id FROM tenant_integrations WHERE tenant_id = ? AND status = 'active'
				AND event_types_json @> CAST(? AS jsonb) ORDER BY id`
			eventJSON, _ := json.Marshal([]string{event.EventType})
			args = []any{scope.TenantID, string(eventJSON)}
		}
		rows, err := txs.query(txCtx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			integrationIDs = append(integrationIDs, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, integrationID := range integrationIDs {
			jobPayload, err := json.Marshal(models.TenantWebhookJob{
				IntegrationID: integrationID, EventID: event.ID, EventType: event.EventType,
				Payload: json.RawMessage(event.PayloadJSON),
			})
			if err != nil {
				return err
			}
			if err := s.EnqueueAsyncJob(txCtx, scope, models.AsyncJob{
				TenantID: scope.TenantID, Kind: "tenant_webhook_delivery",
				IdempotencyKey: integrationID + ":" + event.ID,
				PayloadJSON:    string(jobPayload), MaxAttempts: 8, CreatedAt: now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = s.FinishOutboxEvent(ctx, scope, event.ID, workerID, false, now.Add(time.Minute), "relay_failed")
		return true, err
	}
	if err := s.FinishOutboxEvent(ctx, scope, event.ID, workerID, true, time.Time{}, ""); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Store) BeginIntegrationDelivery(ctx context.Context, scope models.TenantScope, job models.TenantWebhookJob, attempt int, now time.Time) (*models.IntegrationDelivery, error) {
	if err := scope.Validate(); err != nil || job.IntegrationID == "" || job.EventID == "" ||
		job.EventType == "" || !json.Valid(job.Payload) || attempt < 1 {
		return nil, fmt.Errorf("begin integration delivery: invalid input")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	id := stableLearningID("integration_delivery_", scope.TenantID, job.IntegrationID, job.EventID, fmt.Sprint(attempt))
	delivery := &models.IntegrationDelivery{ID: id, TenantID: scope.TenantID,
		IntegrationID: job.IntegrationID, EventID: job.EventID, Attempt: attempt,
		Status: "pending", CreatedAt: now.UTC()}
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var priorResponseCode sql.NullInt64
		var priorDeliveredAt sql.NullTime
		prior := &models.IntegrationDelivery{TenantID: scope.TenantID, IntegrationID: job.IntegrationID}
		err := txs.queryRow(txCtx, `SELECT id, event_id, attempt, status, response_code,
			response_hash, last_error, created_at, delivered_at FROM integration_deliveries
			WHERE tenant_id = ? AND integration_id = ? AND event_id = ? AND status = 'delivered'
			ORDER BY attempt DESC LIMIT 1`, scope.TenantID, job.IntegrationID, job.EventID).Scan(
			&prior.ID, &prior.EventID, &prior.Attempt, &prior.Status, &priorResponseCode,
			&prior.ResponseHash, &prior.LastError, &prior.CreatedAt, &priorDeliveredAt)
		if err == nil {
			if priorResponseCode.Valid {
				code := int(priorResponseCode.Int64)
				prior.ResponseCode = &code
			}
			if priorDeliveredAt.Valid {
				delivered := priorDeliveredAt.Time
				prior.DeliveredAt = &delivered
			}
			delivery = prior
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := txs.exec(txCtx, `INSERT INTO integration_deliveries
			(id, tenant_id, integration_id, event_id, attempt, status, created_at)
			SELECT ?, ?, id, ?, ?, 'pending', ? FROM tenant_integrations
			WHERE tenant_id = ? AND id = ?
			ON CONFLICT (tenant_id, integration_id, event_id, attempt) DO NOTHING`, id,
			scope.TenantID, job.EventID, attempt, now.UTC(), scope.TenantID, job.IntegrationID); err != nil {
			return err
		}
		var responseCode sql.NullInt64
		var deliveredAt sql.NullTime
		if err := txs.queryRow(txCtx, `SELECT id, status, response_code, response_hash,
			last_error, created_at, delivered_at FROM integration_deliveries
			WHERE tenant_id = ? AND integration_id = ? AND event_id = ? AND attempt = ?`,
			scope.TenantID, job.IntegrationID, job.EventID, attempt).Scan(&delivery.ID,
			&delivery.Status, &responseCode, &delivery.ResponseHash, &delivery.LastError,
			&delivery.CreatedAt, &deliveredAt); err != nil {
			return err
		}
		if responseCode.Valid {
			code := int(responseCode.Int64)
			delivery.ResponseCode = &code
		}
		if deliveredAt.Valid {
			delivered := deliveredAt.Time
			delivery.DeliveredAt = &delivered
		}
		return nil
	})
	return delivery, err
}

func (s *Store) FinishIntegrationDelivery(ctx context.Context, scope models.TenantScope, deliveryID, status string, responseCode *int, responseHash, errorCode string) error {
	if err := scope.Validate(); err != nil || deliveryID == "" ||
		(status != "delivered" && status != "failed" && status != "dead_letter") || len(errorCode) > 128 {
		return fmt.Errorf("finish integration delivery: invalid input")
	}
	var deliveredAt any
	if status == "delivered" {
		deliveredAt = time.Now().UTC()
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		result, err := scoped.(*Store).exec(txCtx, `UPDATE integration_deliveries
			SET status = ?, response_code = ?, response_hash = ?, last_error = ?, delivered_at = ?
			WHERE tenant_id = ? AND id = ? AND status = 'pending'`, status, responseCode,
			responseHash, errorCode, deliveredAt, scope.TenantID, deliveryID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("finish integration delivery: pending delivery not found")
		}
		return nil
	})
}

func (s *Store) ListIntegrationDeliveries(ctx context.Context, actor models.Principal, integrationID, after string, limit int) ([]models.IntegrationDelivery, string, error) {
	if !actor.Authorize(models.PermissionIntegrationManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		integrationID == "" || limit < 1 || limit > 100 {
		return nil, "", fmt.Errorf("list integration deliveries: invalid authorization or page")
	}
	var deliveries []models.IntegrationDelivery
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		rows, err := scoped.(*Store).query(txCtx, `SELECT id, event_id, attempt, status,
			response_code, response_hash, last_error, created_at, delivered_at
			FROM integration_deliveries WHERE tenant_id = ? AND integration_id = ? AND id > ?
			ORDER BY id LIMIT ?`, actor.TenantID, integrationID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item := models.IntegrationDelivery{TenantID: actor.TenantID, IntegrationID: integrationID}
			var responseCode sql.NullInt64
			var deliveredAt sql.NullTime
			if err := rows.Scan(&item.ID, &item.EventID, &item.Attempt, &item.Status,
				&responseCode, &item.ResponseHash, &item.LastError, &item.CreatedAt, &deliveredAt); err != nil {
				return err
			}
			if responseCode.Valid {
				code := int(responseCode.Int64)
				item.ResponseCode = &code
			}
			if deliveredAt.Valid {
				delivered := deliveredAt.Time
				item.DeliveredAt = &delivered
			}
			deliveries = append(deliveries, item)
		}
		return rows.Err()
	})
	next := ""
	if len(deliveries) > limit {
		deliveries = deliveries[:limit]
		next = deliveries[len(deliveries)-1].ID
	}
	return deliveries, next, err
}
