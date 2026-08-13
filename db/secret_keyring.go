// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"tutor-mcp/webhookurl"
)

const integrationSecretPrefix = "enc:v1:"

var secretKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// IntegrationSecretKeyring provides versioned application-layer encryption
// for durable integration credentials. Keep all still-referenced keys during
// rotation and select the write key with currentID.
type IntegrationSecretKeyring struct {
	currentID string
	keys      map[string][]byte
}

// NewIntegrationSecretKeyring parses comma-separated keyID:base64-32-byte-key
// entries. Key material should be injected by a secret manager, never stored
// in the database or logs.
func NewIntegrationSecretKeyring(encodedKeys, currentID string) (*IntegrationSecretKeyring, error) {
	currentID = strings.TrimSpace(currentID)
	if !secretKeyIDPattern.MatchString(currentID) {
		return nil, fmt.Errorf("invalid integration secret current key ID")
	}
	keyring := &IntegrationSecretKeyring{currentID: currentID, keys: make(map[string][]byte)}
	for _, entry := range strings.Split(encodedKeys, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 2)
		if len(parts) != 2 || !secretKeyIDPattern.MatchString(parts[0]) {
			return nil, fmt.Errorf("invalid integration secret key entry")
		}
		if _, duplicate := keyring.keys[parts[0]]; duplicate {
			return nil, fmt.Errorf("duplicate integration secret key ID %q", parts[0])
		}
		key, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			key, err = base64.RawStdEncoding.DecodeString(parts[1])
		}
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("integration secret key %q must be 32 base64-encoded bytes", parts[0])
		}
		keyring.keys[parts[0]] = append([]byte(nil), key...)
	}
	if _, ok := keyring.keys[currentID]; !ok {
		return nil, fmt.Errorf("integration secret current key ID is not present")
	}
	return keyring, nil
}

func integrationSecretAAD(learnerID string) []byte {
	return []byte("tutor-mcp\x00learners\x00" + learnerID + "\x00webhook_url")
}

func (k *IntegrationSecretKeyring) encrypt(learnerID, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	return k.encryptWithAAD(plaintext, integrationSecretAAD(learnerID))
}

