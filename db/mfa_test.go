// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	storeport "tutor-mcp/store"
)

func TestTOTPEnrollmentVerificationAndReplayGate(t *testing.T) {
	s := setupTestDB(t)
	keyring, err := NewIntegrationSecretKeyring(
		"mfa:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "mfa",
	)
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(keyring)
	owner := ownerPrincipal(t, s)
	ctx := context.Background()
	credentialID, secret, err := s.BeginTOTPEnrollment(ctx, owner, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if credentialID == "" || secret == "" {
		t.Fatal("TOTP seed was not returned once")
	}
	var stored string
	if err := s.queryRow(ctx, `SELECT secret_ciphertext FROM mfa_credentials WHERE id = ?`, credentialID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == secret || integrationSecretKeyID(stored) != "mfa" {
		t.Fatal("TOTP seed was not encrypted")
	}
	at := time.Date(2026, time.August, 12, 12, 0, 15, 0, time.UTC)
	code, err := totpCode(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyTOTP(ctx, owner.TenantScope(), "000000", at); err == nil {
		t.Fatal("invalid TOTP accepted")
	}
	version, err := s.VerifyTOTP(ctx, owner.TenantScope(), code, at)
	if err != nil {
		t.Fatal(err)
	}
	if version <= owner.TokenVersion {
		t.Fatalf("MFA verification version = %d, old=%d", version, owner.TokenVersion)
	}
	if _, err := s.VerifyTOTP(ctx, owner.TenantScope(), code, at); err == nil {
		t.Fatal("TOTP replay accepted")
	}
	if err := s.ValidatePrincipal(ctx, owner); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("pre-MFA token remained valid: %v", err)
	}
	owner.TokenVersion = version
	if err := s.ValidatePrincipal(ctx, owner); err != nil {
		t.Fatalf("post-MFA principal invalid: %v", err)
	}
	var verified time.Time
	if err := s.queryRow(ctx, `SELECT mfa_verified_at FROM tenant_memberships
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, owner.MembershipID).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if !verified.Equal(at) {
		t.Fatalf("verified_at = %v, want %v", verified, at)
	}
}
