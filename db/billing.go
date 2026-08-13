// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
)

type billingProviderEvent struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Type        string    `json:"type"`
	Status      string    `json:"status"`
	PlanID      string    `json:"plan_id"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// ApplyBillingProviderWebhook authenticates before parsing tenant routing,
// deduplicates provider event IDs, and applies subscription state in the same
// transaction as the immutable source event.
func (s *Store) ApplyBillingProviderWebhook(ctx context.Context, credential models.BillingWebhookCredential, signature string, payload []byte) (bool, error) {
	if credential.Provider == "" || len(credential.Secret) < 32 || !strings.HasPrefix(signature, "sha256=") {
		return false, fmt.Errorf("billing webhook: invalid credential or signature")
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false, fmt.Errorf("billing webhook: invalid signature")
	}
	mac := hmac.New(sha256.New, []byte(credential.Secret))
	_, _ = mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), provided) {
		return false, fmt.Errorf("billing webhook: invalid signature")
	}
	var event billingProviderEvent
	if err := json.Unmarshal(payload, &event); err != nil || event.ID == "" || event.TenantID == "" ||
		event.Type == "" || event.OccurredAt.IsZero() {
		return false, fmt.Errorf("billing webhook: invalid event")
	}
	payloadSum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(payloadSum[:])
	processed := false
	err = s.withTenantControlTx(ctx, event.TenantID, "billing:"+credential.Provider, func(txs *Store) error {
		var existingHash string
		err := txs.queryRow(ctx, `SELECT payload_hash FROM billing_provider_events
			WHERE provider = ? AND event_id = ?`, credential.Provider, event.ID).Scan(&existingHash)
		if err == nil {
			if existingHash != payloadHash {
				return fmt.Errorf("billing webhook: event id payload conflict")
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := txs.exec(ctx, `INSERT INTO billing_provider_events
			(provider, event_id, tenant_id, event_type, payload_hash, occurred_at, processed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, credential.Provider, event.ID, event.TenantID,
			event.Type, payloadHash, event.OccurredAt.UTC(), time.Now().UTC()); err != nil {
			return err
		}
		now := time.Now().UTC()
		switch event.Type {
		case "subscription.active":
			if event.PlanID == "" || !event.PeriodEnd.After(event.PeriodStart) {
				return fmt.Errorf("billing webhook: active subscription lacks plan or period")
			}
			if _, err := txs.exec(ctx, `UPDATE tenant_subscriptions
				SET plan_id = ?, status = 'active', provider = ?,
				    current_period_start = ?, current_period_end = ?, grace_until = NULL,
				    version = version + 1, updated_at = ? WHERE tenant_id = ?`,
				event.PlanID, credential.Provider, event.PeriodStart.UTC(), event.PeriodEnd.UTC(), now, event.TenantID); err != nil {
				return err
			}
		case "subscription.past_due":
			// Provider outages or payment retries enter a bounded grace period;
			// an in-progress learning session is never cut immediately.
			if _, err := txs.exec(ctx, `UPDATE tenant_subscriptions
				SET status = 'grace', grace_until = ?, version = version + 1, updated_at = ?
				WHERE tenant_id = ? AND status <> 'cancelled'`, now.Add(7*24*time.Hour), now, event.TenantID); err != nil {
				return err
			}
		case "subscription.cancelled":
			if _, err := txs.exec(ctx, `UPDATE tenant_subscriptions
				SET status = 'cancelled', grace_until = NULL, version = version + 1, updated_at = ?
				WHERE tenant_id = ?`, now, event.TenantID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("billing webhook: unsupported event type")
		}
		actor := models.ControlPlanePrincipal{
			ActorID: "billing:" + credential.Provider, Roles: []string{models.RolePlatformAdmin},
			Reason: "signed billing provider event", RequestID: event.ID,
		}
		if err := txs.appendControlPlaneAudit(ctx, event.TenantID, actor,
			"billing."+event.Type, "subscription", event.TenantID); err != nil {
			return err
		}
		processed = true
		return nil
	})
	return processed, err
}
