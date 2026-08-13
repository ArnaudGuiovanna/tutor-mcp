// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const sqliteSaaSCommercialMigration = `
CREATE TABLE entitlement_reservations (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    entitlement_key TEXT NOT NULL,
    quantity INTEGER NOT NULL CHECK (quantity > 0),
    status TEXT NOT NULL CHECK (status IN ('reserved','consumed','released','expired')),
    usage_event_key TEXT NOT NULL DEFAULT '',
    source_type TEXT NOT NULL DEFAULT '',
    source_id TEXT NOT NULL DEFAULT '',
    expires_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL,
    completed_at DATETIME,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, entitlement_key)
        REFERENCES tenant_entitlements(tenant_id, entitlement_key)
);
CREATE INDEX idx_entitlement_reservations_expiry
    ON entitlement_reservations(tenant_id, status, expires_at);
CREATE TABLE usage_corrections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    correction_key TEXT NOT NULL,
    original_event_key TEXT NOT NULL,
    metric TEXT NOT NULL,
    quantity_delta INTEGER NOT NULL CHECK (quantity_delta <> 0),
    reason TEXT NOT NULL,
    occurred_at DATETIME NOT NULL,
    recorded_at DATETIME NOT NULL,
    UNIQUE (tenant_id, correction_key),
    FOREIGN KEY (tenant_id, original_event_key)
        REFERENCES usage_events(tenant_id, event_key)
);
CREATE INDEX idx_usage_corrections_metric_time
    ON usage_corrections(tenant_id, metric, occurred_at);
CREATE TRIGGER usage_corrections_no_update BEFORE UPDATE ON usage_corrections
BEGIN SELECT RAISE(ABORT, 'usage corrections are append-only'); END;
CREATE TRIGGER usage_corrections_no_delete BEFORE DELETE ON usage_corrections
BEGIN SELECT RAISE(ABORT, 'usage corrections are append-only'); END;
`

const postgresSaaSCommercialMigration = `
CREATE TABLE entitlement_reservations (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    entitlement_key TEXT NOT NULL,
    quantity BIGINT NOT NULL CHECK (quantity > 0),
    status TEXT NOT NULL CHECK (status IN ('reserved','consumed','released','expired')),
    usage_event_key TEXT NOT NULL DEFAULT '',
    source_type TEXT NOT NULL DEFAULT '',
    source_id TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, entitlement_key)
        REFERENCES tenant_entitlements(tenant_id, entitlement_key)
);
CREATE INDEX idx_entitlement_reservations_expiry
    ON entitlement_reservations(tenant_id, status, expires_at);
CREATE TABLE usage_corrections (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    correction_key TEXT NOT NULL,
    original_event_key TEXT NOT NULL,
    metric TEXT NOT NULL,
    quantity_delta BIGINT NOT NULL CHECK (quantity_delta <> 0),
    reason TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, correction_key),
    FOREIGN KEY (tenant_id, original_event_key)
        REFERENCES usage_events(tenant_id, event_key)
);
CREATE INDEX idx_usage_corrections_metric_time
    ON usage_corrections(tenant_id, metric, occurred_at);
CREATE TRIGGER usage_corrections_append_only BEFORE UPDATE OR DELETE ON usage_corrections
FOR EACH ROW EXECUTE FUNCTION tutor_append_only();
ALTER TABLE entitlement_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE entitlement_reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON entitlement_reservations
    USING (tenant_id = current_setting('app.current_tenant', true))
    WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
