// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestServiceAccountCredentialLifecycleAndTenantIsolation(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	expiresAt := time.Now().UTC().Add(time.Hour)

	account, rawToken, err := s.CreateServiceAccount(ctx, owner, "reporting bot",
		[]string{models.RoleAuditor}, models.OAuthScopeLearnerRead, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if rawToken == "" || !strings.HasPrefix(rawToken, "tsa_") {
		t.Fatalf("raw token = %q", rawToken)
	}
	if account.TenantID != owner.TenantID || account.Status != "active" ||
		len(account.Roles) != 2 || account.Roles[0] != models.RoleServiceAccount ||
		account.Roles[1] != models.RoleAuditor {
		t.Fatalf("service account = %#v", account)
	}

	var storedHash, storedScopes string
	if err := s.queryRow(ctx, `SELECT token_hash, scopes_json FROM service_accounts
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, account.ID).
		Scan(&storedHash, &storedScopes); err != nil {
		t.Fatal(err)
	}
	if storedHash == rawToken || storedHash == "" || strings.Contains(storedScopes, rawToken) {
		t.Fatal("raw service-account credential was persisted")
	}

	credential := models.ServiceAccountCredential{ClientID: account.ClientID, Secret: rawToken}
	principal, err := s.AuthenticateServiceAccount(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if principal.TenantID != owner.TenantID || principal.UserID != account.ID ||
		principal.MembershipID != "service_account_"+account.ID ||
		!principal.Authorize(models.PermissionAuditRead,
			models.AuthorizationResource{TenantID: owner.TenantID}) {
		t.Fatalf("service principal = %#v", principal)
	}
	if err := s.ValidatePrincipal(ctx, principal); err != nil {
		t.Fatalf("validate current service principal: %v", err)
	}
	if _, err := s.AuthenticateServiceAccount(ctx, models.ServiceAccountCredential{
		ClientID: "another-client", Secret: rawToken,
	}); err == nil {
		t.Fatal("credential accepted for another OAuth client")
	}
	if _, err := s.AuthenticateServiceAccount(ctx, models.ServiceAccountCredential{
		ClientID: account.ClientID, Secret: rawToken + "x",
	}); err == nil {
		t.Fatal("wrong service-account credential accepted")
	}

	other := owner
	other.TenantID = "tenant-other"
	other.MembershipID = "membership-other"
	other.LearnerID = ""
	if err := s.RevokeServiceAccount(ctx, other, account.ID); err == nil {
		t.Fatal("cross-tenant service-account revoke accepted")
	}
	if err := s.ValidatePrincipal(ctx, principal); err != nil {
		t.Fatalf("cross-tenant attempt changed account: %v", err)
	}

	if err := s.RevokeServiceAccount(ctx, owner, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateServiceAccount(ctx, credential); err == nil {
		t.Fatal("revoked service-account credential accepted")
	}
	if err := s.ValidatePrincipal(ctx, principal); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("revoked principal validation error = %v", err)
	}
}

func TestServiceAccountCreationFailsClosed(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	expiresAt := time.Now().UTC().Add(time.Hour)

	for _, roles := range [][]string{
		{models.RoleOwner},
		{models.RoleAdmin},
		{models.RoleBillingAdmin},
		{models.RoleLearner},
		{models.RoleServiceAccount},
		{models.RoleAuditor, models.RoleAuditor},
	} {
		if _, _, err := s.CreateServiceAccount(ctx, owner, "unsafe", roles,
			models.OAuthScopeLearnerRead, expiresAt); err == nil {
			t.Fatalf("unsafe roles accepted: %v", roles)
		}
	}
	if _, _, err := s.CreateServiceAccount(ctx, owner, "bad scope", nil,
		"tenant:admin", expiresAt); err == nil {
		t.Fatal("unknown OAuth scope accepted")
	}
	if _, _, err := s.CreateServiceAccount(ctx, owner, "expired", nil,
		models.OAuthScopeLearnerRead, time.Now().UTC().Add(-time.Minute)); err == nil {
		t.Fatal("expired service account accepted")
	}

	nonAdmin := owner
	nonAdmin.Roles = []string{models.RoleAuditor}
	if _, _, err := s.CreateServiceAccount(ctx, nonAdmin, "unauthorized", nil,
		models.OAuthScopeLearnerRead, expiresAt); err == nil {
		t.Fatal("unprivileged service-account creation accepted")
	}
}
