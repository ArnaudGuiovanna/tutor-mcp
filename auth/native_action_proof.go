// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"tutor-mcp/models"
)

const NativeActionProofHeader = "X-Tutor-Action-Proof"

// VerifyNativeActionProof binds host account operations to the exact HTTP
// request. The MCP agent receives a bearer, but never the user's signing key.
// The account store must consume EventID once after matching its live identity.
func VerifyNativeActionProof(header, url, tenantID, sessionID string, body []byte, now time.Time) (models.NativeActionProof, error) {
	var proof models.NativeActionProof
	if tenantID == "" || len(tenantID) > 255 || !nativeID(sessionID) || len(header) > 16*1024 || strings.ContainsAny(header, "\r\n\t") {
		return proof, errors.New("invalid action proof")
	}
	encoded := header
	if prefix, value, ok := strings.Cut(header, " "); ok && strings.EqualFold(prefix, "Nostr") {
		encoded = value
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return proof, errors.New("invalid action proof")
	}
	var event nativeNostrEvent
	if err = decodeNativeJSON(raw, &event); err != nil {
		return proof, err
	}
	nonce := ""
	for _, tag := range event.Tags {
		if len(tag) == 2 && tag[0] == "nonce" {
			nonce = tag[1]
		}
	}
	if len(nonce) < 16 || len(nonce) > 128 {
		return proof, errors.New("invalid action nonce")
	}
	for _, ch := range nonce {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return proof, errors.New("invalid action nonce")
		}
	}
	hash := sha256.Sum256(body)
	payload := hex.EncodeToString(hash[:])
	if err = verifyNativeNostrWindow(header, false, event.PublicKey, map[string]string{"u": url, "method": "POST", "payload": payload, "nonce": nonce, "tenant": tenantID, "session": sessionID}, now, 60, 60); err != nil {
		return proof, err
	}
	proof = models.NativeActionProof{EventID: event.ID, PublicKey: event.PublicKey, PayloadHash: payload, TenantID: tenantID, SessionID: sessionID, CreatedAt: time.Unix(event.CreatedAt, 0).UTC(), ExpiresAt: time.Unix(event.CreatedAt, 0).UTC().Add(time.Minute)}
	return proof, nil
}
