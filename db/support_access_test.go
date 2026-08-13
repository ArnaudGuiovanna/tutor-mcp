// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestSupportAccessIsShortLivedReadOnlyRevocableAndAudited(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	actor := models.ControlPlanePrincipal{
		ActorID: "support-operator", Roles: []string{models.RolePlatformAdmin},
		Reason: "customer-approved incident investigation", RequestID: "support-request-1",
	}
	grant, raw, err := s.BeginSupportAccess(ctx, actor, models.LegacyTenantID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || !strings.HasPrefix(raw, "support_") || grant.ExpiresAt.Sub(grant.CreatedAt) > time.Hour {
		t.Fatalf("grant=%#v raw prefix=%v", grant, strings.HasPrefix(raw, "support_"))
	}
	var storedHash, reason, requestID string
	if err := s.queryRow(ctx, `SELECT token_hash, reason, request_id FROM support_access_grants
		WHERE tenant_id = ? AND id = ?`, grant.TenantID, grant.ID).
		Scan(&storedHash, &reason, &requestID); err != nil {
		t.Fatal(err)
	}
	if storedHash == raw || reason != actor.Reason || requestID != actor.RequestID {
		t.Fatalf("stored support grant hash=%q reason=%q request=%q", storedHash, reason, requestID)
	}
	principal, err := s.AuthenticateSupportAccess(ctx, models.SupportAccessCredential{Token: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !principal.Authorize(models.PermissionAuditRead, models.AuthorizationResource{TenantID: grant.TenantID}) ||
		principal.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: grant.TenantID}) ||
		principal.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: grant.TenantID}) {
		t.Fatalf("support permissions are not read-only: %#v", principal)
	}
	if err := s.ValidatePrincipal(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSupportAccess(ctx, actor, grant.TenantID, grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateSupportAccess(ctx, models.SupportAccessCredential{Token: raw}); err == nil {
		t.Fatal("revoked support credential accepted")
	}
	if err := s.ValidatePrincipal(ctx, principal); err == nil {
		t.Fatal("revoked support principal remained valid")
	}
	var audited int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = ?
		AND target_id = ? AND action IN ('support_access.begin','support_access.revoke')`,
		grant.TenantID, grant.ID).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 2 {
		t.Fatalf("support audit events = %d", audited)
	}
}

func TestSupportAccessRequiresReasonRequestAndBoundedTTL(t *testing.T) {
	s := setupTestDB(t)
	actor := models.ControlPlanePrincipal{ActorID: "support", Roles: []string{models.RolePlatformAdmin}, Reason: "incident"}
	if _, _, err := s.BeginSupportAccess(context.Background(), actor, models.LegacyTenantID, time.Minute); err == nil {
		t.Fatal("support access without request ID accepted")
	}
	actor.RequestID = "request"
	if _, _, err := s.BeginSupportAccess(context.Background(), actor, models.LegacyTenantID, time.Hour+time.Second); err == nil {
		t.Fatal("support access over one hour accepted")
	}
}
