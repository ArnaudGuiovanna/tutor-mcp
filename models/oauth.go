// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

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
