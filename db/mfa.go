// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // TOTP interoperability requires HMAC-SHA1 by default (RFC 6238).
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func mfaSecretAAD(userID, credentialID string) []byte {
	return []byte("tutor-mcp\x00mfa_credentials\x00" + userID + "\x00" + credentialID)
}

// BeginTOTPEnrollment returns the seed exactly once. Only authenticated code
// running the self-service MFA ceremony should call this boundary; the seed is
// encrypted at rest with authenticated associated data.
func (s *Store) BeginTOTPEnrollment(ctx context.Context, actor models.Principal, label string) (string, string, error) {
	if actor.Validate() != nil || strings.TrimSpace(label) == "" {
		return "", "", fmt.Errorf("begin TOTP enrollment: %w", storeport.ErrInvalidPrincipal)
	}
	if s.secretKeyring == nil {
		return "", "", fmt.Errorf("begin TOTP enrollment: integration secret keyring is required")
	}
	id, err := generateID()
	if err != nil {
		return "", "", err
	}
	seed := make([]byte, 20)
	if _, err := rand.Read(seed); err != nil {
		return "", "", err
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(seed)
	ciphertext, err := s.secretKeyring.encryptWithAAD(secret, mfaSecretAAD(actor.UserID, id))
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	if _, err := s.exec(ctx, `INSERT INTO mfa_credentials
		(id, user_id, kind, label, secret_ciphertext, key_id, credential_json, created_at)
		SELECT ?, id, 'totp', ?, ?, ?, '{}', ? FROM users
		WHERE id = ? AND status = 'active'`, id, strings.TrimSpace(label), ciphertext,
		integrationSecretKeyID(ciphertext), now, actor.UserID); err != nil {
		return "", "", fmt.Errorf("begin TOTP enrollment: %w", err)
	}
	return id, secret, nil
}

func totpCode(secret string, at time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil || len(key) < 16 {
		return "", fmt.Errorf("invalid TOTP seed")
	}
	counter := uint64(at.UTC().Unix() / 30)
	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], counter)
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(payload[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	value := (uint32(digest[offset])&0x7f)<<24 |
		uint32(digest[offset+1])<<16 |
		uint32(digest[offset+2])<<8 |
		uint32(digest[offset+3])
	return fmt.Sprintf("%06d", value%1_000_000), nil
}

func (s *Store) VerifyTOTP(ctx context.Context, scope models.TenantScope, code string, at time.Time) (int64, error) {
	if err := scope.Validate(); err != nil || len(code) != 6 {
		return 0, fmt.Errorf("verify TOTP: invalid input")
	}
	if _, err := strconv.Atoi(code); err != nil || s.secretKeyring == nil {
		return 0, fmt.Errorf("verify TOTP: invalid input")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var version int64
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		rows, err := txs.query(txCtx, `SELECT id, secret_ciphertext, last_used_at
			FROM mfa_credentials WHERE user_id = ? AND kind = 'totp' AND revoked_at IS NULL
			ORDER BY created_at`, scope.UserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		matchedID := ""
		stepStart := time.Unix((at.Unix()/30)*30, 0).UTC()
		for rows.Next() {
			var id, ciphertext string
			var lastUsed sqlNullTime
			if err := rows.Scan(&id, &ciphertext, &lastUsed); err != nil {
				return err
			}
			if lastUsed.Valid && !lastUsed.Time.Before(stepStart) {
				continue
			}
			secret, err := txs.secretKeyring.decryptWithAAD(ciphertext, mfaSecretAAD(scope.UserID, id))
			if err != nil {
				return err
			}
			for delta := -1; delta <= 1; delta++ {
				candidate, err := totpCode(secret, at.Add(time.Duration(delta)*30*time.Second))
				if err != nil {
					return err
				}
				if hmac.Equal([]byte(candidate), []byte(code)) {
					matchedID = id
					break
				}
			}
			if matchedID != "" {
				break
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if matchedID == "" {
			return fmt.Errorf("verify TOTP: invalid or replayed code")
		}
		result, err := txs.exec(txCtx, `UPDATE mfa_credentials SET last_used_at = ?
			WHERE id = ? AND user_id = ? AND revoked_at IS NULL
			  AND (last_used_at IS NULL OR last_used_at < ?)`, at.UTC(), matchedID,
			scope.UserID, stepStart)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("verify TOTP: replayed code")
		}
		version, err = txs.RecordMembershipMFAVerification(txCtx, scope, at.UTC())
		return err
	})
	return version, err
}

// sqlNullTime keeps mfa.go independent of database/sql names in its public API.
type sqlNullTime struct {
	Time  time.Time
	Valid bool
}

func (n *sqlNullTime) Scan(value any) error {
	if value == nil {
		n.Valid = false
		return nil
	}
	switch typed := value.(type) {
	case time.Time:
		n.Time, n.Valid = typed, true
		return nil
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, typed)
		if err != nil {
			return err
		}
		n.Time, n.Valid = parsed, true
		return nil
	default:
		return errors.New("unsupported timestamp type")
	}
}
