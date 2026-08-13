// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"testing"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestTenantIdentityBackfillAndNewLearnerProvisioning(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	principal, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearnerRead})
	if err != nil {
		t.Fatal(err)
	}
	if principal.TenantID != models.LegacyTenantID || principal.UserID != "L1" ||
		principal.MembershipID != "membership_legacy_L1" || principal.TokenVersion != 1 {
		t.Fatalf("backfilled principal = %#v", principal)
	}
	if err := s.ValidatePrincipal(ctx, principal); err != nil {
		t.Fatalf("validate backfilled principal: %v", err)
	}

	learner, err := s.CreateLearner(ctx, "second@example.com", "hash", "", "")
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.GetPrincipalForLearner(ctx, learner.ID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatalf("new learner identity was not provisioned atomically: %v", err)
	}
	if created.UserID != learner.ID || created.MembershipID != "membership_legacy_"+learner.ID {
		t.Fatalf("new learner principal = %#v", created)
	}
}

func TestMembershipSuspensionAndRoleChangeImmediatelyInvalidatePrincipal(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	principal, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearnerRead})
	if err != nil {
		t.Fatal(err)
	}

	version, err := s.SetMembershipAuthorization(ctx, principal.TenantScope(), models.MembershipStatusSuspended, []string{models.RoleLearner})
	if err != nil {
		t.Fatal(err)
	}
	if version != principal.TokenVersion+1 {
		t.Fatalf("suspension version = %d, want %d", version, principal.TokenVersion+1)
	}
	if err := s.ValidatePrincipal(ctx, principal); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("old bearer after suspension = %v, want ErrInvalidPrincipal", err)
	}
	if _, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearnerRead}); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("resolve suspended membership = %v, want ErrInvalidPrincipal", err)
	}

	if _, err := s.SetMembershipAuthorization(ctx, principal.TenantScope(), models.MembershipStatusActive,
		[]string{models.RoleLearner, models.RoleAuditor}); err != nil {
		t.Fatal(err)
	}
	refreshed, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearnerRead})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.TokenVersion <= version || len(refreshed.Roles) != 2 {
		t.Fatalf("refreshed principal = %#v", refreshed)
	}
	if err := s.ValidatePrincipal(ctx, principal); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("old role-bearing token after role change = %v", err)
	}
}

func TestMembershipAuthorizationRequiresExactTenantUserTuple(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	principal, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	wrong := principal.TenantScope()
	wrong.TenantID = "tenant-b"
	if _, err := s.SetMembershipAuthorization(ctx, wrong, models.MembershipStatusRevoked,
		[]string{models.RoleLearner}); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("cross-tenant mutation = %v, want ErrNotFound", err)
	}
	if err := s.ValidatePrincipal(ctx, principal); err != nil {
		t.Fatalf("cross-tenant attempt changed real membership: %v", err)
	}
}

func TestGlobalUsersAreNeverMergedByNormalizedEmail(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	_, err := s.exec(ctx, `INSERT INTO users
        (id, email, normalized_email, password_hash, status, token_version, created_at, updated_at)
        VALUES (?, ?, ?, ?, 'active', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		"separate-user", "TEST@test.com", "test@test.com", "separate-hash")
	if err != nil {
		t.Fatalf("same email evidence must not force an identity merge: %v", err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM users WHERE normalized_email = ?`, "test@test.com").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("users sharing normalized email = %d, want 2 distinct identities", count)
	}
}
