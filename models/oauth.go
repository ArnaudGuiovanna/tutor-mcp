// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

// AuthCode holds the authorization code state (persisted in DB).
// Moved from package db so the store port can return it without consumers importing db.
type AuthCode struct {
	Code          string
	LearnerID     string
	CodeChallenge string
	ClientID      string
	ExpiresAt     time.Time
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
}
