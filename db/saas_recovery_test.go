// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestTenantDSARResumeRearmsExhaustedJobAtomically(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	ensureErasureReplacementOwner(t, s, owner)
	request, err := s.RequestTenantDSAR(ctx, owner, owner.LearnerID, "erase", "verified request")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `UPDATE tenant_dsar_requests SET status='blocked' WHERE id=?`, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `UPDATE tenant_dsar_phases SET status=CASE WHEN position=0 THEN 'completed' ELSE 'blocked' END WHERE request_id=?`, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `UPDATE async_jobs SET status='dead_letter',attempt_count=max_attempts,last_error='dsar_processing_failed' WHERE idempotency_key=?`, request.ID); err != nil {
		t.Fatal(err)
	}
	// Reject the last statement of resume: all three state changes must roll back.
	if s.dialect == DialectSQLite {
		_, err = s.exec(ctx, `CREATE TRIGGER reject_resume_audit BEFORE INSERT ON audit_events WHEN NEW.action='dsar.resume'
			BEGIN SELECT RAISE(ABORT,'injected resume audit failure'); END`)
	} else {
		_, err = s.exec(ctx, `CREATE FUNCTION reject_resume_audit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.action='dsar.resume' THEN RAISE EXCEPTION 'injected resume audit failure'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER reject_resume_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_resume_audit()`)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeTenantDSAR(ctx, owner, request.ID); err == nil {
		t.Fatal("resume ignored failed audit")
	}
	assertDSARRecoveryState(t, s, request.ID, "blocked", "dead_letter", 32)
	if _, err := s.exec(ctx, `DROP TRIGGER reject_resume_audit`+map[bool]string{true: " ON audit_events"}[s.dialect == DialectPostgres]); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeTenantDSAR(ctx, owner, request.ID); err != nil {
		t.Fatal(err)
	}
	assertDSARRecoveryState(t, s, request.ID, "pending", "pending", 0)
	var phase string
	if err := s.queryRow(ctx, `SELECT status FROM tenant_dsar_phases WHERE request_id=? AND position=0`, request.ID).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != "completed" {
		t.Fatalf("completed checkpoint changed to %s", phase)
	}
	job, err := s.ClaimAsyncJob(ctx, owner.TenantScope(), "resumed-worker", time.Now().UTC(), time.Minute)
	if err != nil || job.IdempotencyKey != request.ID || job.AttemptCount != 1 {
		t.Fatalf("resumed job=%+v err=%v", job, err)
	}
}

