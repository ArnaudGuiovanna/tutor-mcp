// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"slices"
	"time"
)

// ConsoleRoles are the tenant roles allowed into the browser console. Learners
// use their AI client; they have nothing to administer.
var ConsoleRoles = []string{RoleOwner, RoleAdmin, RolePedagogyManager, RoleTrainer}

// MFARequiredRoles lists roles that read learner data or administer the
// tenant. Their memberships cannot obtain a principal without a second factor.
var MFARequiredRoles = []string{RoleOwner, RoleAdmin, RolePedagogyManager, RoleTrainer}

// RolesRequireMFA reports whether any role in roles requires MFA.
func RolesRequireMFA(roles []string) bool {
	for _, role := range roles {
		if slices.Contains(MFARequiredRoles, role) {
			return true
		}
	}
	return false
}

// RolesAllowConsole reports whether any role in roles may open the console.
func RolesAllowConsole(roles []string) bool {
	for _, role := range roles {
		if slices.Contains(ConsoleRoles, role) {
			return true
		}
	}
	return false
}

// ConsoleSession is a server-side browser session bound to one membership.
// The token itself is never stored; TokenHash is its opaque hash.
type ConsoleSession struct {
	TokenHash         string
	UserID            string
	TenantID          string
	MembershipID      string
	MembershipVersion int64
	MFAVerifiedAt     *time.Time
	CreatedAt         time.Time
	LastSeenAt        time.Time
	ExpiresAt         time.Time
}

func (s ConsoleSession) TenantScope() TenantScope {
	return TenantScope{TenantID: s.TenantID, UserID: s.UserID, MembershipID: s.MembershipID}
}

// ConsoleMembership is the live authorization state behind a console session.
type ConsoleMembership struct {
	TenantName  string
	Email       string
	LearnerID   string
	Roles       []string
	Version     int64
	MFARequired bool
	MFAVerified bool
	MFAEnrolled bool
}

// TenantMember is one row of the console member list.
type TenantMember struct {
	MembershipID string
	UserID       string
	Email        string
	Roles        []string
	Status       string
	MFAEnrolled  bool
	CreatedAt    time.Time
}

// InvitationPreview is what an invitation link may reveal before acceptance.
type InvitationPreview struct {
	TenantName   string
	Email        string
	Roles        []string
	ExpiresAt    time.Time
	ExistingUser bool
}

// PendingSignup is an institution signup waiting for mailbox verification.
type PendingSignup struct {
	Email      string
	TenantName string
	TenantSlug string
	ExpiresAt  time.Time
}

// ConsoleSessionCredential is the opaque browser session token. The store
// resolves the session, and so the tenant, from its digest alone.
type ConsoleSessionCredential struct {
	Token string
}

// InvitationCredential is the secret carried by an invitation link.
type InvitationCredential struct {
	Token string
}

// SignupCredential is the secret carried by a signup verification link.
type SignupCredential struct {
	Token string
}
