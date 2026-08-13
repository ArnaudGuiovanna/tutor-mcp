// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"

	"tutor-mcp/models"
)

func TestControlPlaneProvisionStatusFlagsAndDomainRouting(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	actor := models.ControlPlanePrincipal{
		ActorID: "platform-operator", Roles: []string{models.RolePlatformAdmin},
		Reason: "tenant acceptance test", RequestID: "request-control-1",
	}
	tenant, err := s.ProvisionTenant(ctx, actor, "acme-training", "Acme Training", "eu-west", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.ProvisionTenant(ctx, actor, "acme-training", "Acme Training", "eu-west", "plan_legacy")
	if err != nil || replayed.ID != tenant.ID {
		t.Fatalf("idempotent provision = %#v err=%v", replayed, err)
	}
	if _, err := s.ProvisionTenant(ctx, actor, "acme-training", "Different", "eu-west", "plan_legacy"); err == nil {
		t.Fatal("conflicting tenant provision accepted")
	}
	var entitlements int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM tenant_entitlements WHERE tenant_id = ?`, tenant.ID).Scan(&entitlements); err != nil {
		t.Fatal(err)
	}
	if entitlements < 5 {
		t.Fatalf("provisioned entitlements = %d", entitlements)
	}
	if err := s.SetTenantFeatureFlag(ctx, actor, tenant.ID, "catalog_v2", true); err != nil {
		t.Fatal(err)
	}
	raw, err := s.BeginTenantDomainVerification(ctx, actor, tenant.ID, "learn.acme.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTenantByHostname(ctx, s, "learn.acme.test"); err == nil {
		t.Fatal("pending custom domain routed")
	}
	if err := s.CompleteTenantDomainVerification(ctx, actor, "learn.acme.test", "wrong"); err == nil {
		t.Fatal("wrong domain verification token accepted")
	}
	if err := s.CompleteTenantDomainVerification(ctx, actor, "learn.acme.test", raw); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveTenantByHostname(ctx, s, "LEARN.ACME.TEST."); err != nil || got != tenant.ID {
		t.Fatalf("hostname route = %q err=%v", got, err)
	}
	if err := s.SetTenantStatus(ctx, actor, tenant.ID, models.TenantStatusSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTenantByHostname(ctx, s, "learn.acme.test"); err == nil {
		t.Fatal("suspended tenant remained routable")
	}
	if err := s.SetTenantStatus(ctx, actor, tenant.ID, models.TenantStatusActive); err != nil {
		t.Fatal(err)
	}
	var auditCount int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM audit_events
		WHERE tenant_id = ? AND membership_id = 'control_plane'`, tenant.ID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount < 4 {
		t.Fatalf("control-plane audit count = %d", auditCount)
	}
}
