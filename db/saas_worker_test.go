// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestSaaSOutboxRelayAndDeliveryRecovery(t *testing.T) {
	s := setupTestDB(t)
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "webhooks:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "webhooks"))
	if err := ConfigureTenantIntegrationAllowedHosts(s, []string{"hooks.customer.test"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	integration, _, err := s.CreateTenantIntegration(ctx, owner, "webhook",
		"https://hooks.customer.test/events", []string{"formation.version.published"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.AppendOutboxEvent(ctx, owner.TenantScope(), models.OutboxEvent{
		TenantID: owner.TenantID, AggregateType: "formation_version", AggregateID: "v1",
		EventType: "formation.version.published", IdempotencyKey: "publish-v1",
		PayloadJSON: `{"version_id":"v1"}`, CreatedAt: now, AvailableAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.RelayOutboxEvent(ctx, owner.TenantScope(), "relay-a", now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("relay claimed=%v err=%v", claimed, err)
	}
	job, err := s.ClaimAsyncJob(ctx, owner.TenantScope(), "worker-a", now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var payload models.TenantWebhookJob
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.IntegrationID != integration.ID || payload.EventID == "" || payload.EventType != "formation.version.published" {
		t.Fatalf("relayed payload = %#v", payload)
	}
	first, err := s.BeginIntegrationDelivery(ctx, owner.TenantScope(), payload, job.AttemptCount, now)
	if err != nil {
		t.Fatal(err)
	}
	code := 204
	if err := s.FinishIntegrationDelivery(ctx, owner.TenantScope(), first.ID, "delivered", &code, "response-hash", ""); err != nil {
		t.Fatal(err)
	}
	// A recovered job must see the prior durable success and skip a duplicate
	// network request even though its lease attempt increased.
	recovered, err := s.BeginIntegrationDelivery(ctx, owner.TenantScope(), payload, job.AttemptCount+1, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || recovered.Status != "delivered" {
		t.Fatalf("recovered delivery = %#v", recovered)
	}
	deliveries, next, err := s.ListIntegrationDeliveries(ctx, owner, integration.ID, "", 10)
	if err != nil || next != "" || len(deliveries) != 1 {
		t.Fatalf("delivery history len=%d next=%q err=%v", len(deliveries), next, err)
	}
	if claimed, err := s.RelayOutboxEvent(ctx, owner.TenantScope(), "relay-b", now.Add(time.Hour), time.Minute); err != nil || claimed {
		t.Fatalf("delivered outbox reclaimed=%v err=%v", claimed, err)
	}
	if _, err := s.ClaimAsyncJob(ctx, owner.TenantScope(), "worker-b", now.Add(time.Second), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("leased async job concurrently: %v", err)
	}
}
