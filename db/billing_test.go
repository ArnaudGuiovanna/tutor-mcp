// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestBillingWebhookSignatureDedupConflictAndGrace(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	operator := models.ControlPlanePrincipal{
		ActorID: "platform", Roles: []string{models.RolePlatformAdmin}, Reason: "billing test", RequestID: "billing-test-setup",
	}
	tenant, err := s.ProvisionTenant(ctx, operator, "billing-tenant", "Billing Tenant", "eu", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	credential := models.BillingWebhookCredential{Provider: "testpay", Secret: "0123456789abcdef0123456789abcdef"}
	now := time.Date(2026, time.August, 12, 14, 0, 0, 0, time.UTC)
	payload := []byte(fmt.Sprintf(`{"id":"evt-1","tenant_id":%q,"type":"subscription.past_due","occurred_at":%q}`,
		tenant.ID, now.Format(time.RFC3339)))
	sign := func(body []byte) string {
		mac := hmac.New(sha256.New, []byte(credential.Secret))
		_, _ = mac.Write(body)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	if processed, err := s.ApplyBillingProviderWebhook(ctx, credential, "sha256=00", payload); err == nil || processed {
		t.Fatalf("bad signature processed=%v err=%v", processed, err)
	}
	if processed, err := s.ApplyBillingProviderWebhook(ctx, credential, sign(payload), payload); err != nil || !processed {
		t.Fatalf("first event processed=%v err=%v", processed, err)
	}
	if processed, err := s.ApplyBillingProviderWebhook(ctx, credential, sign(payload), payload); err != nil || processed {
		t.Fatalf("replay processed=%v err=%v", processed, err)
	}
	conflict := []byte(fmt.Sprintf(`{"id":"evt-1","tenant_id":%q,"type":"subscription.cancelled","occurred_at":%q}`,
		tenant.ID, now.Format(time.RFC3339)))
	if _, err := s.ApplyBillingProviderWebhook(ctx, credential, sign(conflict), conflict); err == nil {
		t.Fatal("provider event id conflict accepted")
	}
	var status string
	var graceUntil time.Time
	if err := s.queryRow(ctx, `SELECT status, grace_until FROM tenant_subscriptions
		WHERE tenant_id = ?`, tenant.ID).Scan(&status, &graceUntil); err != nil {
		t.Fatal(err)
	}
	if status != "grace" || !graceUntil.After(time.Now().UTC().Add(6*24*time.Hour)) {
		t.Fatalf("subscription status/grace = %q/%v", status, graceUntil)
	}
	if _, err := s.exec(ctx, `DELETE FROM billing_provider_events WHERE provider = 'testpay'`); err == nil {
		t.Fatal("billing source event was mutable")
	}
}
