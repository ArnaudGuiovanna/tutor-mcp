// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestEntitlementReservationUsageCorrectionAndGrace(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	scope := owner.TenantScope()
	now := time.Now().UTC()
	if err := s.ConfigureEntitlement(ctx, owner, "exports_month", 3, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	reserved, replayed, err := s.ReserveEntitlement(ctx, scope, "reservation-1", "exports_month", 2, now, time.Hour)
	if err != nil || replayed || reserved.Quantity != 2 {
		t.Fatalf("reserve=%#v replayed=%v err=%v", reserved, replayed, err)
	}
	if _, replayed, err := s.ReserveEntitlement(ctx, scope, "reservation-1", "exports_month", 2, now, time.Hour); err != nil || !replayed {
		t.Fatalf("exact reservation replay replayed=%v err=%v", replayed, err)
	}
	if _, _, err := s.ReserveEntitlement(ctx, scope, "reservation-1", "exports_month", 1, now, time.Hour); err == nil {
		t.Fatal("conflicting reservation replay accepted")
	}
	if _, _, err := s.ReserveEntitlement(ctx, scope, "reservation-over", "exports_month", 2, now, time.Hour); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over reservation=%v, want quota exceeded", err)
	}
	if err := s.FinishEntitlementReservation(ctx, scope, "reservation-1", "consumed",
		"usage-export-1", "export", "export-1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishEntitlementReservation(ctx, scope, "reservation-1", "consumed",
		"usage-export-1", "export", "export-1", now); err != nil {
		t.Fatalf("exact consume replay: %v", err)
	}
	if err := s.FinishEntitlementReservation(ctx, scope, "reservation-1", "consumed",
		"different-event", "export", "export-1", now); err == nil {
		t.Fatal("conflicting consume replay accepted")
	}
	inserted, err := s.CorrectUsage(ctx, owner, models.UsageCorrection{
		CorrectionKey: "correction-export-1", OriginalEventKey: "usage-export-1",
		QuantityDelta: -1, Reason: "one exported item was invalid", OccurredAt: now,
	})
	if err != nil || !inserted {
		t.Fatalf("correction inserted=%v err=%v", inserted, err)
	}
	if inserted, err := s.CorrectUsage(ctx, owner, models.UsageCorrection{
		CorrectionKey: "correction-export-1", OriginalEventKey: "usage-export-1",
		QuantityDelta: -1, Reason: "replay", OccurredAt: now,
	}); err != nil || inserted {
		t.Fatalf("correction replay inserted=%v err=%v", inserted, err)
	}
	if _, err := s.CorrectUsage(ctx, owner, models.UsageCorrection{
		CorrectionKey: "correction-export-over", OriginalEventKey: "usage-export-1",
		QuantityDelta: -2, Reason: "invalid overcorrection", OccurredAt: now,
	}); err == nil {
		t.Fatal("correction below zero accepted")
	}
	quantity, err := s.ReconcileUsageRollup(ctx, scope, "exports_month", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || quantity != 1 {
		t.Fatalf("corrected rollup=%d err=%v", quantity, err)
	}
	var used, held int64
	if err := s.queryRow(ctx, `SELECT used_value, reserved_value FROM tenant_entitlements
		WHERE tenant_id = ? AND entitlement_key = 'exports_month'`, owner.TenantID).Scan(&used, &held); err != nil {
		t.Fatal(err)
	}
	if used != 1 || held != 0 {
		t.Fatalf("entitlement used/reserved=%d/%d", used, held)
	}
	if _, err := s.exec(ctx, `UPDATE usage_corrections SET reason = 'tampered'
		WHERE tenant_id = ?`, owner.TenantID); err == nil {
		t.Fatal("usage correction was mutable")
	}

	if _, _, err := s.ReserveEntitlement(ctx, scope, "reservation-expire", "exports_month", 1, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if count, err := s.ExpireEntitlementReservations(ctx, scope, now.Add(2*time.Minute), 10); err != nil || count != 1 {
		t.Fatalf("expired count=%d err=%v", count, err)
	}
	if _, err := s.exec(ctx, `UPDATE tenant_subscriptions SET status = 'grace', grace_until = ?
		WHERE tenant_id = ?`, now.Add(-time.Minute), owner.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReserveEntitlement(ctx, scope, "reservation-after-grace", "exports_month", 1, now, time.Minute); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expired grace reservation=%v", err)
	}
}

func TestControlPlanePlanAssignmentRejectsUnsafeDowngrade(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	now := time.Now().UTC()
	operator := models.ControlPlanePrincipal{ActorID: "commercial-operator", Roles: []string{models.RolePlatformAdmin},
		Reason: "plan acceptance test", RequestID: "plan-request-1"}
	if _, err := s.UpsertPlan(ctx, operator, models.Plan{ID: "plan_test_high", Name: "Test high", Status: "active",
		EntitlementsJSON: `{"exports_month":5,"mcp_calls_month":100}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignTenantPlan(ctx, operator, owner.TenantID, "plan_test_high", "active", now, now.AddDate(0, 1, 0), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReserveEntitlement(ctx, owner.TenantScope(), "plan-reservation", "exports_month", 4, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertPlan(ctx, operator, models.Plan{ID: "plan_test_low", Name: "Test low", Status: "active",
		EntitlementsJSON: `{"exports_month":3,"mcp_calls_month":100}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignTenantPlan(ctx, operator, owner.TenantID, "plan_test_low", "active", now, now.AddDate(0, 1, 0), nil); err == nil {
		t.Fatal("plan downgrade below reserved usage accepted")
	}
	var planID string
	if err := s.queryRow(ctx, `SELECT plan_id FROM tenant_subscriptions WHERE tenant_id = ?`, owner.TenantID).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if planID != "plan_test_high" {
		t.Fatalf("failed downgrade changed plan to %q", planID)
	}
	var planAudits int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM platform_audit_events
		WHERE actor_id = ? AND action = 'plan.upsert'`, operator.ActorID).Scan(&planAudits); err != nil {
		t.Fatal(err)
	}
	if planAudits != 2 {
		t.Fatalf("platform plan audit events=%d", planAudits)
	}
	if _, err := s.exec(ctx, `UPDATE platform_audit_events SET reason = 'tampered'
		WHERE actor_id = ?`, operator.ActorID); err == nil {
		t.Fatal("platform audit event was mutable")
	}
}
