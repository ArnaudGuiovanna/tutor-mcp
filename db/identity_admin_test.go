// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func ownerPrincipal(t *testing.T, s *Store) models.Principal {
	t.Helper()
	ctx := context.Background()
	learner, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, learner.TenantScope(), models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	owner, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func ownerLearnerPrincipal(t *testing.T, s *Store) models.Principal {
	t.Helper()
	p := ownerPrincipal(t, s)
	if _, err := s.SetMembershipAuthorization(t.Context(), p.TenantScope(), models.MembershipStatusActive, []string{models.RoleOwner, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPrincipalForLearner(t.Context(), "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A staff role takes effect at once (there is no separate second-factor
// gate on memberships any more) and the role change invalidates every token
// issued before it.
func TestPrivilegedMembershipRoleChangeInvalidatesOldToken(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	learner, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, learner.TenantScope(), models.MembershipStatusActive, []string{models.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidatePrincipal(ctx, learner); err == nil {
		t.Fatal("pre-role-change token remained valid")
	}
	admin, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatalf("admin principal: %v", err)
	}
	if len(admin.Roles) != 1 || admin.Roles[0] != models.RoleAdmin {
		t.Fatalf("admin roles = %v", admin.Roles)
	}
	if err := s.ValidatePrincipal(ctx, admin); err != nil {
		t.Fatalf("admin principal rejected: %v", err)
	}
}

func TestExternalIdentityNeverLinksByEmailAndAuditIsAppendOnly(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.exec(ctx, `INSERT INTO users
        (id, email, normalized_email, password_hash, status, email_verified_at, token_version, created_at, updated_at)
        VALUES ('other-user', 'test@test.com', 'test@test.com', 'hash', 'active', ?, 1, ?, ?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	linked, err := s.LinkExternalIdentity(ctx, "other-user", models.ExternalIdentityInput{
		Provider: "oidc", Issuer: "https://idp.example", Subject: "subject-1", EmailAtLink: "test@test.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if linked.UserID != "other-user" {
		t.Fatal("external identity was merged to the user sharing its email")
	}
	if _, err := s.LinkExternalIdentity(ctx, "L1", models.ExternalIdentityInput{
		Provider: "oidc", Issuer: "https://idp.example", Subject: "subject-1", EmailAtLink: "test@test.com",
	}); err == nil {
		t.Fatal("issuer/subject collision linked to a second user")
	}

	owner := ownerPrincipal(t, s)
	if err := s.WithTenantTx(ctx, owner.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		return scoped.AppendAuditEvent(txCtx, owner, models.AuditEvent{
			Action: "tenant.policy.update", TargetType: "tenant", TargetID: owner.TenantID,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.root.Exec(`UPDATE audit_events SET reason = 'tampered'`); err == nil {
		t.Fatal("append-only audit row was mutable")
	}
	if _, err := s.root.Exec(`DELETE FROM audit_events`); err == nil {
		t.Fatal("append-only audit row was deletable")
	}
}
