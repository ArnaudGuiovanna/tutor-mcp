// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// LegacyTenantID is the stable tenant provisioned for data created before
	// multi-tenancy. It is a real tenant identifier, not a wildcard.
	LegacyTenantID = "tenant_legacy"

	RoleOwner            = "owner"
	RoleAdmin            = "admin"
	RolePedagogyManager  = "pedagogy_manager"
	RoleTrainer          = "trainer"
	RoleAuditor          = "auditor"
	RoleBillingAdmin     = "billing_admin"
	RoleLearner          = "learner"
	RoleServiceAccount   = "service_account"
	RoleSupport          = "support"
	legacyMembershipBase = "membership_legacy_"
)

// TenantScope is the minimum identity carried across tenant-owned persistence
// boundaries. EnrollmentID is optional until a learning enrollment has been
// selected; LearnerID is retained during the expand/backfill migration.
type TenantScope struct {
	TenantID     string
	UserID       string
	MembershipID string
	EnrollmentID string
	LearnerID    string
}

// Validate rejects an absent or ambiguous tenant identity. IDs are opaque and
// must already be canonical: silently trimming them could select another row.
func (s TenantScope) Validate() error {
	for name, value := range map[string]string{
		"tenant_id":     s.TenantID,
		"user_id":       s.UserID,
		"membership_id": s.MembershipID,
	} {
		if err := validatePrincipalID(name, value); err != nil {
			return err
		}
	}
	if s.EnrollmentID != "" {
		if err := validatePrincipalID("enrollment_id", s.EnrollmentID); err != nil {
			return err
		}
	}
	if s.LearnerID != "" {
		if err := validatePrincipalID("learner_id", s.LearnerID); err != nil {
			return err
		}
	}
	return nil
}

// Principal is the authenticated identity for exactly one tenant membership.
// A user with two memberships receives two distinct principals and tokens.
type Principal struct {
	UserID       string
	TenantID     string
	MembershipID string
	LearnerID    string
	Roles        []string
	Scopes       []string
	TokenVersion int64
}

func (p Principal) TenantScope() TenantScope {
	return TenantScope{
		TenantID:     p.TenantID,
		UserID:       p.UserID,
		MembershipID: p.MembershipID,
		LearnerID:    p.LearnerID,
	}
}

// Validate centralizes all fail-closed checks performed before a principal is
// admitted to a business context.
func (p Principal) Validate() error {
	if err := p.TenantScope().Validate(); err != nil {
		return err
	}
	if p.TokenVersion < 1 {
		return fmt.Errorf("token_version must be positive")
	}
	if len(p.Roles) == 0 {
		return fmt.Errorf("at least one role is required")
	}
	seenRoles := make(map[string]struct{}, len(p.Roles))
	for _, role := range p.Roles {
		if !ValidTenantRole(role) {
			return fmt.Errorf("unsupported role %q", role)
		}
		if _, duplicate := seenRoles[role]; duplicate {
			return fmt.Errorf("duplicate role %q", role)
		}
		seenRoles[role] = struct{}{}
	}
	canonicalScope, err := CanonicalOAuthScope(strings.Join(p.Scopes, " "))
	if err != nil {
		return fmt.Errorf("invalid principal scopes: %w", err)
	}
	if canonicalScope != strings.Join(p.Scopes, " ") {
		return fmt.Errorf("principal scopes must be canonical")
	}
	return nil
}

func ValidTenantRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RolePedagogyManager, RoleTrainer, RoleAuditor,
		RoleBillingAdmin, RoleLearner, RoleServiceAccount, RoleSupport:
		return true
	default:
		return false
	}
}

// SessionBindingID is deliberately derived from tenant + membership + user.
// It prevents a transport session initialized in tenant A from being replayed
// with another valid membership of the same global user in tenant B.
func (p Principal) SessionBindingID() string {
	sum := sha256.Sum256([]byte(p.TenantID + "\x00" + p.MembershipID + "\x00" + p.UserID))
	return "principal:" + hex.EncodeToString(sum[:])
}

// LegacyPrincipal provides an explicit, temporary bridge for the pre-tenant
// OAuth flow. The resulting token remains tenant-bound and is never global.
func LegacyPrincipal(learnerID string) Principal {
	return Principal{
		UserID:       learnerID,
		TenantID:     LegacyTenantID,
		MembershipID: legacyMembershipBase + learnerID,
		LearnerID:    learnerID,
		Roles:        []string{RoleLearner},
		Scopes:       []string{OAuthScopeLearner},
		TokenVersion: 1,
	}
}

func validatePrincipalID(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be canonical", name)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s contains an invalid byte", name)
	}
	if len(value) > 255 {
		return fmt.Errorf("%s is too long", name)
	}
	return nil
}
