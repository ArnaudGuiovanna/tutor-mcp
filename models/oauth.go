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
	LearnerID           string
	CodeChallenge       string
	CodeChallengeMethod string
	ClientID            string
	RedirectURI         string
	Resource            string
	Scope               string
	ExpiresAt           time.Time
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

// AccountToken is a hashed, single-use email verification or password-reset
// capability. OAuth continuation fields are populated for email verification
// so the browser can resume the exact request after proving mailbox control.
type AccountToken struct {
	TokenHash           string
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
