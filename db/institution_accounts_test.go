// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"tutor-mcp/internal/testsupport"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func institutionTestStore(t *testing.T) *Store {
	t.Helper()
	s := setupTestDB(t)
	keyring, err := NewIntegrationSecretKeyring(
		"mfa:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "mfa",
	)
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(keyring)
	return s
}

// seedInstitutionOwner provisions an institution with its native owner and
// returns the owner scope.
func seedInstitutionOwner(t *testing.T, s *Store, slug string) models.TenantScope {
	t.Helper()
	return testsupport.ProvisionInstitution(t, s, slug)
}

// testMembership is the authorization state of one membership, read under
// its own tenant binding.
type testMembership struct {
	LearnerID string
	Email     string
	Roles     []string
	Version   int64
}

func loadTestMembership(ctx context.Context, s *Store, scope models.TenantScope) (*testMembership, error) {
	var membership testMembership
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		var rolesJSON string
		if err := scoped.(*Store).queryRow(txCtx, `SELECT COALESCE(tm.learner_id, ''), COALESCE(u.email, ''), tm.roles_json, tm.version
			FROM tenant_memberships tm JOIN users u ON u.id = tm.user_id
			WHERE tm.tenant_id = ? AND tm.id = ? AND tm.user_id = ? AND tm.status = 'active'`,
			scope.TenantID, scope.MembershipID, scope.UserID).Scan(
			&membership.LearnerID, &membership.Email, &rolesJSON, &membership.Version); err != nil {
			return err
		}
		return json.Unmarshal([]byte(rolesJSON), &membership.Roles)
	})
	if err != nil {
		return nil, err
	}
	return &membership, nil
}

// verifiedOwner returns the principal the membership's authorized sessions
// carry.
func verifiedOwner(t *testing.T, s *Store, scope models.TenantScope) (models.Principal, string) {
	t.Helper()
	ctx := context.Background()
	membership, err := loadTestMembership(ctx, s, scope)
	if err != nil {
		t.Fatal(err)
	}
	return models.Principal{
		UserID: scope.UserID, TenantID: scope.TenantID, MembershipID: scope.MembershipID,
		LearnerID: membership.LearnerID, Roles: membership.Roles,
		Scopes: []string{models.OAuthScopeLearner}, TokenVersion: membership.Version,
	}, ""
}

func TestUpdateTenantMemberGuards(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	ownerScope := seedInstitutionOwner(t, s, "acme")
	owner, _ := verifiedOwner(t, s, ownerScope)

	member := func(name string, roles ...string) *models.TenantMembership {
		t.Helper()
		identity := testsupport.ProvisionMember(t, s, owner.TenantID, name, roles...)
		return &models.TenantMembership{ID: identity.MembershipID, TenantID: identity.TenantID, UserID: identity.UserID, LearnerID: identity.LearnerID}
	}
	adminMembership := member("Admin", models.RoleAdmin)
	learner := member("Alice", models.RoleLearner)

	if err := s.UpdateTenantMember(ctx, owner, ownerScope.MembershipID, models.MembershipStatusSuspended, []string{models.RoleOwner}); err == nil {
		t.Fatal("owner suspended its own membership")
	}
	if err := s.UpdateTenantMember(ctx, owner, ownerScope.MembershipID, models.MembershipStatusActive, []string{models.RoleAdmin}); err == nil {
		t.Fatal("the last owner gave up the owner role")
	}

	adminScope := models.TenantScope{TenantID: owner.TenantID, UserID: adminMembership.UserID, MembershipID: adminMembership.ID}
	admin, _ := verifiedOwner(t, s, adminScope)
	if err := s.UpdateTenantMember(ctx, admin, ownerScope.MembershipID, models.MembershipStatusActive, []string{models.RoleAdmin}); err == nil {
		t.Fatal("an admin demoted the owner")
	}
	if err := s.UpdateTenantMember(ctx, admin, learner.ID, models.MembershipStatusActive, []string{models.RoleOwner}); err == nil {
		t.Fatal("an admin granted the owner role")
	}

	// Promoting a learner to trainer keeps the learner profile.
	if err := s.UpdateTenantMember(ctx, admin, learner.ID, models.MembershipStatusActive,
		[]string{models.RoleTrainer, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	learnerScope := models.TenantScope{TenantID: owner.TenantID, UserID: learner.UserID, MembershipID: learner.ID}
	state, err := loadTestMembership(ctx, s, learnerScope)
	if err != nil || state.LearnerID != learner.LearnerID {
		t.Fatalf("promoted membership = %+v, %v", state, err)
	}

	// Granting the learner role to an admin creates a learner profile.
	if err := s.UpdateTenantMember(ctx, owner, adminMembership.ID, models.MembershipStatusActive,
		[]string{models.RoleAdmin, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	state, err = loadTestMembership(ctx, s, adminScope)
	if err != nil || state.LearnerID == "" {
		t.Fatalf("admin learner profile = %+v, %v", state, err)
	}

	// A removed member no longer has an active membership.
	if err := s.UpdateTenantMember(ctx, owner, learner.ID, models.MembershipStatusRevoked, []string{models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTestMembership(ctx, s, learnerScope); err == nil {
		t.Fatal("revoked membership still loads")
	}
	learnerOnly := models.Principal{
		UserID: learner.UserID, TenantID: owner.TenantID, MembershipID: learner.ID,
		LearnerID: learner.LearnerID, Roles: []string{models.RoleLearner},
		Scopes: []string{models.OAuthScopeLearner}, TokenVersion: 1,
	}
	if err := s.UpdateTenantMember(ctx, learnerOnly, adminMembership.ID, models.MembershipStatusSuspended, []string{models.RoleAdmin}); err == nil {
		t.Fatal("a learner managed memberships")
	}
}
