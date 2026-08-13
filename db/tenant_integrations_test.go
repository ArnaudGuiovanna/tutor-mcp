// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestTenantIntegrationEncryptionAllowlistSigningAndIsolation(t *testing.T) {
	s := setupTestDB(t)
	keyring, err := NewIntegrationSecretKeyring(
		"webhooks:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "webhooks",
	)
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(keyring)
	if err := ConfigureTenantIntegrationAllowedHosts(s, []string{"hooks.customer.test"}); err != nil {
		t.Fatal(err)
	}
	owner := ownerPrincipal(t, s)
	ctx := context.Background()
	if _, _, err := s.CreateTenantIntegration(ctx, owner, "webhook", "https://127.0.0.1/hook", []string{"formation.version.published"}); err == nil {
		t.Fatal("IP integration endpoint accepted")
	}
	integration, secret, err := s.CreateTenantIntegration(ctx, owner, "webhook",
		"https://hooks.customer.test/events", []string{"formation.version.published"})
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext string
	if err := s.queryRow(ctx, `SELECT secret_ciphertext FROM tenant_integrations
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, integration.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == secret || integrationSecretKeyID(ciphertext) != "webhooks" {
		t.Fatal("tenant integration secret stored in plaintext")
	}
	now := time.Date(2026, time.August, 12, 13, 0, 0, 0, time.UTC)
	payload := []byte(`{"event_id":"event-1"}`)
	signed, err := s.PrepareTenantWebhook(ctx, owner.TenantScope(), integration.ID,
		"event-1", "formation.version.published", payload, now)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(now.Unix(), 10) + ".event-1."))
	_, _ = mac.Write(payload)
	if signed.Signature != "v1="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature = %q", signed.Signature)
	}
	foreignScope := models.TenantScope{
		TenantID: "tenant_foreign", UserID: "worker", MembershipID: "worker_foreign",
	}
	if _, err := s.PrepareTenantWebhook(ctx, foreignScope, integration.ID,
		"event-1", "formation.version.published", payload, now); err == nil {
		t.Fatal("foreign tenant prepared integration webhook")
	}
}

func TestTenantIntegrationSecretRotationRetainsBoundedOverlap(t *testing.T) {
	s := setupTestDB(t)
	keyring, err := NewIntegrationSecretKeyring(
		"webhooks:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "webhooks",
	)
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(keyring)
	if err := ConfigureTenantIntegrationAllowedHosts(s, []string{"hooks.customer.test"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	integration, oldSecret, err := s.CreateTenantIntegration(ctx, owner, "webhook",
		"https://hooks.customer.test/events", []string{"formation.version.published"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RotateTenantIntegrationSecret(ctx, owner, integration.ID, time.Minute); err == nil {
		t.Fatal("unsafe short rotation overlap accepted")
	}
	before := time.Now().UTC()
	rotated, newSecret, err := s.RotateTenantIntegrationSecret(ctx, owner, integration.ID, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Version != 2 || newSecret == "" || newSecret == oldSecret {
		t.Fatalf("rotation metadata=%#v secret changed=%v", rotated, newSecret != oldSecret)
	}
	versions, err := s.ListTenantIntegrationSecretVersions(ctx, owner, integration.ID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("secret history len=%d err=%v", len(versions), err)
	}
	if versions[0].Version != 2 || versions[0].ValidUntil != nil || versions[1].Version != 1 || versions[1].ValidUntil == nil {
		t.Fatalf("secret history = %#v", versions)
	}
	if versions[1].ValidUntil.Before(before.Add(23*time.Hour)) || versions[1].ValidUntil.After(before.Add(25*time.Hour)) {
		t.Fatalf("old secret overlap expires at %v", versions[1].ValidUntil)
	}
	var oldCiphertext, newCiphertext string
	if err := s.queryRow(ctx, `SELECT secret_ciphertext FROM tenant_integration_secret_versions
		WHERE tenant_id = ? AND integration_id = ? AND secret_version = 1`, owner.TenantID, integration.ID).Scan(&oldCiphertext); err != nil {
		t.Fatal(err)
	}
	if err := s.queryRow(ctx, `SELECT secret_ciphertext FROM tenant_integrations
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, integration.ID).Scan(&newCiphertext); err != nil {
		t.Fatal(err)
	}
	if oldCiphertext == oldSecret || newCiphertext == newSecret {
		t.Fatal("integration secret persisted in plaintext")
	}
	decodedOld, err := keyring.decryptWithAAD(oldCiphertext, tenantIntegrationAAD(owner.TenantID, integration.ID, 1))
	if err != nil || decodedOld != oldSecret {
		t.Fatalf("retained old secret decode=%q err=%v", decodedOld, err)
	}
	now := time.Now().UTC()
	payload := []byte(`{"event_id":"event-after-rotation"}`)
	signed, err := s.PrepareTenantWebhook(ctx, owner.TenantScope(), integration.ID,
		"event-after-rotation", "formation.version.published", payload, now)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(newSecret))
	_, _ = mac.Write([]byte(strconv.FormatInt(now.Unix(), 10) + ".event-after-rotation."))
	_, _ = mac.Write(payload)
	if signed.SecretVersion != 2 || signed.Signature != "v1="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("rotated signature version=%d signature=%q", signed.SecretVersion, signed.Signature)
	}
}
