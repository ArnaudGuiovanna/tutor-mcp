// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestFormationPublishAndOutboxAreAtomic(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	_, version, err := s.CreateFormationDraft(ctx, owner, "Atomic outbox", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationModule(ctx, owner, version.ID, models.FormationModuleInput{StableKey: "m", Title: "M"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationConcept(ctx, owner, version.ID, models.FormationConceptInput{ModuleStableKey: "m", StableKey: "c", Label: "C"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, version.ID); err != nil {
		t.Fatal(err)
	}
	var eventType string
	if err := s.queryRow(ctx, `SELECT event_type FROM outbox_events
		WHERE tenant_id = ? AND aggregate_id = ?`, owner.TenantID, version.ID).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
	if eventType != "formation.version.published" {
		t.Fatalf("event type = %q", eventType)
	}

	_, failedVersion, err := s.CreateFormationDraft(ctx, owner, "Rollback outbox", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationModule(ctx, owner, failedVersion.ID, models.FormationModuleInput{StableKey: "m", Title: "M"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationConcept(ctx, owner, failedVersion.ID, models.FormationConceptInput{ModuleStableKey: "m", StableKey: "c", Label: "C"}); err != nil {
		t.Fatal(err)
	}
	if s.dialect == DialectSQLite {
		if _, err := s.root.Exec(`CREATE TRIGGER reject_test_outbox BEFORE INSERT ON outbox_events
			WHEN NEW.aggregate_id = '` + failedVersion.ID + `'
			BEGIN SELECT RAISE(ABORT, 'test outbox failure'); END`); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := s.root.Exec(`CREATE OR REPLACE FUNCTION reject_test_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.aggregate_id = '` + failedVersion.ID + `' THEN RAISE EXCEPTION 'test outbox failure'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER reject_test_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION reject_test_outbox()`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PublishFormationVersion(ctx, owner, failedVersion.ID); err == nil {
		t.Fatal("publish succeeded despite rejected outbox")
	}
	var status string
	if err := s.queryRow(ctx, `SELECT status FROM formation_versions
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, failedVersion.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "draft" {
		t.Fatalf("formation status after outbox failure = %q, want draft", status)
	}
}

func TestOutboxLeaseRecoveryAndDeadLetter(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	principal := ownerPrincipal(t, s)
	scope := principal.TenantScope()
	now := time.Now().UTC()
	if err := s.AppendOutboxEvent(ctx, scope, models.OutboxEvent{
		TenantID: scope.TenantID, AggregateType: "test", AggregateID: "aggregate",
		EventType: "test.created", IdempotencyKey: "test-event", PayloadJSON: `{}`,
		CreatedAt: now, AvailableAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimOutboxEvent(ctx, scope, "worker-a", now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimOutboxEvent(ctx, scope, "worker-b", now, time.Second); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("concurrent claim = %v, want no rows", err)
	}
	recovered, err := s.ClaimOutboxEvent(ctx, scope, "worker-b", now.Add(2*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || recovered.AttemptCount != 2 {
		t.Fatalf("recovered event = %#v", recovered)
	}
	if err := s.FinishOutboxEvent(ctx, scope, first.ID, "worker-a", true, time.Time{}, ""); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker finish = %v, want lease lost", err)
	}
	if err := s.FinishOutboxEvent(ctx, scope, first.ID, "worker-b", true, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimOutboxEvent(ctx, scope, "worker-c", now.Add(time.Hour), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delivered event reclaimed: %v", err)
	}
}

func TestAsyncJobHeartbeatRecoveryAndPoisonDLQ(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	scope := ownerPrincipal(t, s).TenantScope()
	now := time.Now().UTC()
	if err := s.EnqueueAsyncJob(ctx, scope, models.AsyncJob{
		TenantID: scope.TenantID, Kind: "export", IdempotencyKey: "export-1",
		PayloadJSON: `{}`, MaxAttempts: 2, CreatedAt: now, AvailableAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueAsyncJob(ctx, scope, models.AsyncJob{
		TenantID: scope.TenantID, Kind: "export", IdempotencyKey: "export-1",
		PayloadJSON: `{}`, MaxAttempts: 2, CreatedAt: now, AvailableAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimAsyncJob(ctx, scope, "worker-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HeartbeatAsyncJob(ctx, scope, job.ID, "worker-a", now.Add(30*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAsyncJob(ctx, scope, "worker-b", now.Add(61*time.Second), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("heartbeat lease ignored: %v", err)
	}
	recovered, err := s.ClaimAsyncJob(ctx, scope, "worker-b", now.Add(91*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != job.ID || recovered.AttemptCount != 2 {
		t.Fatalf("recovered job = %#v", recovered)
	}
	if err := s.FinishAsyncJob(ctx, scope, job.ID, "worker-b", false, now.Add(2*time.Minute), "poison"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.queryRow(ctx, `SELECT status FROM async_jobs WHERE tenant_id = ? AND id = ?`,
		scope.TenantID, job.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "dead_letter" {
		t.Fatalf("poison job status = %q, want dead_letter", status)
	}
}

func TestUsageIsIdempotentAndQuotaBounded(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	now := time.Now().UTC()
	if err := s.ConfigureEntitlement(ctx, owner, "mcp_calls_month", 2, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	event := models.UsageEvent{
		TenantID: owner.TenantID, EventKey: "request-1", Metric: "mcp_calls_month",
		Quantity: 1, SourceType: "mcp_call", SourceID: "request-1",
		DimensionsJSON: `{}`, OccurredAt: now,
	}
	if inserted, err := s.RecordUsage(ctx, owner.TenantScope(), event); err != nil || !inserted {
		t.Fatalf("first usage inserted=%v err=%v", inserted, err)
	}
	if inserted, err := s.RecordUsage(ctx, owner.TenantScope(), event); err != nil || inserted {
		t.Fatalf("replay usage inserted=%v err=%v", inserted, err)
	}
	event.EventKey = "request-2"
	event.SourceID = "request-2"
	event.Quantity = 2
	if inserted, err := s.RecordUsage(ctx, owner.TenantScope(), event); !errors.Is(err, ErrQuotaExceeded) || inserted {
		t.Fatalf("quota usage inserted=%v err=%v", inserted, err)
	}
	var used, events int64
	if err := s.queryRow(ctx, `SELECT used_value FROM tenant_entitlements
		WHERE tenant_id = ? AND entitlement_key = 'mcp_calls_month'`, owner.TenantID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM usage_events WHERE tenant_id = ?`, owner.TenantID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if used != 1 || events != 1 {
		t.Fatalf("used/events = %d/%d, want 1/1", used, events)
	}
	quantity, err := s.ReconcileUsageRollup(ctx, owner.TenantScope(), "mcp_calls_month",
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || quantity != 1 {
		t.Fatalf("rollup quantity=%d err=%v", quantity, err)
	}
	if _, err := s.exec(ctx, `UPDATE usage_events SET quantity = 99 WHERE tenant_id = ?`, owner.TenantID); err == nil {
		t.Fatal("usage source was mutable")
	}
}

func TestPostgresRollingWriterLearningScopeTriggers(t *testing.T) {
	s := setupTestDB(t)
	if s.dialect != DialectPostgres {
		t.Skip("PostgreSQL rolling-writer trigger gate")
	}
	ctx := context.Background()
	var triggerCount int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM pg_trigger
		WHERE NOT tgisinternal AND tgname IN
		('zz_fill_learning_scope','zz_concept_states_fill_learning_scope',
		 'zz_interactions_fill_learning_scope','zz_assessment_attempts_fill_learning_scope',
		 'zz_learning_sessions_fill_learning_scope','zz_affect_states_fill_learning_scope')`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 11 {
		t.Fatalf("learning-scope trigger count = %d, want 11", triggerCount)
	}
	var tenantID, enrollmentID string
	if err := s.queryRow(ctx, `SELECT tenant_id, enrollment_id FROM legacy_domain_enrollments
		WHERE learner_id = 'L1' AND domain_id = ''`).Scan(&tenantID, &enrollmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `INSERT INTO implementation_intentions
		(learner_id, domain_id, trigger_text, action_text, created_at)
		VALUES ('L1', '', 'trigger gate', 'action', ?)`, time.Now().UTC()); err != nil {
		t.Fatalf("rolling writer trigger: tenant=%q enrollment=%q: %v", tenantID, enrollmentID, err)
	}
}
