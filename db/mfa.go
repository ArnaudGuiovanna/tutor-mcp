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
// encrypted at rest with authenticated associated data. The credential stays
// unusable until ConfirmTOTPEnrollment proves the authenticator holds it; a
// new ceremony discards any earlier unconfirmed seed.
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
	err = s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx, `DELETE FROM mfa_credentials
			WHERE user_id = ? AND kind = 'totp' AND confirmed_at IS NULL`, actor.UserID); err != nil {
			return err
		}
		result, err := txs.exec(ctx, `INSERT INTO mfa_credentials
			(id, user_id, kind, label, secret_ciphertext, key_id, credential_json, created_at)
			SELECT ?, id, 'totp', ?, ?, ?, '{}', ? FROM users
			WHERE id = ? AND status = 'active'`, id, strings.TrimSpace(label), ciphertext,
			integrationSecretKeyID(ciphertext), now, actor.UserID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return storeport.ErrInvalidPrincipal
		}
		return nil
	})
	if err != nil {
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

// VerifyTOTP checks a code for the membership's user and records the
// membership MFA verification, which bumps its version.
func (s *Store) VerifyTOTP(ctx context.Context, scope models.TenantScope, code string, at time.Time) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, fmt.Errorf("verify TOTP: invalid input")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var version int64
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if err := txs.consumeTOTP(txCtx, scope.UserID, code, at); err != nil {
			return err
		}
		var err error
		version, err = txs.RecordMembershipMFAVerification(txCtx, scope, at.UTC())
		return err
	})
	return version, err
}

