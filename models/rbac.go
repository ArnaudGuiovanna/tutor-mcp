// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "slices"

type Permission string

const (
	PermissionTenantManage     Permission = "tenant:manage"
	PermissionMembershipManage Permission = "membership:manage"
	PermissionFormationWrite   Permission = "formation:write"
	PermissionCohortManage     Permission = "cohort:manage"
	PermissionProgressRead     Permission = "progress:read"
	// Reading raw assessment material is distinct from aggregate progression.
	// This permission never grants the right to certify an evaluation.
	PermissionAssessmentReview  Permission = "assessment:review"
	PermissionLearningSelf      Permission = "learning:self"
	PermissionBillingManage     Permission = "billing:manage"
	PermissionAuditRead         Permission = "audit:read"
	PermissionIntegrationManage Permission = "integration:manage"
	PermissionUsageRead         Permission = "usage:read"
)

// AuthorizationResource carries only trusted identifiers loaded from the
// route and persistence layer. SuppliedTenantID must never originate from a
// free-form client header.
type AuthorizationResource struct {
	TenantID          string
	OwnerUserID       string
	CohortID          string
	AssignedCohortIDs []string
}

var rolePermissions = map[string]map[Permission]bool{
	RoleOwner: {
		PermissionTenantManage: true, PermissionMembershipManage: true,
		PermissionFormationWrite: true, PermissionCohortManage: true,
		PermissionProgressRead: true, PermissionBillingManage: true,
		PermissionAuditRead: true, PermissionIntegrationManage: true,
		PermissionUsageRead:        true,
		PermissionAssessmentReview: true,
	},
	RoleAdmin: {
		PermissionTenantManage: true, PermissionMembershipManage: true,
		PermissionFormationWrite: true, PermissionCohortManage: true,
		PermissionProgressRead: true, PermissionAuditRead: true,
		PermissionIntegrationManage: true, PermissionUsageRead: true,
		PermissionAssessmentReview: true,
	},
	RolePedagogyManager: {
		PermissionFormationWrite: true, PermissionCohortManage: true,
		PermissionProgressRead: true, PermissionUsageRead: true,
		PermissionAssessmentReview: true,
	},
	RoleTrainer: {
		PermissionProgressRead: true, PermissionAssessmentReview: true,
	},
	RoleAuditor: {
		PermissionProgressRead: true, PermissionAuditRead: true,
		PermissionUsageRead: true,
	},
	RoleBillingAdmin: {
		PermissionBillingManage: true, PermissionUsageRead: true,
	},
	RoleLearner: {
		PermissionLearningSelf: true,
	},
	RoleServiceAccount: {},
	RoleSupport: {
		PermissionProgressRead: true, PermissionAuditRead: true, PermissionUsageRead: true,
	},
}

// Authorize is deny-by-default and always verifies tenant equality before
// looking at a role. Trainer access requires an explicit cohort assignment;
// learner access requires exact self ownership.
func (p Principal) Authorize(permission Permission, resource AuthorizationResource) bool {
	if p.Validate() != nil || resource.TenantID == "" || resource.TenantID != p.TenantID {
		return false
	}
	for _, role := range p.Roles {
		if !rolePermissions[role][permission] {
			continue
		}
		switch {
		case permission == PermissionLearningSelf:
			return resource.OwnerUserID != "" && resource.OwnerUserID == p.UserID
		case role == RoleTrainer && (permission == PermissionProgressRead || permission == PermissionAssessmentReview):
			if resource.CohortID != "" && slices.Contains(resource.AssignedCohortIDs, resource.CohortID) {
				return true
			}
			// An inapplicable cohort grant must not mask another role's grant.
			continue
		default:
			return true
		}
	}
	return false
}

func KnownPermissions() []Permission {
	return []Permission{
		PermissionTenantManage, PermissionMembershipManage, PermissionFormationWrite,
		PermissionCohortManage, PermissionProgressRead, PermissionLearningSelf,
		PermissionBillingManage, PermissionAuditRead, PermissionIntegrationManage,
		PermissionUsageRead,
		PermissionAssessmentReview,
	}
}