ALTER TABLE usage_corrections ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_corrections FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON usage_corrections
    USING (tenant_id = current_setting('app.current_tenant', true))
    WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`

var commercialKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

func (s *Store) UpsertPlan(ctx context.Context, actor models.ControlPlanePrincipal, plan models.Plan) (*models.Plan, error) {
	if !actor.Validate() || !commercialKeyPattern.MatchString(plan.ID) || strings.TrimSpace(plan.Name) == "" ||
		(plan.Status != "active" && plan.Status != "retired") || !json.Valid([]byte(plan.EntitlementsJSON)) {
		return nil, fmt.Errorf("upsert plan: invalid authority or input")
	}
	var entitlements map[string]int64
	if err := json.Unmarshal([]byte(plan.EntitlementsJSON), &entitlements); err != nil || len(entitlements) == 0 {
		return nil, fmt.Errorf("upsert plan: invalid entitlements")
	}
	for key, limit := range entitlements {
		if !commercialKeyPattern.MatchString(key) || limit < 0 {
			return nil, fmt.Errorf("upsert plan: invalid entitlement")
		}
	}
	canonical, _ := json.Marshal(entitlements)
	now := time.Now().UTC()
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx, `INSERT INTO plans (id, name, status, entitlements_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET name = excluded.name,
			status = excluded.status, entitlements_json = excluded.entitlements_json,
			updated_at = excluded.updated_at`, plan.ID, plan.Name, plan.Status, string(canonical), now, now); err != nil {
			return err
		}
		return txs.appendPlatformAudit(ctx, actor, "plan.upsert", "plan", plan.ID,
			`{"status":`+strconvQuote(plan.Status)+`}`)
	})
	if err != nil {
		return nil, err
	}
	plan.EntitlementsJSON = string(canonical)
	plan.UpdatedAt = now
	if plan.CreatedAt.IsZero() {
		plan.CreatedAt = now
	}
	return &plan, nil
}

func (s *Store) AssignTenantPlan(ctx context.Context, actor models.ControlPlanePrincipal, tenantID, planID, status string, periodStart, periodEnd time.Time, graceUntil *time.Time) error {
	if !actor.Validate() || tenantID == "" || planID == "" || !periodEnd.After(periodStart) ||
		(status != "trialing" && status != "active" && status != "grace" && status != "suspended" && status != "cancelled") {
		return fmt.Errorf("assign tenant plan: invalid authority or input")
	}
	if status == "grace" && (graceUntil == nil || !graceUntil.After(time.Now().UTC())) {
		return fmt.Errorf("assign tenant plan: grace deadline is required")
	}
	return s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		var raw string
		if err := txs.queryRow(ctx, `SELECT entitlements_json FROM plans WHERE id = ? AND status = 'active'`, planID).Scan(&raw); err != nil {
			return fmt.Errorf("assign tenant plan: active plan not found")
		}
		var limits map[string]int64
		if err := json.Unmarshal([]byte(raw), &limits); err != nil {
			return err
		}
		keys := make([]string, 0, len(limits))
		for key := range limits {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			limit := limits[key]
			var used, reserved int64
			err := txs.queryRow(ctx, `SELECT used_value, reserved_value FROM tenant_entitlements
				WHERE tenant_id = ? AND entitlement_key = ?`, tenantID, key).Scan(&used, &reserved)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if used+reserved > limit {
				return fmt.Errorf("assign tenant plan: entitlement %s is already over target limit", key)
			}
			_, err = txs.exec(ctx, `INSERT INTO tenant_entitlements
				(tenant_id, entitlement_key, hard_limit, used_value, reserved_value,
				 period_start, period_end, version, updated_at)
				VALUES (?, ?, ?, 0, 0, ?, ?, 1, ?)
				ON CONFLICT (tenant_id, entitlement_key) DO UPDATE SET hard_limit = excluded.hard_limit,
				period_start = excluded.period_start, period_end = excluded.period_end,
				version = tenant_entitlements.version + 1, updated_at = excluded.updated_at`,
				tenantID, key, limit, periodStart.UTC(), periodEnd.UTC(), time.Now().UTC())
			if err != nil {
				return err
			}
		}
		_, err := txs.exec(ctx, `UPDATE tenant_subscriptions SET plan_id = ?, status = ?,
			current_period_start = ?, current_period_end = ?, grace_until = ?,
			version = version + 1, updated_at = ? WHERE tenant_id = ?`, planID, status,
			periodStart.UTC(), periodEnd.UTC(), graceUntil, time.Now().UTC(), tenantID)
		if err != nil {
			return err
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "subscription.plan.assign", "plan", planID)
	})
}

func (s *Store) ReserveEntitlement(ctx context.Context, scope models.TenantScope, reservationID, key string, quantity int64, now time.Time, ttl time.Duration) (*models.EntitlementReservation, bool, error) {
	if err := scope.Validate(); err != nil || reservationID == "" || !commercialKeyPattern.MatchString(key) ||
		quantity <= 0 || ttl < time.Second || ttl > 24*time.Hour {
		return nil, false, fmt.Errorf("reserve entitlement: invalid input")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	reservation := &models.EntitlementReservation{ID: reservationID, TenantID: scope.TenantID,
		EntitlementKey: key, Quantity: quantity, Status: "reserved", ExpiresAt: now.Add(ttl), CreatedAt: now}
	replayed := false
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var grace sql.NullTime
		var subscriptionStatus string
		if err := txs.queryRow(txCtx, `SELECT status, grace_until FROM tenant_subscriptions WHERE tenant_id = ?`, scope.TenantID).Scan(&subscriptionStatus, &grace); err != nil {
			return ErrQuotaExceeded
		}
		if subscriptionStatus != "active" && subscriptionStatus != "trialing" &&
			(subscriptionStatus != "grace" || !grace.Valid || !grace.Time.After(now)) {
			return ErrQuotaExceeded
		}
		var existing models.EntitlementReservation
		var completed sql.NullTime
		err := txs.queryRow(txCtx, `SELECT entitlement_key, quantity, status, expires_at,
			created_at, completed_at FROM entitlement_reservations WHERE tenant_id = ? AND id = ?`,
			scope.TenantID, reservationID).Scan(&existing.EntitlementKey, &existing.Quantity,
			&existing.Status, &existing.ExpiresAt, &existing.CreatedAt, &completed)
		if err == nil {
			if existing.EntitlementKey != key || existing.Quantity != quantity {
				return fmt.Errorf("reserve entitlement: idempotency conflict")
			}
			existing.ID, existing.TenantID = reservationID, scope.TenantID
			if completed.Valid {
				at := completed.Time
				existing.CompletedAt = &at
			}
			reservation, replayed = &existing, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		update, err := txs.exec(txCtx, `UPDATE tenant_entitlements SET reserved_value = reserved_value + ?,
			version = version + 1, updated_at = ? WHERE tenant_id = ? AND entitlement_key = ?
			AND period_start <= ? AND period_end > ? AND used_value + reserved_value + ? <= hard_limit`,
			quantity, now, scope.TenantID, key, now, now, quantity)
		if err != nil {
			return err
		}
		if count, _ := update.RowsAffected(); count != 1 {
			return ErrQuotaExceeded
		}
		_, err = txs.exec(txCtx, `INSERT INTO entitlement_reservations
			(id, tenant_id, entitlement_key, quantity, status, expires_at, created_at)
			VALUES (?, ?, ?, ?, 'reserved', ?, ?)`, reservationID, scope.TenantID, key,
			quantity, reservation.ExpiresAt, now)
		return err
	})
	return reservation, replayed, err
}

func (s *Store) FinishEntitlementReservation(ctx context.Context, scope models.TenantScope, reservationID, outcome, usageEventKey, sourceType, sourceID string, now time.Time) error {
	if err := scope.Validate(); err != nil || reservationID == "" || (outcome != "consumed" && outcome != "released") ||
		(outcome == "consumed" && (usageEventKey == "" || sourceType == "" || sourceID == "")) {
		return fmt.Errorf("finish entitlement reservation: invalid input")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		query := `SELECT entitlement_key, quantity, status, usage_event_key, source_type, source_id FROM entitlement_reservations
			WHERE tenant_id = ? AND id = ?`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE`
		}
		var key, status string
		var quantity int64
		var recordedEventKey, recordedSourceType, recordedSourceID string
		if err := txs.queryRow(txCtx, query, scope.TenantID, reservationID).Scan(&key, &quantity, &status,
			&recordedEventKey, &recordedSourceType, &recordedSourceID); err != nil {
			return err
		}
		if status == outcome && (outcome != "consumed" || (recordedEventKey == usageEventKey &&
			recordedSourceType == sourceType && recordedSourceID == sourceID)) {
			return nil
		}
		if status != "reserved" {
			return fmt.Errorf("finish entitlement reservation: reservation is terminal")
		}
		usedDelta := int64(0)
		if outcome == "consumed" {
			usedDelta = quantity
		}
		update, err := txs.exec(txCtx, `UPDATE tenant_entitlements SET reserved_value = reserved_value - ?,
			used_value = used_value + ?, version = version + 1, updated_at = ?
			WHERE tenant_id = ? AND entitlement_key = ? AND reserved_value >= ?`, quantity,
			usedDelta, now, scope.TenantID, key, quantity)
		if err != nil {
			return err
		}
		if count, _ := update.RowsAffected(); count != 1 {
			return ErrQuotaExceeded
		}
		if outcome == "consumed" {
			_, err = txs.exec(txCtx, `INSERT INTO usage_events
				(tenant_id, event_key, metric, quantity, source_type, source_id,
				 correction_of, dimensions_json, occurred_at, recorded_at)
				VALUES (?, ?, ?, ?, ?, ?, '', '{}', ?, ?) ON CONFLICT DO NOTHING`, scope.TenantID,
				usageEventKey, key, quantity, sourceType, sourceID, now, now)
			if err != nil {
				return err
			}
		}
		result, err := txs.exec(txCtx, `UPDATE entitlement_reservations SET status = ?,
			usage_event_key = ?, source_type = ?, source_id = ?, completed_at = ?
			WHERE tenant_id = ? AND id = ? AND status = 'reserved'`, outcome, usageEventKey,
			sourceType, sourceID, now, scope.TenantID, reservationID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("finish entitlement reservation: concurrent transition")
		}
		return nil
	})
}