// ConfirmTOTPEnrollment activates a pending credential once the authenticator
// proves it holds the seed. Earlier authenticators are revoked and a fresh set
// of recovery codes replaces any unused ones; the raw codes are returned once.
func (s *Store) ConfirmTOTPEnrollment(ctx context.Context, actor models.Principal, credentialID, code string, at time.Time) ([]string, error) {
	userID := actor.UserID
	if actor.Validate() != nil || strings.TrimSpace(credentialID) == "" || !sixDigits(code) || s.secretKeyring == nil {
		return nil, fmt.Errorf("confirm TOTP enrollment: invalid input")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	codes := make([]string, mfaRecoveryCodeCount)
	for i := range codes {
		raw, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes[i] = raw
	}
	err := s.inTx(ctx, nil, func(txs *Store) error {
		var ciphertext string
		if err := txs.queryRow(ctx, `SELECT secret_ciphertext FROM mfa_credentials
			WHERE id = ? AND user_id = ? AND kind = 'totp'
			  AND confirmed_at IS NULL AND revoked_at IS NULL`, credentialID, userID).Scan(&ciphertext); err != nil {
			return fmt.Errorf("confirm TOTP enrollment: unknown enrollment")
		}
		secret, err := txs.secretKeyring.decryptWithAAD(ciphertext, mfaSecretAAD(userID, credentialID))
		if err != nil {
			return err
		}
		step, ok := totpMatchStep(secret, code, at)
		if !ok {
			return fmt.Errorf("confirm TOTP enrollment: invalid code")
		}
		now := at.UTC()
		if _, err := txs.exec(ctx, `UPDATE mfa_credentials SET revoked_at = ?
			WHERE user_id = ? AND kind = 'totp' AND id <> ? AND revoked_at IS NULL`, now, userID, credentialID); err != nil {
			return err
		}
		if _, err := txs.exec(ctx, `UPDATE mfa_credentials SET confirmed_at = ?, last_used_at = ?
			WHERE id = ? AND user_id = ?`, now, step, credentialID, userID); err != nil {
			return err
		}
		if _, err := txs.exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, raw := range codes {
			id, err := generateID()
			if err != nil {
				return err
			}
			if _, err := txs.exec(ctx, `INSERT INTO mfa_recovery_codes (id, user_id, code_hash, created_at)
				VALUES (?, ?, ?, ?)`, id, userID, recoveryCodeHash(userID, raw), now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// HasConfirmedTOTP reports whether the user completed the TOTP ceremony.
func (s *Store) HasConfirmedTOTP(ctx context.Context, scope models.TenantScope) (bool, error) {
	if err := scope.Validate(); err != nil {
		return false, fmt.Errorf("has confirmed TOTP: %w", storeport.ErrInvalidPrincipal)
	}
	userID := scope.UserID
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM mfa_credentials
		WHERE user_id = ? AND kind = 'totp' AND confirmed_at IS NOT NULL AND revoked_at IS NULL`,
		userID).Scan(&count); err != nil {
		return false, fmt.Errorf("has confirmed TOTP: %w", err)
	}
	return count > 0, nil
}

// VerifySecondFactor accepts a current TOTP code, at most once per time step,
// or an unused recovery code, which it consumes.
func (s *Store) VerifySecondFactor(ctx context.Context, scope models.TenantScope, code string, at time.Time) error {
	userID := scope.UserID
	if scope.Validate() != nil {
		return fmt.Errorf("verify second factor: invalid input")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	code = strings.TrimSpace(code)
	return s.inTx(ctx, nil, func(txs *Store) error {
		if sixDigits(code) {
			return txs.consumeTOTP(ctx, userID, code, at)
		}
		normalized := normalizeRecoveryCode(code)
		if len(normalized) != recoveryCodeLength {
			return fmt.Errorf("verify second factor: invalid code")
		}
		result, err := txs.exec(ctx, `UPDATE mfa_recovery_codes SET used_at = ?
			WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
			at.UTC(), userID, recoveryCodeHash(userID, normalized))
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("verify second factor: invalid code")
		}
		// A recovery code proves the account holder, not the authenticator.
		// Confirmed TOTP credentials stay active until the user re-enrolls.
		return nil
	})
}

// consumeTOTP matches code against the user's confirmed TOTP credentials and
// burns the time step so the same code cannot be replayed.
func (s *Store) consumeTOTP(ctx context.Context, userID, code string, at time.Time) error {
	if !sixDigits(code) || s.secretKeyring == nil {
		return fmt.Errorf("verify TOTP: invalid input")
	}
	rows, err := s.query(ctx, `SELECT id, secret_ciphertext, last_used_at
		FROM mfa_credentials WHERE user_id = ? AND kind = 'totp'
		  AND confirmed_at IS NOT NULL AND revoked_at IS NULL
		ORDER BY created_at`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	matchedID := ""
	var matchedStep time.Time
	for rows.Next() {
		var id, ciphertext string
		var lastUsed sqlNullTime
		if err := rows.Scan(&id, &ciphertext, &lastUsed); err != nil {
			return err
		}
		secret, err := s.secretKeyring.decryptWithAAD(ciphertext, mfaSecretAAD(userID, id))
		if err != nil {
			return err
		}
		step, ok := totpMatchStep(secret, code, at)
		// last_used_at holds the start of the last accepted step: only a code
		// from a later step is fresh.
		if ok && (!lastUsed.Valid || step.After(lastUsed.Time)) {
			matchedID, matchedStep = id, step
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
	result, err := s.exec(ctx, `UPDATE mfa_credentials SET last_used_at = ?
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL
		  AND (last_used_at IS NULL OR last_used_at < ?)`, matchedStep, matchedID,
		userID, matchedStep)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("verify TOTP: replayed code")
	}
	return nil
}

// totpMatchStep returns the start of the time step whose code equals code,
// searching one step either side of at for clock drift.
func totpMatchStep(secret, code string, at time.Time) (time.Time, bool) {
	var matched time.Time
	found := false
	for delta := -1; delta <= 1; delta++ {
		stepTime := at.Add(time.Duration(delta) * 30 * time.Second)
		candidate, err := totpCode(secret, stepTime)
		if err != nil {
			return time.Time{}, false
		}
		if hmac.Equal([]byte(candidate), []byte(code)) && !found {
			matched, found = time.Unix((stepTime.Unix()/30)*30, 0).UTC(), true
		}
	}
	return matched, found
}

func sixDigits(code string) bool {
	if len(code) != 6 {
		return false
	}
	_, err := strconv.Atoi(code)
	return err == nil && !strings.ContainsAny(code, "+-")
}

const (
	mfaRecoveryCodeCount = 10
	recoveryCodeLength   = 10
)

var recoveryCodeEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newRecoveryCode returns 50 random bits as ten lowercase base32 characters,
// displayed as two groups of five.
func newRecoveryCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	encoded := strings.ToLower(recoveryCodeEncoding.EncodeToString(raw))[:recoveryCodeLength]
	return encoded[:5] + "-" + encoded[5:], nil
}

func normalizeRecoveryCode(code string) string {
	code = strings.ToLower(code)
	code = strings.NewReplacer("-", "", " ", "").Replace(code)
	return code
}

func recoveryCodeHash(userID, code string) string {
	return opaqueTokenHash("mfa-recovery\x00" + userID + "\x00" + normalizeRecoveryCode(code))
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
