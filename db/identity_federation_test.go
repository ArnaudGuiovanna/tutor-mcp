// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"

	"tutor-mcp/models"
)

func TestFederatedProvisioningNeverMergesByEmailAndProviderRevocationPropagates(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	provider, err := s.ConfigureIdentityProvider(ctx, owner, "oidc", "https://idp.example")
	if err != nil {
		t.Fatal(err)
	}
	assertion := func(subject string) models.VerifiedFederatedIdentityAssertion {
		return models.VerifiedFederatedIdentityAssertion{
			TenantID: owner.TenantID, ProviderID: provider.ID, Issuer: provider.Issuer,
			Subject: subject, Email: "same-email@example.com",
		}
	}
	first, err := s.ProvisionFederatedMembership(ctx, owner, assertion("subject-a"), []string{models.RoleLearner})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ProvisionFederatedMembership(ctx, owner, assertion("subject-b"), []string{models.RoleLearner})
	if err != nil {
		t.Fatal(err)
	}
	if first.UserID == second.UserID || first.ID == second.ID || first.LearnerID == second.LearnerID {
		t.Fatalf("same-email subjects merged: first=%#v second=%#v", first, second)
	}
	principal, err := s.GetFederatedPrincipal(ctx, assertion("subject-a"), []string{models.OAuthScopeLearnerRead})
	if err != nil {
		t.Fatal(err)
	}
	if principal.UserID != first.UserID || principal.TenantID != owner.TenantID ||
		principal.MembershipID != first.ID || principal.LearnerID != first.LearnerID {
		t.Fatalf("federated principal = %#v", principal)
	}
	if err := s.ValidatePrincipal(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFederatedPrincipal(ctx, models.VerifiedFederatedIdentityAssertion{
		TenantID: owner.TenantID, ProviderID: provider.ID, Issuer: "https://attacker.example",
		Subject: "subject-a",
	}, []string{models.OAuthScopeLearnerRead}); err == nil {
		t.Fatal("issuer confusion accepted")
	}

	if err := s.SetIdentityProviderStatus(ctx, owner, provider.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFederatedPrincipal(ctx, assertion("subject-a"), []string{models.OAuthScopeLearnerRead}); err == nil {
		t.Fatal("revoked provider still issued a principal")
	}
	if err := s.ValidatePrincipal(ctx, principal); err == nil {
		t.Fatal("provider revocation did not invalidate an existing principal")
	}
	var version int64
	var status string
	if err := s.queryRow(ctx, `SELECT status, version FROM tenant_memberships
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, first.ID).Scan(&status, &version); err != nil {
		t.Fatal(err)
	}
	if status != models.MembershipStatusSuspended || version <= first.Version {
		t.Fatalf("revoked membership status=%s version=%d", status, version)
	}
}

func TestFederatedAssertionRequiresExplicitTenantAndProvider(t *testing.T) {
	s := setupTestDB(t)
	if _, err := s.GetFederatedPrincipal(context.Background(), models.VerifiedFederatedIdentityAssertion{
		ProviderID: "provider", Issuer: "issuer", Subject: "subject",
	}, []string{models.OAuthScopeLearnerRead}); err == nil {
		t.Fatal("tenantless federated assertion accepted")
	}
}