func (s *Store) ExpireEntitlementReservations(ctx context.Context, scope models.TenantScope, now time.Time, limit int) (int, error) {
	if err := scope.Validate(); err != nil || limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("expire entitlement reservations: invalid input")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	expired := 0
	for expired < limit {
		var id string
		err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
			txs := scoped.(*Store)
			query := `SELECT id FROM entitlement_reservations WHERE tenant_id = ? AND status = 'reserved'
				AND expires_at <= ? ORDER BY expires_at, id LIMIT 1`
			if txs.dialect == DialectPostgres {
				query += ` FOR UPDATE SKIP LOCKED`
			}
			if err := txs.queryRow(txCtx, query, scope.TenantID, now).Scan(&id); err != nil {
				return err
			}
			var key string
			var quantity int64
			if err := txs.queryRow(txCtx, `SELECT entitlement_key, quantity FROM entitlement_reservations
				WHERE tenant_id = ? AND id = ? AND status = 'reserved'`, scope.TenantID, id).Scan(&key, &quantity); err != nil {
				return err
			}
			if _, err := txs.exec(txCtx, `UPDATE tenant_entitlements SET reserved_value = reserved_value - ?,
				version = version + 1, updated_at = ? WHERE tenant_id = ? AND entitlement_key = ? AND reserved_value >= ?`,
				quantity, now, scope.TenantID, key, quantity); err != nil {
				return err
			}
			_, err := txs.exec(txCtx, `UPDATE entitlement_reservations SET status = 'expired', completed_at = ?
				WHERE tenant_id = ? AND id = ? AND status = 'reserved'`, now, scope.TenantID, id)
			return err
		})
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}

