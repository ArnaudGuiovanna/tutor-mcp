package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var _ storeport.AsyncJobAttemptStore = (*Store)(nil)

// FinalizeAsyncJobAttempt owns both durable transitions. A stale attempt cannot
// alter a delivery, even if it still knows its ID or shares the process owner.
func (s *Store) FinalizeAsyncJobAttempt(ctx context.Context, scope models.TenantScope, jobID, owner string, attempt int, result models.AsyncJobAttemptResult, delivery *models.AsyncJobDeliveryResult, now time.Time) (models.AsyncJobOutcome, error) {
	if s.root == nil || ctx.Value(tenantTransactionContextKey{}) != nil {
		return "", fmt.Errorf("finalize async job: independent transaction required")
	}
	if scope.Validate() != nil || jobID == "" || owner == "" || attempt < 1 || now.IsZero() || len(result.ErrorCode) > 128 {
		return "", fmt.Errorf("finalize async job: invalid input")
	}
	for _, c := range result.ErrorCode {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return "", fmt.Errorf("finalize async job: invalid error code")
		}
	}
	switch result.Outcome {
	case models.AsyncJobCompleted, models.AsyncJobRetry, models.AsyncJobDeadLetter, models.AsyncJobContinue:
	default:
		return "", fmt.Errorf("finalize async job: invalid outcome")
	}
	if delivery != nil {
		if delivery.DeliveryID == "" || (delivery.ResponseCode != nil && (*delivery.ResponseCode < 100 || *delivery.ResponseCode > 599)) {
			return "", fmt.Errorf("finalize async job: invalid delivery result")
		}
		if delivery.ResponseHash != "" {
			if decoded, err := hex.DecodeString(delivery.ResponseHash); err != nil || len(decoded) != 32 {
				return "", fmt.Errorf("finalize async job: invalid response digest")
			}
		}
	}
	outcome := result.Outcome
	err := s.WithTenantTx(ctx, scope, func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		query := `SELECT kind,payload_json,max_attempts FROM async_jobs WHERE tenant_id=? AND id=?
		 AND status='processing' AND lease_owner=? AND attempt_count=? AND lease_expires_at>?`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE`
		}
		var kind, payloadJSON string
		var maxAttempts int
		if err := txs.queryRow(ctx, query, scope.TenantID, jobID, owner, attempt, now.UTC()).Scan(&kind, &payloadJSON, &maxAttempts); err != nil {
			if err == sql.ErrNoRows {
				return ErrLeaseLost
			}
			return err
		}
		if outcome == models.AsyncJobRetry && attempt >= maxAttempts {
			outcome = models.AsyncJobDeadLetter
		}
		if outcome == models.AsyncJobContinue && (kind != "tenant_dsar" || delivery != nil) {
			return fmt.Errorf("finalize async job: invalid continuation")
		}
		priorDelivered := false
		if delivery != nil {
			var payload models.TenantWebhookJob
			if kind != "tenant_webhook_delivery" || json.Unmarshal([]byte(payloadJSON), &payload) != nil || payload.IntegrationID == "" || payload.EventID == "" || payload.EventType == "" || !json.Valid(payload.Payload) {
				return fmt.Errorf("finalize async job: invalid delivery binding")
			}
			var integrationID, eventID, status string
			var deliveryAttempt int
			if err := txs.queryRow(ctx, `SELECT integration_id,event_id,attempt,status FROM integration_deliveries WHERE tenant_id=? AND id=?`, scope.TenantID, delivery.DeliveryID).Scan(&integrationID, &eventID, &deliveryAttempt, &status); err != nil {
				return err
			}
			if integrationID != payload.IntegrationID || eventID != payload.EventID {
				return fmt.Errorf("finalize async job: delivery binding mismatch")
			}
			priorDelivered = status == "delivered"
			if priorDelivered {
				if outcome != models.AsyncJobCompleted || deliveryAttempt > attempt {
					return fmt.Errorf("finalize async job: invalid accepted delivery")
				}
			} else if status != "pending" || deliveryAttempt != attempt {
				return fmt.Errorf("finalize async job: delivery attempt mismatch")
			}
		} else if kind == "tenant_webhook_delivery" && outcome == models.AsyncJobCompleted {
			return fmt.Errorf("finalize async job: accepted delivery required")
		}
		status := "pending"
		var completed any
		if outcome == models.AsyncJobCompleted || outcome == models.AsyncJobDeadLetter {
			status = string(outcome)
			completed = now.UTC()
		}
		available := result.AvailableAt
		if available.IsZero() {
			available = now
		}
		refund := 0
		if outcome == models.AsyncJobContinue {
			refund = 1
		}
		updated, err := txs.exec(ctx, `UPDATE async_jobs SET status=?,available_at=?,last_error=?,completed_at=?,
		 attempt_count=attempt_count-?,lease_owner='',lease_expires_at=NULL,heartbeat_at=NULL
		 WHERE tenant_id=? AND id=? AND status='processing' AND lease_owner=? AND attempt_count=? AND lease_expires_at>?`,
			status, available.UTC(), result.ErrorCode, completed, refund, scope.TenantID, jobID, owner, attempt, now.UTC())
		if err != nil {
			return err
		}
		if rows, err := updated.RowsAffected(); err != nil {
			return err
		} else if rows != 1 {
			return ErrLeaseLost
		}
		if delivery == nil || priorDelivered {
			return nil
		}
		deliveryStatus := "failed"
		var deliveredAt any
		if outcome == models.AsyncJobCompleted {
			deliveryStatus = "delivered"
			deliveredAt = now.UTC()
		} else if outcome == models.AsyncJobDeadLetter {
			deliveryStatus = "dead_letter"
		}
		updated, err = txs.exec(ctx, `UPDATE integration_deliveries SET status=?,response_code=?,response_hash=?,last_error=?,delivered_at=?
		 WHERE tenant_id=? AND id=? AND status='pending' AND attempt=?`, deliveryStatus, delivery.ResponseCode, delivery.ResponseHash, result.ErrorCode, deliveredAt, scope.TenantID, delivery.DeliveryID, attempt)
		if err != nil {
			return err
		}
		if rows, err := updated.RowsAffected(); err != nil {
			return err
		} else if rows != 1 {
			return fmt.Errorf("finalize async job: pending delivery changed")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}
