// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"fmt"
	"strings"
	"time"
)

const (
	// OAuthScopeLearner is the bounded legacy bundle. It grants exactly the
	// learner read and write capabilities below; it must never be treated as a
	// wildcard for future scopes.
	OAuthScopeLearner          = "learner"
	OAuthScopeLearnerRead      = "learner:read"
	OAuthScopeLearnerWrite     = "learner:write"
	OAuthScopeLearnerReadWrite = "learner:read learner:write"
)

// CanonicalOAuthScope validates the complete supported scope vocabulary and
// returns its stable persisted/JWT representation. The legacy bundle cannot be
// mixed with granular scopes, and duplicate scopes are rejected.
func CanonicalOAuthScope(raw string) (string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "", fmt.Errorf("scope is required")
	}
	seen := make(map[string]bool, len(fields))
	for _, scope := range fields {
		if seen[scope] {
			return "", fmt.Errorf("duplicate scope %q", scope)
		}
		seen[scope] = true
		switch scope {
		case OAuthScopeLearner, OAuthScopeLearnerRead, OAuthScopeLearnerWrite:
		default:
			return "", fmt.Errorf("unsupported scope %q", scope)
		}
	}
	if seen[OAuthScopeLearner] {
		if len(seen) != 1 {
			return "", fmt.Errorf("legacy learner scope cannot be combined with granular scopes")
		}
		return OAuthScopeLearner, nil
	}
	switch {
	case seen[OAuthScopeLearnerRead] && seen[OAuthScopeLearnerWrite]:
		return OAuthScopeLearnerReadWrite, nil
	case seen[OAuthScopeLearnerRead]:
		return OAuthScopeLearnerRead, nil
	case seen[OAuthScopeLearnerWrite]:
		return OAuthScopeLearnerWrite, nil
	default:
		return "", fmt.Errorf("unsupported scope combination")
	}
}

// OAuthScopeAllows reports whether a granted canonical scope contains one
// exact granular capability. The legacy bundle is deliberately bounded to the
// two learner capabilities known when it was issued.
func OAuthScopeAllows(granted, required string) bool {
	if required != OAuthScopeLearnerRead && required != OAuthScopeLearnerWrite {
		return false
	}
	switch granted {
	case OAuthScopeLearner, OAuthScopeLearnerReadWrite:
		return true
	case OAuthScopeLearnerRead:
		return required == OAuthScopeLearnerRead
	case OAuthScopeLearnerWrite:
		return required == OAuthScopeLearnerWrite
	default:
		// Context and persistence boundaries canonicalize grants before use.
		// Exact matching here both fails closed for malformed values and keeps
		// the per-tool authorization hot path allocation-free.
		return false
	}
}

// OAuthScopeCanNarrow allows refresh-time preservation or least-privilege
// reduction, never expansion. A granular grant cannot be converted back into
// the legacy bundle even when the current capabilities happen to be equal.
func OAuthScopeCanNarrow(granted, requested string) bool {
	grantedCanonical, err := CanonicalOAuthScope(granted)
	if err != nil {
		return false
	}
	requestedCanonical, err := CanonicalOAuthScope(requested)
	if err != nil {
		return false
	}
	if grantedCanonical == OAuthScopeLearner {
		return requestedCanonical == OAuthScopeLearner ||
			requestedCanonical == OAuthScopeLearnerRead ||
			requestedCanonical == OAuthScopeLearnerWrite ||
			requestedCanonical == OAuthScopeLearnerReadWrite
	}
	if requestedCanonical == OAuthScopeLearner {
		return false
	}
	for _, required := range strings.Fields(requestedCanonical) {
		if !OAuthScopeAllows(grantedCanonical, required) {
			return false
		}
	}
	return true
}

// AuthCode holds the authorization code state (persisted in DB).
// Moved from package db so the store port can return it without consumers importing db.
type AuthCode struct {
	Code                string
	UserID              string
	TenantID            string
	MembershipID        string
	MembershipVersion   int64
	LearnerID           string
	CodeChallenge       string
	CodeChallengeMethod string
	ClientID            string
	RedirectURI         string
	Resource            string
	Scope               string
	ExpiresAt           time.Time
}

func (a AuthCode) Principal(roles []string) Principal {
	return Principal{
		UserID:       a.UserID,
		TenantID:     a.TenantID,
		MembershipID: a.MembershipID,
		LearnerID:    a.LearnerID,
		Roles:        append([]string(nil), roles...),
		Scopes:       strings.Fields(a.Scope),
		TokenVersion: a.MembershipVersion,
	}
}

// OAuthClient is a dynamically-registered OAuth client.
// RedirectURIs holds the JSON array as persisted.
// ClientSecretHash is a bcrypt digest of the secret; empty for public (PKCE-only) clients.
// Moved from package db so the store port can return it without consumers importing db.
type OAuthClient struct {
	ClientID         string
	ClientName       string
	RedirectURIs     string
	ClientSecretHash string
	ExpiresAt        *time.Time // nil for preregistered/CIMD clients
}

// DCRInitialAccessToken is the safe metadata for one dynamic-client
// registration capability. TokenHash is populated only at persistence/admin
// boundaries and must never be exposed by HTTP discovery or logs.
type DCRInitialAccessToken struct {
	TokenID           string
	TokenHash         string
	Label             string
	MaxRegistrations  int
	UsedRegistrations int
	CreatedAt         time.Time
	ExpiresAt         *time.Time
	RevokedAt         *time.Time
	CreatedBy         string
}

// DynamicClientRegistration is the validated, canonical effective metadata
// supplied to the atomic DCR persistence boundary. CandidateClientSecret is
// plaintext only in memory; the Store persists its bcrypt digest and, when a
// keyring is configured, an authenticated envelope solely for exact replay.
type DynamicClientRegistration struct {
	TokenID                   string
	Fingerprint               string
	CandidateClientID         string
	ClientName                string
	RedirectURIsJSON          string
	AuthMethod                string
	ApplicationType           string
	CandidateClientSecret     string
	CandidateClientSecretHash string
	MaxClients                int
	ExpiresAt                 time.Time
	Now                       time.Time
}

type DynamicClientRegistrationResult struct {
	ClientID     string
	ClientSecret string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	Replayed     bool
}

// DCRAuditEvent is an immutable administrative or lifecycle record. Details
// contain only operator-supplied reasons and numeric/non-secret metadata.
type DCRAuditEvent struct {
	ID         int64
	Action     string
	Actor      string
	TokenID    string
	ClientID   string
	DetailJSON string
	OccurredAt time.Time
}

// AccountToken is a hashed, single-use email verification or password-reset
// capability. OAuth continuation fields are populated for email verification
// so the browser can resume the exact request after proving mailbox control.
type AccountToken struct {
	TokenHash           string
	UserID              string
	TenantID            string
	MembershipID        string
	LearnerID           string
	Purpose             string
	ClientID            string
	RedirectURI         string
	Resource            string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	ExpiresAt           time.Time
	CreatedAt           time.Time
	ConsumedAt          *time.Time
}

// LoginChallenge is a short-lived mailbox capability created only after a
// correct password is presented from an untrusted device while the account is
// under an elevated failure signal. The raw token is never persisted.
type LoginChallenge struct {
	TokenHash           string
	UserID              string
	TenantID            string
	MembershipID        string
	LearnerID           string
	ClientID            string
	RedirectURI         string
	Resource            string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	ExpiresAt           time.Time
	CreatedAt           time.Time
	ConsumedAt          *time.Time
	TrustedUntil        *time.Time
}
