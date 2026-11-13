// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Package testsupport provisions institutions for tests through the path
// production uses: the control plane provisions the tenant, then native key
// identities are bootstrapped as its owner or provisioned as its members.
// It depends only on the store methods it calls, so the db package's own
// tests can use it too.
package testsupport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"tutor-mcp/models"
)

// InstitutionStore is the provisioning surface of the SQL store.
type InstitutionStore interface {
	ProvisionTenant(ctx context.Context, actor models.ControlPlanePrincipal, slug, name, region, planID string) (*models.Tenant, error)
	BootstrapNativeOwner(ctx context.Context, actor models.ControlPlanePrincipal, request models.NativeOwnerProvision) (*models.NativeIdentity, error)
	ProvisionNativeMember(ctx context.Context, actor models.ControlPlanePrincipal, request models.NativeOwnerProvision, roles []string) (*models.NativeIdentity, error)
}

// Operator is the platform operator identity used to provision test tenants.
func Operator() models.ControlPlanePrincipal {
	return models.ControlPlanePrincipal{
		ActorID: "test-operator", Roles: []string{models.RolePlatformAdmin},
		Reason: "test institution provisioning", RequestID: "test-provisioning",
	}
}

// NativePublicKey returns a fresh, valid native identity public key (x-only
// Schnorr key, lowercase hex).
func NativePublicKey(t testing.TB) string {
	t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	_, public := btcec.PrivKeyFromBytes(secret)
	return hex.EncodeToString(schnorr.SerializePubKey(public))
}

// ProvisionTenant provisions an active tenant on the legacy plan.
func ProvisionTenant(t testing.TB, s InstitutionStore, slug string) *models.Tenant {
	t.Helper()
	tenant, err := s.ProvisionTenant(context.Background(), Operator(), slug, "Institution "+slug, "test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}

// BootstrapOwner bootstraps the tenant's first owner (owner role only, no
// learner profile).
func BootstrapOwner(t testing.TB, s InstitutionStore, tenantID string) *models.NativeIdentity {
	t.Helper()
	owner, err := s.BootstrapNativeOwner(context.Background(), Operator(), models.NativeOwnerProvision{
		TenantID: tenantID, DisplayName: "Owner", PublicKey: NativePublicKey(t), RecoveryPublicKey: NativePublicKey(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

// ProvisionMember provisions a member with roles; RoleLearner adds a learner
// profile.
func ProvisionMember(t testing.TB, s InstitutionStore, tenantID, displayName string, roles ...string) *models.NativeIdentity {
	t.Helper()
	member, err := s.ProvisionNativeMember(context.Background(), Operator(), models.NativeOwnerProvision{
		TenantID: tenantID, DisplayName: displayName, PublicKey: NativePublicKey(t), RecoveryPublicKey: NativePublicKey(t),
	}, roles)
	if err != nil {
		t.Fatal(err)
	}
	return member
}

// ProvisionInstitution provisions a tenant and its owner and returns the
// owner's tenant scope.
func ProvisionInstitution(t testing.TB, s InstitutionStore, slug string) models.TenantScope {
	t.Helper()
	return Scope(BootstrapOwner(t, s, ProvisionTenant(t, s, slug).ID))
}

// Scope is the tenant scope of a provisioned identity's membership.
func Scope(identity *models.NativeIdentity) models.TenantScope {
	return models.TenantScope{
		TenantID: identity.TenantID, UserID: identity.UserID,
		MembershipID: identity.MembershipID, LearnerID: identity.LearnerID,
	}
}