func (k *IntegrationSecretKeyring) encryptWithAAD(plaintext string, aad []byte) (string, error) {
	block, err := aes.NewCipher(k.keys[k.currentID])
	if err != nil {
		return "", fmt.Errorf("initialize integration secret cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("initialize integration secret AEAD: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate integration secret nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), aad)
	return integrationSecretPrefix + k.currentID + ":" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (k *IntegrationSecretKeyring) decrypt(learnerID, envelope string) (string, error) {
	return k.decryptWithAAD(envelope, integrationSecretAAD(learnerID))
}

func (k *IntegrationSecretKeyring) decryptWithAAD(envelope string, aad []byte) (string, error) {
	if envelope == "" {
		return "", nil
	}
	if !strings.HasPrefix(envelope, integrationSecretPrefix) {
		return "", fmt.Errorf("integration secret is not encrypted")
	}
	parts := strings.SplitN(strings.TrimPrefix(envelope, integrationSecretPrefix), ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("malformed integration secret envelope")
	}
	key, ok := k.keys[parts[0]]
	if !ok {
		return "", fmt.Errorf("integration secret key ID is unavailable")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("malformed integration secret ciphertext")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("initialize integration secret cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("initialize integration secret AEAD: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return "", fmt.Errorf("malformed integration secret ciphertext")
	}
	plaintext, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad)
	if err != nil {
		return "", fmt.Errorf("decrypt integration secret: authentication failed")
	}
	return string(plaintext), nil
}

func integrationSecretKeyID(envelope string) string {
	if !strings.HasPrefix(envelope, integrationSecretPrefix) {
		return ""
	}
	parts := strings.SplitN(strings.TrimPrefix(envelope, integrationSecretPrefix), ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[0]
}

func (s *Store) SetIntegrationSecretKeyring(keyring *IntegrationSecretKeyring) {
	s.secretKeyring = keyring
}

func (s *Store) encryptIntegrationSecret(learnerID, plaintext string) (string, error) {
	if plaintext == "" || s.secretKeyring == nil {
		return plaintext, nil
	}
	return s.secretKeyring.encrypt(learnerID, plaintext)
}

func (s *Store) decryptIntegrationSecret(learnerID, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if s.secretKeyring == nil {
		if strings.HasPrefix(stored, integrationSecretPrefix) {
			return "", fmt.Errorf("integration secret keyring is not configured")
		}
		return stored, nil
	}
	return s.secretKeyring.decrypt(learnerID, stored)
}

// RotateIntegrationSecrets upgrades plaintext legacy webhook rows and DCR
// replay-secret envelopes made with older retained keys to the current key.
// All values are authenticated before the write transaction, so one bad row
// leaves every integration-secret family intact.
func (s *Store) RotateIntegrationSecrets(ctx context.Context) (int64, error) {
	if s.secretKeyring == nil {
		return 0, fmt.Errorf("integration secret keyring is not configured")
	}
	rows, err := s.query(ctx, `SELECT id, webhook_url FROM learners WHERE webhook_url != ''`)
	if err != nil {
		return 0, fmt.Errorf("list integration secrets: %w", err)
	}
	type rotation struct{ learnerID, ciphertext string }
	var rotations []rotation
	for rows.Next() {
		var learnerID, stored string
		if err := rows.Scan(&learnerID, &stored); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan integration secret: %w", err)
		}
		plaintext := stored
		if strings.HasPrefix(stored, integrationSecretPrefix) {
			plaintext, err = s.secretKeyring.decrypt(learnerID, stored)
			if err != nil {
				_ = rows.Close()
				return 0, err
			}
		}
		if !webhookurl.IsSafeWebhookURL(plaintext) {
			_ = rows.Close()
			return 0, fmt.Errorf("integration secret is invalid")
		}
		// Authenticate current-key envelopes too. Skipping before decrypt would
		// let a tampered GCM tag survive startup and fail only during delivery.
		if integrationSecretKeyID(stored) == s.secretKeyring.currentID {
			continue
		}
		ciphertext, err := s.secretKeyring.encrypt(learnerID, plaintext)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		rotations = append(rotations, rotation{learnerID: learnerID, ciphertext: ciphertext})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate integration secrets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close integration secret rows: %w", err)
	}

	type dcrRotation struct {
		clientID, fingerprint, previous, ciphertext string
	}
	var dcrRotations []dcrRotation
	dcrRows, err := s.query(ctx,
		`SELECT client_id, registration_fingerprint, registration_secret_ciphertext
		 FROM oauth_clients WHERE registration_secret_ciphertext <> ''`,
	)
	if err != nil {
		return 0, fmt.Errorf("list DCR replay secrets: %w", err)
	}
	for dcrRows.Next() {
		var clientID, fingerprint, stored string
		if err := dcrRows.Scan(&clientID, &fingerprint, &stored); err != nil {
			_ = dcrRows.Close()
			return 0, fmt.Errorf("scan DCR replay secret: %w", err)
		}
		plaintext, err := s.secretKeyring.decryptWithAAD(stored, dcrRegistrationSecretAAD(clientID, fingerprint))
		if err != nil || plaintext == "" {
			_ = dcrRows.Close()
			return 0, fmt.Errorf("authenticate DCR replay secret")
		}
		if integrationSecretKeyID(stored) == s.secretKeyring.currentID {
			continue
		}
		ciphertext, err := s.secretKeyring.encryptWithAAD(plaintext, dcrRegistrationSecretAAD(clientID, fingerprint))
		if err != nil {
			_ = dcrRows.Close()
			return 0, fmt.Errorf("encrypt DCR replay secret: %w", err)
		}
		dcrRotations = append(dcrRotations, dcrRotation{
			clientID: clientID, fingerprint: fingerprint, previous: stored, ciphertext: ciphertext,
		})
	}
	if err := dcrRows.Err(); err != nil {
		_ = dcrRows.Close()
		return 0, fmt.Errorf("iterate DCR replay secrets: %w", err)
	}
	if err := dcrRows.Close(); err != nil {
		return 0, fmt.Errorf("close DCR replay secret rows: %w", err)
	}
	if len(rotations) == 0 && len(dcrRotations) == 0 {
		return 0, nil
	}
	err = s.inTx(ctx, nil, func(txs *Store) error {
		for _, item := range rotations {
			if _, err := txs.exec(ctx, `UPDATE learners SET webhook_url = ? WHERE id = ?`, item.ciphertext, item.learnerID); err != nil {
				return fmt.Errorf("rotate integration secret: %w", err)
			}
		}
		for _, item := range dcrRotations {
			result, err := txs.exec(ctx,
				`UPDATE oauth_clients SET registration_secret_ciphertext = ?
				 WHERE client_id = ? AND registration_fingerprint = ?
				   AND registration_secret_ciphertext = ?`,
				item.ciphertext, item.clientID, item.fingerprint, item.previous,
			)
			if err != nil {
				return fmt.Errorf("rotate DCR replay secret: %w", err)
			}
			if rows, _ := result.RowsAffected(); rows != 1 {
				return fmt.Errorf("rotate DCR replay secret: concurrent modification")
			}
		}
		return nil
	})
	return int64(len(rotations) + len(dcrRotations)), err
}
