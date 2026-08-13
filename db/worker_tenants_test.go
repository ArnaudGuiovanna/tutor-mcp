// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestWorkerTenantEnumerationIsGlobalBoundedAndAudited(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	worker := models.WorkerPrincipal{ActorID: "worker-enumeration-test"}
	if _, _, err := s.ListWorkerTenantScopes(ctx, models.WorkerPrincipal{}, "", 10); err == nil {
		t.Fatal("invalid worker enumerated tenants")
	}
	scopes, next, err := s.ListWorkerTenantScopes(ctx, worker, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || next != "" {
		t.Fatalf("tenant page len=%d next=%q", len(scopes), next)
	}
	scope := scopes[0]
	if err := scope.Validate(); err != nil || !strings.Contains(scope.UserID, worker.ActorID) || scope.MembershipID == "" {
		t.Fatalf("worker scope=%#v err=%v", scope, err)
	}
	if err := s.RecordWorkerTenantRun(ctx, worker, scope, "retention", "started", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordWorkerTenantRun(ctx, worker, scope, "retention", "failed", "bounded_test_failure"); err != nil {
		t.Fatal(err)
	}
	foreign := scope
	foreign.TenantID = "tenant-not-enumerated"
	if err := s.RecordWorkerTenantRun(ctx, worker, foreign, "retention", "started", ""); err == nil {
		t.Fatal("worker recorded a run for an unknown tenant")
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM worker_tenant_runs
		WHERE tenant_id = ? AND actor_id = ? AND job_name = ?`, scope.TenantID,
		worker.ActorID, "retention").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("worker audit rows=%d, want 2", count)
	}
}
