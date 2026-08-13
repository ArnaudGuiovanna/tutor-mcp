// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"

	"go.opentelemetry.io/otel/trace"
)

func TestTenantAuditRetentionDSARAndRestoreVerification(t *testing.T) {
	s := setupTestDB(t)
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID: trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	owner := ownerPrincipal(t, s)
	if err := s.SetTenantRetentionPolicy(ctx, owner, models.TenantRetentionPolicy{
		DataClass: "audit_events", RetentionDays: 365, LegalHold: true,
	}); err != nil {
		t.Fatal(err)
	}
	policies, err := s.ListTenantRetentionPolicies(ctx, owner)
	if err != nil || len(policies) != 1 || policies[0].DataClass != "audit_events" || !policies[0].LegalHold {
		t.Fatalf("retention policies=%#v err=%v", policies, err)
	}
	dsar, err := s.RequestTenantDSAR(ctx, owner, owner.LearnerID, "export", "learner access request")
	if err != nil {
		t.Fatal(err)
	}
	worker := models.WorkerPrincipal{ActorID: "dsar-export-worker"}
	workerScope := owner.TenantScope()
	workerScope.UserID = "worker_" + worker.ActorID
	workerScope.MembershipID = "worker_process"
	if err := s.CompleteTenantDSARExport(ctx, workerScope, worker, dsar.ID); err != nil {
		t.Fatal(err)
	}
	var dsarStatus, resultJSON string
	if err := s.queryRow(ctx, `SELECT status, result_json FROM tenant_dsar_requests
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, dsar.ID).Scan(&dsarStatus, &resultJSON); err != nil {
		t.Fatal(err)
	}
	if dsarStatus != "completed" || !canonicalJSONEqual(resultJSON, resultJSON) || resultJSON == "{}" {
		t.Fatalf("DSAR status=%q result=%q", dsarStatus, resultJSON)
	}
	page, err := s.ListAuditEvents(ctx, owner, models.AuditEventFilter{ActionPrefix: "dsar.", Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].TargetID != owner.LearnerID ||
		page.Items[0].Result != "success" || page.Items[0].TraceID != spanContext.TraceID().String() {
		t.Fatalf("audit page=%#v err=%v", page, err)
	}
	if _, err := s.exec(ctx, `UPDATE audit_events SET reason = 'tampered' WHERE tenant_id = ?`, owner.TenantID); err == nil {
		t.Fatal("audit event was mutable")
	}

	operator := models.ControlPlanePrincipal{ActorID: "restore-operator", Roles: []string{models.RolePlatformAdmin},
		Reason: "restore exercise", RequestID: "restore-request-1"}
	tablesJSON, objectsJSON, err := s.ComputeTenantChecksums(ctx, operator, owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := s.RequestTenantRestoreVerification(ctx, operator, owner.TenantID,
		"backup-2026-08-12", tablesJSON, objectsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := s.VerifyTenantRestore(ctx, operator, owner.TenantID, manifest.ID); err != nil || !matched {
		t.Fatalf("restore matched=%v err=%v", matched, err)
	}
	bad, err := s.RequestTenantRestoreVerification(ctx, operator, owner.TenantID,
		"backup-corrupt", `{"formations":"wrong"}`, objectsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := s.VerifyTenantRestore(ctx, operator, owner.TenantID, bad.ID); err != nil || matched {
		t.Fatalf("corrupt restore matched=%v err=%v", matched, err)
	}
	var status string
	var verifiedAt time.Time
	if err := s.queryRow(ctx, `SELECT status, verified_at FROM tenant_restore_manifests
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, bad.ID).Scan(&status, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || verifiedAt.IsZero() {
		t.Fatalf("corrupt manifest status=%q verified_at=%v", status, verifiedAt)
	}
}

func TestTenantDSARErasureIsBoundedBlockedByHoldAndResumable(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	now := time.Now().UTC()
	if err := s.CreateRetentionLegalHold(ctx, RetentionLegalHold{HoldID: "dsar-hold", LearnerID: owner.LearnerID,
		Reason: "pending litigation", CreatedBy: "legal", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	request, err := s.RequestTenantDSAR(ctx, owner, owner.LearnerID, "erase", "verified erasure request")
	if err != nil {
		t.Fatal(err)
	}
	worker := models.WorkerPrincipal{ActorID: "dsar-erase-worker"}
	workerScope := owner.TenantScope()
	workerScope.UserID = "worker_" + worker.ActorID
	workerScope.MembershipID = "worker_process"
	if _, _, err := s.ProcessTenantDSARErasureBatch(ctx, workerScope, worker, request.ID, 1, now); err == nil {
		t.Fatal("active legal hold did not block erasure")
	}
	if released, err := s.ReleaseRetentionLegalHold(ctx, "dsar-hold", "legal", "matter closed", now.Add(time.Minute)); err != nil || !released {
		t.Fatalf("release=%v err=%v", released, err)
	}
	if err := s.ResumeTenantDSAR(ctx, owner, request.ID); err != nil {
		t.Fatal(err)
	}
	completed := false
	for attempt := 0; attempt < 100 && !completed; attempt++ {
		var affected int64
		completed, affected, err = s.ProcessTenantDSARErasureBatch(ctx, workerScope, worker,
			request.ID, 1, now.Add(time.Duration(attempt+2)*time.Minute))
		if err != nil {
			t.Fatalf("erasure attempt %d affected=%d: %v", attempt, affected, err)
		}
	}
	if !completed {
		t.Fatal("bounded erasure did not complete")
	}
	var email, profile, objective, status string
	if err := s.queryRow(ctx, `SELECT email, profile_json, objective FROM learners
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, owner.LearnerID).Scan(&email, &profile, &objective); err != nil {
		t.Fatal(err)
	}
	if email == "test@test.com" || profile != "{}" || objective != "" {
		t.Fatalf("learner was not scrubbed: email=%q profile=%q objective=%q", email, profile, objective)
	}
	if err := s.queryRow(ctx, `SELECT status FROM tenant_dsar_requests WHERE tenant_id = ? AND id = ?`,
		owner.TenantID, request.ID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("DSAR status=%q err=%v", status, err)
	}
}
