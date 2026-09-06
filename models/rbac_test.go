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

func TestAssessmentReviewPermissionIsSeparateAndCohortBound(t *testing.T) {
	resource := AuthorizationResource{TenantID: LegacyTenantID}
	for _, role := range []string{RoleOwner, RoleAdmin, RolePedagogyManager} {
		if !principalWithRole(role).Authorize(PermissionAssessmentReview, resource) {
			t.Errorf("%s cannot review", role)
		}
	}
	for _, role := range []string{RoleLearner, RoleTrainer, RoleAuditor, RoleBillingAdmin, RoleServiceAccount, RoleSupport} {
		if principalWithRole(role).Authorize(PermissionAssessmentReview, resource) {
			t.Errorf("%s can read unscoped raw assessment material", role)
		}
	}
	trainer := principalWithRole(RoleTrainer)
	resource.CohortID = "assigned"
	resource.AssignedCohortIDs = []string{"different"}
	if trainer.Authorize(PermissionAssessmentReview, resource) {
		t.Fatal("trainer can review a different cohort")
	}
	resource.AssignedCohortIDs = []string{"assigned"}
	if !trainer.Authorize(PermissionAssessmentReview, resource) {
		t.Fatal("assigned trainer cannot review")
	}
	resource.TenantID = "foreign"
	if trainer.Authorize(PermissionAssessmentReview, resource) {
		t.Fatal("trainer can review another tenant")
	}
	for _, roles := range [][]string{{RoleTrainer, RoleAdmin}, {RoleAdmin, RoleTrainer}} {
		combined := principalWithRole(RoleAdmin)
		combined.Roles = roles
		for _, permission := range []Permission{PermissionAssessmentReview, PermissionProgressRead} {
			if !combined.Authorize(permission, AuthorizationResource{TenantID: LegacyTenantID}) {
				t.Errorf("role ordering %v hid %s grant", roles, permission)
			}
		}
	}
}