func TestTenantDSARResumeRefusesLiveJobAndActiveHold(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	ensureErasureReplacementOwner(t, s, owner)
	request, err := s.RequestTenantDSAR(ctx, owner, owner.LearnerID, "erase", "verified request")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAsyncJob(ctx, owner.TenantScope(), "live-worker", time.Now().UTC(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `UPDATE tenant_dsar_requests SET status='blocked' WHERE id=?`, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeTenantDSAR(ctx, owner, request.ID); err == nil {
		t.Fatal("resume stole live lease")
	}
	assertDSARRecoveryState(t, s, request.ID, "blocked", "processing", 1)
	if err := s.CreateRetentionLegalHold(ctx, RetentionLegalHold{HoldID: "active-resume-hold", LearnerID: owner.LearnerID, Reason: "litigation", CreatedBy: "legal", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeTenantDSAR(ctx, owner, request.ID); !errors.Is(err, storeport.ErrTenantDSARBlocked) {
		t.Fatalf("resume with active hold=%v", err)
	}
	assertDSARRecoveryState(t, s, request.ID, "blocked", "processing", 1)
}

func TestTenantDSARParkingFencesStaleWorkerAndResume(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	scope := owner.TenantScope()
	ensureErasureReplacementOwner(t, s, owner)
	request, err := s.RequestTenantDSAR(ctx, owner, owner.LearnerID, "erase", "verified request")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := s.ClaimAsyncJob(ctx, scope, "process.attempt-a", now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `UPDATE async_jobs SET lease_expires_at = ? WHERE id = ?`, now.Add(-time.Second), first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimAsyncJob(ctx, scope, "process.attempt-b", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRetentionLegalHold(ctx, RetentionLegalHold{HoldID: "parking-hold", LearnerID: owner.LearnerID, Reason: "litigation", CreatedBy: "legal", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	worker := models.WorkerPrincipal{ActorID: "parking-test"}
	if _, _, err := s.ProcessTenantDSARErasureBatch(ctx, scope, worker, request.ID, 500, now); !errors.Is(err, storeport.ErrTenantDSARBlocked) {
		t.Fatalf("hold result=%v", err)
	}
	if err := s.ParkTenantDSARJob(ctx, scope, first.ID, request.ID, first.LeaseOwner, first.AttemptCount, now); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old worker parked replacement: %v", err)
	}
	assertDSARRecoveryState(t, s, request.ID, "blocked", "processing", 2)
	if _, err := s.ReleaseRetentionLegalHold(ctx, "parking-hold", "legal", "closed", now); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeTenantDSAR(ctx, owner, request.ID); err == nil {
		t.Fatal("resume stole the replacement's live lease")
	}
	if err := s.ParkTenantDSARJob(ctx, scope, second.ID, request.ID, second.LeaseOwner, second.AttemptCount, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeTenantDSAR(ctx, owner, request.ID); err != nil {
		t.Fatal(err)
	}
	third, err := s.ClaimAsyncJob(ctx, scope, "process.attempt-c", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if third.AttemptCount != 1 {
		t.Fatalf("resumed attempts=%d", third.AttemptCount)
	}
	if err := s.ParkTenantDSARJob(ctx, scope, first.ID, request.ID, first.LeaseOwner, first.AttemptCount, time.Now().UTC()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old park after resume=%v", err)
	}
	if err := s.FinishAsyncJob(ctx, scope, first.ID, first.LeaseOwner, true, time.Time{}, ""); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old completion after resume=%v", err)
	}
	assertDSARRecoveryState(t, s, request.ID, "pending", "processing", 1)
}

func assertDSARRecoveryState(t *testing.T, s *Store, requestID, requestStatus, jobStatus string, attempts int) {
	t.Helper()
	var gotRequest, gotJob string
	var gotAttempts int
	if err := s.queryRow(context.Background(), `SELECT r.status,j.status,j.attempt_count FROM tenant_dsar_requests r
		JOIN async_jobs j ON j.tenant_id=r.tenant_id AND j.idempotency_key=r.id WHERE r.id=?`, requestID).Scan(&gotRequest, &gotJob, &gotAttempts); err != nil {
		t.Fatal(err)
	}
	if gotRequest != requestStatus || gotJob != jobStatus || gotAttempts != attempts {
		t.Fatalf("request/job/attempts=%s/%s/%d want%s/%s/%d", gotRequest, gotJob, gotAttempts, requestStatus, jobStatus, attempts)
	}
}

func TestTenantDSARExportReplayAfterManifestCommit(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	scope := owner.TenantScope()
	worker := models.WorkerPrincipal{ActorID: "export-recovery"}
	request, err := s.RequestTenantDSAR(ctx, owner, owner.LearnerID, "export", "verified request")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job, err := s.ClaimAsyncJob(ctx, scope, "export.attempt-a", now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTenantDSARExport(ctx, scope, worker, request.ID); err != nil {
		t.Fatal(err)
	}
	var original string
	if err := s.queryRow(ctx, `SELECT result_json FROM tenant_dsar_requests WHERE id=?`, request.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	// Simulate death after the manifest committed, before the queue ACK.
	recovered, err := s.ClaimAsyncJob(ctx, scope, "export.attempt-b", now.Add(2*time.Second), time.Minute)
	if err != nil || recovered.ID != job.ID || recovered.AttemptCount != 2 {
		t.Fatalf("recovered job=%+v err=%v", recovered, err)
	}
	if err := s.CompleteTenantDSARExport(ctx, scope, worker, request.ID); err != nil {
		t.Fatalf("completed export not replayable: %v", err)
	}
	if err := s.FinishAsyncJob(ctx, scope, recovered.ID, recovered.LeaseOwner, true, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	assertDSARRecoveryState(t, s, request.ID, "completed", "completed", 2)
	var replay string
	if err := s.queryRow(ctx, `SELECT result_json FROM tenant_dsar_requests WHERE id=?`, request.ID).Scan(&replay); err != nil {
		t.Fatal(err)
	}
	if replay != original {
		t.Fatal("recovery regenerated a committed export manifest")
	}
}
