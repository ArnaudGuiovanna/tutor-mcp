// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

// Package certification verifies independent assessment authorities. It has
// no signing key, network client or dependency on the host's OAuth identity.
package certification

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"tutor-mcp/assessment"
)

var ErrInvalid = errors.New("invalid assessment certification")

const MessagePrefix = "tutor-assessment-certification-v1\n"

// Authority is operator configuration. Only external-service certification is
// supported: possession of a key cannot attest that a human reviewed a response.
type Authority struct {
	KeyID       string `json:"key_id"`
	AuthorityID string `json:"authority_id"`
	TenantID    string `json:"tenant_id"`
	PublicKey   string `json:"public_key"`
}

type Claims struct {
	ID               string `json:"id"`
	Audience         string `json:"audience"`
	TenantID         string `json:"tenant_id"`
	AttemptID        string `json:"attempt_id"`
	ReviewID         string `json:"review_id"`
	MaterialHash     string `json:"material_hash"`
	ScoreHash        string `json:"score_hash"`
	Verdict          string `json:"verdict"`
	ExpectedRevision int    `json:"expected_revision"`
	IssuedAt         int64  `json:"issued_at"`
	ExpiresAt        int64  `json:"expires_at"`
}

type Envelope struct {
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type key struct {
	authority Authority
	public    ed25519.PublicKey
}

type Verifier struct {
	audience string
	keys     map[string]key
}

// Verified cannot be constructed with claims by callers outside this package.
type Verified struct {
	claims    Claims
	authority Authority
	hash      string
}

func (v Verified) Claims() Claims       { return v.claims }
func (v Verified) Authority() Authority { return v.authority }
func (v Verified) Hash() string         { return v.hash }

// New constructs a fixed key allow-list. Empty configuration disables trust.
// Removing a key prevents future certifications; it does not erase history.
func New(audience, configuration string) (*Verifier, error) {
	v := &Verifier{audience: audience, keys: make(map[string]key)}
	if configuration == "" {
		return v, nil
	}
	var authorities []Authority
	if audience == "" || assessment.DecodeDocument(configuration, &authorities) != nil || len(authorities) == 0 || len(authorities) > 100 {
		return nil, ErrInvalid
	}
	for _, authority := range authorities {
		public, err := base64.RawURLEncoding.Strict().DecodeString(authority.PublicKey)
		if !identifier(authority.KeyID) || !identifier(authority.AuthorityID) || !identifier(authority.TenantID) || err != nil || len(public) != ed25519.PublicKeySize {
			return nil, ErrInvalid
		}
		if _, exists := v.keys[authority.KeyID]; exists {
			return nil, ErrInvalid
		}
		v.keys[authority.KeyID] = key{authority: authority, public: append(ed25519.PublicKey(nil), public...)}
	}
	return v, nil
}

func (v *Verifier) Verify(raw string, now time.Time) (Verified, error) {
	var envelope Envelope
	if v == nil || assessment.DecodeDocument(raw, &envelope) != nil {
		return Verified{}, ErrInvalid
	}
	k, exists := v.keys[envelope.KeyID]
	if !exists {
		return Verified{}, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil {
		return Verified{}, ErrInvalid
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Signature)
	message := []byte(MessagePrefix + envelope.KeyID + "\n" + envelope.Payload)
	if err != nil || !ed25519.Verify(k.public, message, signature) {
		return Verified{}, ErrInvalid
	}
	var claims Claims
	if assessment.DecodeDocument(string(payload), &claims) != nil || !identifier(claims.ID) || !identifier(claims.AttemptID) || !identifier(claims.ReviewID) ||
		claims.TenantID != k.authority.TenantID || claims.Audience != v.audience || !digest(claims.MaterialHash) || !digest(claims.ScoreHash) ||
		(claims.Verdict != "accept" && claims.Verdict != "reject") || claims.ExpectedRevision < 0 || claims.ExpectedRevision > 1_000_000 ||
		claims.IssuedAt <= 0 || claims.IssuedAt > now.Unix() || claims.ExpiresAt <= now.Unix() || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > 900 {
		return Verified{}, ErrInvalid
	}
	sum := sha256.Sum256(message)
	return Verified{claims: claims, authority: k.authority, hash: hex.EncodeToString(sum[:])}, nil
}

func identifier(s string) bool {
	return s != "" && len(s) <= 255 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

func digest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == strings.ToLower(s)
}
