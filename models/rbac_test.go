// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "testing"

func principalWithRole(role string) Principal {
	p := LegacyPrincipal("user-a")
	p.Roles = []string{role}
	return p
}

func TestRBACRolePermissionMatrixDenyByDefault(t *testing.T) {
	resource := AuthorizationResource{TenantID: LegacyTenantID, OwnerUserID: "user-a"}
	cases := []struct {
		role       string
		permission Permission
		allow      bool
	}{
		{RoleOwner, PermissionTenantManage, true},
		{RoleAdmin, PermissionMembershipManage, true},
		{RolePedagogyManager, PermissionFormationWrite, true},
		{RoleTrainer, PermissionFormationWrite, false},
		{RoleAuditor, PermissionAuditRead, true},
		{RoleAuditor, PermissionFormationWrite, false},
		{RoleBillingAdmin, PermissionBillingManage, true},
		{RoleBillingAdmin, PermissionProgressRead, false},
		{RoleLearner, PermissionLearningSelf, true},
		{RoleLearner, PermissionProgressRead, false},
		{RoleServiceAccount, PermissionUsageRead, false},
	}
	for _, test := range cases {
		if got := principalWithRole(test.role).Authorize(test.permission, resource); got != test.allow {
			t.Errorf("role=%s permission=%s allow=%v, want %v", test.role, test.permission, got, test.allow)
		}
	}
	if principalWithRole(RoleOwner).Authorize(Permission("unknown:action"), resource) {
		t.Fatal("even an owner must be denied an unknown action")
	}
}

func TestRBACRejectsCrossTenantAndNonSelfLearning(t *testing.T) {
	learner := principalWithRole(RoleLearner)
	if learner.Authorize(PermissionLearningSelf, AuthorizationResource{TenantID: "tenant-b", OwnerUserID: learner.UserID}) {
		t.Fatal("cross-tenant resource authorized")
	}
	if learner.Authorize(PermissionLearningSelf, AuthorizationResource{TenantID: learner.TenantID, OwnerUserID: "other"}) {
		t.Fatal("learner authorized for another user's learning data")
	}
}

func TestTrainerProgressRequiresAssignedCohort(t *testing.T) {
	trainer := principalWithRole(RoleTrainer)
	resource := AuthorizationResource{
		TenantID: trainer.TenantID, CohortID: "cohort-a",
		AssignedCohortIDs: []string{"cohort-b"},
	}
	if trainer.Authorize(PermissionProgressRead, resource) {
		t.Fatal("trainer authorized outside assigned cohorts")
	}
	resource.AssignedCohortIDs = append(resource.AssignedCohortIDs, "cohort-a")
	if !trainer.Authorize(PermissionProgressRead, resource) {
		t.Fatal("trainer denied inside assigned cohort")
	}
}