func (s *Store) CorrectUsage(ctx context.Context, actor models.Principal, correction models.UsageCorrection) (bool, error) {
	if !actor.Authorize(models.PermissionBillingManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		correction.CorrectionKey == "" || correction.OriginalEventKey == "" || correction.QuantityDelta == 0 ||
		strings.TrimSpace(correction.Reason) == "" {
		return false, fmt.Errorf("correct usage: invalid authorization or input")
	}
	if correction.OccurredAt.IsZero() {
		correction.OccurredAt = time.Now().UTC()
	}
	inserted := false
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var metric string
		var originalQuantity int64
		if err := txs.queryRow(txCtx, `SELECT metric, quantity FROM usage_events
			WHERE tenant_id = ? AND event_key = ?`, actor.TenantID, correction.OriginalEventKey).Scan(&metric, &originalQuantity); err != nil {
			return err
		}
		var existingMetric string
		var existingDelta int64
		err := txs.queryRow(txCtx, `SELECT metric, quantity_delta FROM usage_corrections
			WHERE tenant_id = ? AND correction_key = ?`, actor.TenantID, correction.CorrectionKey).Scan(&existingMetric, &existingDelta)
		if err == nil {
			if existingMetric != metric || existingDelta != correction.QuantityDelta {
				return fmt.Errorf("correct usage: idempotency conflict")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var priorDelta int64
		if err := txs.queryRow(txCtx, `SELECT COALESCE(SUM(quantity_delta),0) FROM usage_corrections
			WHERE tenant_id = ? AND original_event_key = ?`, actor.TenantID, correction.OriginalEventKey).Scan(&priorDelta); err != nil {
			return err
		}
		if originalQuantity+priorDelta+correction.QuantityDelta < 0 {
			return fmt.Errorf("correct usage: correction exceeds original usage")
		}
		result, err := txs.exec(txCtx, `UPDATE tenant_entitlements SET used_value = used_value + ?,
			version = version + 1, updated_at = ? WHERE tenant_id = ? AND entitlement_key = ?
			AND used_value + ? >= 0 AND used_value + reserved_value + ? <= hard_limit`, correction.QuantityDelta,
			time.Now().UTC(), actor.TenantID, metric, correction.QuantityDelta, correction.QuantityDelta)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrQuotaExceeded
		}
		_, err = txs.exec(txCtx, `INSERT INTO usage_corrections
			(tenant_id, correction_key, original_event_key, metric, quantity_delta, reason, occurred_at, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, actor.TenantID, correction.CorrectionKey,
			correction.OriginalEventKey, metric, correction.QuantityDelta, correction.Reason,
			correction.OccurredAt.UTC(), time.Now().UTC())
		if err != nil {
			return err
		}
		inserted = true
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "usage.correct", TargetType: "usage_event", TargetID: correction.OriginalEventKey})
	})
	return inserted, err
}
