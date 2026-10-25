// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

// NativeIdentity contains public Nostr keys only. Tenant and membership fields
// are resolved for one institution; the underlying user identity is global.
// Private identity and recovery keys never enter server persistence.
type NativeIdentity struct {
	ID                string    `json:"id"`
	UserID            string    `json:"user_id"`
	TenantID          string    `json:"tenant_id"`
	MembershipID      string    `json:"membership_id"`
	LearnerID         string    `json:"learner_id,omitempty"`
	PublicKey         string    `json:"public_key"`
	RecoveryPublicKey string    `json:"recovery_public_key"`
	DisplayName       string    `json:"display_name"`
	Status            string    `json:"status"`
	Version           int64     `json:"key_version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// NativeChallenge binds one signature to one exact HTTP operation. TokenHash
// is SHA-256 of a random 256-bit nonce; the raw nonce is returned to the client
// and is not retained. Consumption must be atomic, durable and single-use.
type NativeChallenge struct {
	TokenHash   string     `json:"-"`
	IdentityID  string     `json:"identity_id"`
	TenantID    string     `json:"tenant_id"`
	PublicKey   string     `json:"public_key"`
	Action      string     `json:"action"`
	Scopes      []string   `json:"scopes"`
	KeyVersion  int64      `json:"key_version"`
	URL         string     `json:"url"`
	Method      string     `json:"method"`
	PayloadHash string     `json:"payload_hash"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ConsumedAt  *time.Time `json:"consumed_at,omitempty"`
}

// NativeSession is the revocable, short-lived grant behind azp=native:<ID>.
// Scope and membership versions are checked against current durable state on
// every protected request, including requests in an existing MCP session.
type NativeSession struct {
	ID           string     `json:"session_id"`
	IdentityID   string     `json:"identity_id"`
	TenantID     string     `json:"tenant_id"`
	UserID       string     `json:"user_id"`
	MembershipID string     `json:"membership_id"`
	LearnerID    string     `json:"learner_id,omitempty"`
	KeyVersion   int64      `json:"key_version"`
	Scopes       []string   `json:"scopes"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

// NativeOwnerProvision is accepted only through the explicit control plane.
// A key presented to a public authentication endpoint never creates an owner.
type NativeOwnerProvision struct {
	TenantID          string `json:"tenant_id"`
	DisplayName       string `json:"display_name"`
	PublicKey         string `json:"public_key"`
	RecoveryPublicKey string `json:"recovery_public_key"`
}

type NativeLearningMembership struct {
	LearnerID string   `json:"learner_id"`
	Roles     []string `json:"roles"`
	Changed   bool     `json:"changed"`
}
