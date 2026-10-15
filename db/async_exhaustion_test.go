// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestAsyncJobExpiredFinalAttemptIsDeadLetteredWithoutTouchingLiveOrForeignLeases(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	scope := ownerPrincipal(t, s).TenantScope()
	now := time.Now().UTC().Truncate(time.Microsecond)
	operator := models.ControlPlanePrincipal{
		ActorID: "recovery-test", Roles: []string{models.RolePlatformAdmin},
		Reason: "verify tenant-isolated recovery", RequestID: "recovery-test",
	}
	other, err := s.ProvisionTenant(ctx, operator, "recovery-other", "Other tenant", "test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	foreign := models.TenantScope{TenantID: other.ID, UserID: "worker_recovery", MembershipID: "worker_process"}
	seed := func(jobScope models.TenantScope, id string, attempts, maxAttempts int, expiry time.Time) {
		t.Helper()
		if err := s.EnqueueAsyncJob(ctx, jobScope, models.AsyncJob{
			ID: id, TenantID: jobScope.TenantID, Kind: "test", IdempotencyKey: id, PayloadJSON: `{}`,
			MaxAttempts: maxAttempts, AvailableAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.exec(ctx, `UPDATE async_jobs SET status = 'processing', attempt_count = ?,
			lease_owner = 'previous-worker', lease_expires_at = ? WHERE tenant_id = ? AND id = ?`,
			attempts, expiry, jobScope.TenantID, id); err != nil {
			t.Fatal(err)
		}
	}
	seed(scope, "exhausted", 8, 8, now)
	seed(scope, "live-final-attempt", 8, 8, now.Add(time.Minute))
	seed(scope, "recoverable", 2, 3, now.Add(-time.Second))
	seed(foreign, "foreign-exhausted", 8, 8, now.Add(-time.Hour))

	job, err := s.ClaimAsyncJob(ctx, scope, "replacement-worker", now, time.Minute)
	if err != nil || job == nil || job.ID != "recoverable" || job.AttemptCount != 3 {
		t.Fatalf("recoverable claim=%+v err=%v", job, err)
	}
	var status, owner, cause string
	var expires, completed sql.NullTime
	if err := s.queryRow(ctx, `SELECT status, lease_owner, lease_expires_at, completed_at, last_error
		FROM async_jobs WHERE tenant_id = ? AND id = 'exhausted'`, scope.TenantID).
		Scan(&status, &owner, &expires, &completed, &cause); err != nil {
		t.Fatal(err)
	}
	if status != "dead_letter" || owner != "" || expires.Valid || !completed.Valid ||
		cause != "lease_expired_after_max_attempts" {
		t.Fatalf("final expired attempt status=%s owner=%q expiry=%v completed=%v cause=%q",
			status, owner, expires, completed, cause)
	}
	for _, tc := range []struct{ tenant, id string }{
		{scope.TenantID, "live-final-attempt"},
		{foreign.TenantID, "foreign-exhausted"},
	} {
		if err := s.queryRow(ctx, `SELECT status, lease_owner FROM async_jobs WHERE tenant_id = ? AND id = ?`,
			tc.tenant, tc.id).Scan(&status, &owner); err != nil {
			t.Fatal(err)
		}
		if status != "processing" || owner != "previous-worker" {
			t.Fatalf("unrelated lease %s changed: status=%s owner=%q", tc.id, status, owner)
		}
	}
	if err := s.FinishAsyncJob(ctx, scope, "exhausted", "previous-worker", true, time.Time{}, ""); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired owner completed terminalized attempt: %v", err)
	}
	if err := s.FinishAsyncJob(ctx, scope, job.ID, "replacement-worker", true, time.Time{}, ""); err != nil {
		t.Fatalf("recovered final attempt could not complete: %v", err)
	}
}

func TestAsyncJobExhaustionRecoveryCommitsBoundedPagesWithoutRunnableJobs(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	scope := ownerPrincipal(t, s).TenantScope()
	now := time.Now().UTC()
	// A large stale queue must not make one claim an unbounded maintenance job.
	for i := 0; i < 101; i++ {
		id := fmt.Sprintf("exhausted-%03d", i)
		if err := s.EnqueueAsyncJob(ctx, scope, models.AsyncJob{
			ID: id, TenantID: scope.TenantID, Kind: "test", IdempotencyKey: id, PayloadJSON: `{}`, MaxAttempts: 1,
			AvailableAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.exec(ctx, `UPDATE async_jobs SET status = 'processing', attempt_count = 1,
		lease_owner = 'crashed-worker', lease_expires_at = ? WHERE tenant_id = ?`, now.Add(-time.Second), scope.TenantID); err != nil {
		t.Fatal(err)
	}
	for page, want := range []int{100, 101} {
		if _, err := s.ClaimAsyncJob(ctx, scope, "replacement-worker", now, time.Minute); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("page %d unexpectedly returned runnable work: %v", page, err)
		}
		var terminal int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM async_jobs WHERE tenant_id = ? AND status = 'dead_letter'`,
			scope.TenantID).Scan(&terminal); err != nil {
			t.Fatal(err)
		}
		if terminal != want {
			t.Fatalf("committed terminal rows after page %d=%d, want %d", page, terminal, want)
		}
	}
}
