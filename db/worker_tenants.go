// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const sqliteWorkerTenantMigration = `CREATE TABLE worker_tenant_runs (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), actor_id TEXT NOT NULL,
    job_name TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('started','succeeded','failed')),
    error_code TEXT NOT NULL DEFAULT '', occurred_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX idx_worker_tenant_runs_time ON worker_tenant_runs(tenant_id, occurred_at DESC);`

const postgresWorkerTenantMigration = `CREATE TABLE worker_tenant_runs (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), actor_id TEXT NOT NULL,
    job_name TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('started','succeeded','failed')),
    error_code TEXT NOT NULL DEFAULT '', occurred_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX idx_worker_tenant_runs_time ON worker_tenant_runs(tenant_id, occurred_at DESC);
ALTER TABLE worker_tenant_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE worker_tenant_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON worker_tenant_runs
USING (tenant_id = current_setting('app.current_tenant', true))
WITH CHECK (tenant_id = current_setting('app.current_tenant', true));`

// ListWorkerTenantScopes is the sole cross-tenant worker enumeration boundary.
// It reads only the global tenant roots and returns opaque synthetic actor IDs;
// no tenant-owned business row is exposed outside a scoped transaction.
func (s *Store) ListWorkerTenantScopes(ctx context.Context, worker models.WorkerPrincipal, after string, limit int) ([]models.TenantScope, string, error) {
	if !worker.Validate() || limit < 1 || limit > 1000 {
		return nil, "", fmt.Errorf("list worker tenant scopes: invalid worker or page")
	}
	rows, err := s.query(ctx, `SELECT id FROM tenants
		WHERE status = 'active' AND id > ? ORDER BY id LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var scopes []models.TenantScope
	for rows.Next() {
		var tenantID string
		if err := rows.Scan(&tenantID); err != nil {
			return nil, "", err
		}
		scopes = append(scopes, models.TenantScope{
			TenantID: tenantID, UserID: "worker_" + worker.ActorID,
			MembershipID: "worker_process",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(scopes) > limit {
		scopes = scopes[:limit]
		next = scopes[len(scopes)-1].TenantID
	}
	return scopes, next, nil
}

func (s *Store) RecordWorkerTenantRun(ctx context.Context, worker models.WorkerPrincipal, scope models.TenantScope, jobName, status, errorCode string) error {
	if !worker.Validate() || scope.TenantID == "" || !strings.HasPrefix(scope.UserID, "worker_") ||
		scope.MembershipID != "worker_process" || strings.TrimSpace(jobName) == "" || len(jobName) > 128 ||
		(status != "started" && status != "succeeded" && status != "failed") || len(errorCode) > 128 {
		return fmt.Errorf("record worker tenant run: invalid input")
	}
	id, err := generateID()
	if err != nil {
		return err
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		_, err := scoped.(*Store).exec(txCtx, `INSERT INTO worker_tenant_runs
			(id, tenant_id, actor_id, job_name, status, error_code, occurred_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, scope.TenantID, worker.ActorID,
			jobName, status, errorCode, time.Now().UTC())
		return err
	})
}
