// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

const (
	TenantStatusActive    = "active"
	TenantStatusSuspended = "suspended"
	TenantStatusClosed    = "closed"

	UserStatusPending   = "pending"
	UserStatusActive    = "active"
	UserStatusSuspended = "suspended"
	UserStatusRevoked   = "revoked"

	MembershipStatusInvited   = "invited"
	MembershipStatusActive    = "active"
	MembershipStatusSuspended = "suspended"
	MembershipStatusRevoked   = "revoked"
)

type Tenant struct {
	ID         string
	Slug       string
	Name       string
	Status     string
	Region     string
	PolicyJSON string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// User is a global authentication identity. It intentionally carries no
// tenant role or pedagogical profile.
type User struct {
	ID              string
	Email           string
	NormalizedEmail string
	PasswordHash    string
	Status          string
	EmailVerifiedAt *time.Time
	TokenVersion    int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type TenantMembership struct {
	ID         string
	TenantID   string
	TenantName string
	UserID     string
	LearnerID  string
	Roles      []string
	Status     string
	Version    int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type ExternalIdentity struct {
	ID          string
	UserID      string
	Provider    string
	Issuer      string
	Subject     string
	EmailAtLink string
	CreatedAt   time.Time
	LastSeenAt  time.Time
}

type TenantInvitation struct {
	ID                   string
	TenantID             string
	Email                string
	NormalizedEmail      string
	Roles                []string
	Status               string
	CreatedBy            string
	CreatedAt            time.Time
	ExpiresAt            time.Time
	AcceptedAt           *time.Time
	AcceptedUserID       string
	AcceptedMembershipID string
}

type AuditEvent struct {
	ID           int64
	TenantID     string
	ActorUserID  string
	MembershipID string
	Action       string
	TargetType   string
	TargetID     string
	RequestID    string
	Reason       string
	DetailsJSON  string
	Result       string
	TraceID      string
	OccurredAt   time.Time
}

type ExternalIdentityInput struct {
	Provider    string
	Issuer      string
	Subject     string
	EmailAtLink string
}

type TenantIdentityProvider struct {
	ID        string
	TenantID  string
	Kind      string
	Issuer    string
	Status    string
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// VerifiedFederatedIdentityAssertion may only be constructed by an OIDC/SAML
// adapter after signature, issuer, audience, time and nonce validation. Email
// is profile metadata and is never used as the identity key.
type VerifiedFederatedIdentityAssertion struct {
	TenantID   string
	ProviderID string
	Issuer     string
	Subject    string
	Email      string
}

type ServiceAccount struct {
	ID         string
	TenantID   string
	Name       string
	ClientID   string
	Roles      []string
	Scopes     []string
	Status     string
	Version    int64
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}
