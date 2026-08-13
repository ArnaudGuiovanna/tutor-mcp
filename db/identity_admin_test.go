// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"strings"
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
	if _, err := s.RecordMembershipMFAVerification(ctx, learner.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	owner, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestPrivilegedMembershipRequiresMFAAndInvalidatesOldToken(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	learner, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, learner.TenantScope(), models.MembershipStatusActive, []string{models.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner}); err == nil {
		t.Fatal("admin principal issued before MFA verification")
	}
	if err := s.ValidatePrincipal(ctx, learner); err == nil {
		t.Fatal("pre-role-change token remained valid")
	}
	if _, err := s.RecordMembershipMFAVerification(ctx, learner.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	admin, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatalf("MFA-verified admin principal: %v", err)
	}
	if len(admin.Roles) != 1 || admin.Roles[0] != models.RoleAdmin {
		t.Fatalf("admin roles = %v", admin.Roles)
	}
}

func TestInvitationAcceptsVerifiedIdentityIntoSeparateTenantProfile(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	now := time.Now().UTC()
	if _, err := s.exec(ctx, `INSERT INTO tenants
        (id, slug, name, status, region, policy_json, created_at, updated_at)
        VALUES ('tenant-b', 'tenant-b', 'Tenant B', 'active', 'default', '{}', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `INSERT INTO users
        (id, email, normalized_email, password_hash, status, email_verified_at,
         token_version, created_at, updated_at)
        VALUES ('user-b', 'invitee@example.com', 'invitee@example.com', 'hash', 'active', ?, 1, ?, ?)`,
		now, now, now); err != nil {
		t.Fatal(err)
	}
	// Invite from an owner in tenant B, modeled as an explicit tenant-bound
	// control-plane principal rather than an authority-bearing HTTP header.
	actor := owner
	actor.TenantID = "tenant-b"
	actor.MembershipID = "owner-b"
	actor.LearnerID = ""
	actor.TokenVersion = 1
	if _, err := s.exec(ctx, `INSERT INTO tenant_memberships
        (id, tenant_id, user_id, roles_json, status, version, mfa_required, mfa_verified_at, created_at, updated_at)
        VALUES ('owner-b', 'tenant-b', 'L1', '["owner"]', 'active', 1, 1, ?, ?, ?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	invitation, raw, err := s.CreateTenantInvitation(ctx, actor, "INVITEE@example.com", []string{models.RoleLearner}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || strings.Contains(invitation.ID, raw) {
		t.Fatal("invitation capability was not returned safely")
	}
	var leaked int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM tenant_invitations WHERE token_hash = ?`, raw).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("raw invitation token persisted")
	}
	membership, err := s.AcceptTenantInvitation(ctx, raw, "user-b")
	if err != nil {
		t.Fatal(err)
	}
	if membership.TenantID != "tenant-b" || membership.UserID != "user-b" || membership.LearnerID == "" {
		t.Fatalf("accepted membership = %#v", membership)
	}
	if _, err := s.AcceptTenantInvitation(ctx, raw, "user-b"); err == nil {
		t.Fatal("invitation replay accepted")
	}
	var tenantID, userID string
	if err := s.queryRow(ctx, `SELECT tenant_id, user_id FROM learners WHERE id = ?`, membership.LearnerID).Scan(&tenantID, &userID); err != nil {
		t.Fatal(err)
	}
	if tenantID != "tenant-b" || userID != "user-b" {
		t.Fatalf("separate learner profile scope = %s/%s", tenantID, userID)
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

func TestInvitationRejectsEmailMismatch(t *testing.T) {
	s := setupTestDB(t)
	owner := ownerPrincipal(t, s)
	invitation, raw, err := s.CreateTenantInvitation(context.Background(), owner,
		"expected@example.com", []string{models.RoleAuditor}, time.Now().Add(time.Hour))
	if err != nil || invitation == nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptTenantInvitation(context.Background(), raw, "L1"); err == nil {
		t.Fatal("invitation accepted by a different verified email")
	}
	var status string
	if err := s.queryRow(context.Background(), `SELECT status FROM tenant_invitations WHERE id = ?`, invitation.ID).Scan(&status); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("failed acceptance consumed invitation: status=%q", status)
	}
}
