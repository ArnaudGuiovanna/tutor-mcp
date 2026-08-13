// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestWithTenantTxRoutesRootStoreCallsAndRollsBack(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	principal, err := s.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearnerWrite})
	if err != nil {
		t.Fatal(err)
	}
	wantRollback := errors.New("rollback tenant operation")
	err = s.WithTenantTx(ctx, principal.TenantScope(), func(txCtx context.Context, _ storeport.Store) error {
		// Deliberately call the root Store captured by application handlers. The
		// transaction-bearing context must route this call through the tx.
		if _, err := s.CreateDomain(txCtx, "L1", "rolled back", "", models.KnowledgeSpace{}); err != nil {
			return err
		}
		return wantRollback
	})
	if !errors.Is(err, wantRollback) {
		t.Fatalf("WithTenantTx error = %v, want rollback sentinel", err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM domains WHERE name = ?`, "rolled back").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled-back tenant write count = %d, want 0", count)
	}
}

func TestPostgresForcedRLSAllOperationsAndPoolReset(t *testing.T) {
	pgDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	if pgDSN == "" {
		t.Skip("TUTOR_TEST_PG_DSN is not configured")
	}
	s := setupTestPG(t, pgDSN)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := s.exec(ctx, `INSERT INTO tenants
        (id, slug, name, status, region, policy_json, created_at, updated_at)
        VALUES ('tenant-b', 'tenant-b', 'Tenant B', 'active', 'default', '{}', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `INSERT INTO learners
        (id, email, password_hash, objective, created_at, email_verified_at, tenant_id, user_id, membership_id)
        VALUES ('L2', 'tenant-b@example.com', 'hash', '', ?, ?, 'tenant-b', 'user-b', 'membership-b')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `INSERT INTO domains
        (id, learner_id, tenant_id, name, graph_json) VALUES ('domain-b', 'L2', 'tenant-b', 'B', '{"concepts":[],"prerequisites":{}}')`); err != nil {
		t.Fatal(err)
	}

	var schema string
	if err := s.root.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf("tutor_rls_test_%d", testDBCounter.Add(1))
	quotedRole := `"` + role + `"`
	if _, err := s.root.ExecContext(ctx, `CREATE ROLE `+quotedRole+` NOLOGIN NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.root.ExecContext(context.Background(), `REVOKE ALL ON ALL TABLES IN SCHEMA `+schema+` FROM `+quotedRole)
		_, _ = s.root.ExecContext(context.Background(), `REVOKE USAGE ON SCHEMA `+schema+` FROM `+quotedRole)
		_, _ = s.root.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+quotedRole)
	}()
	if _, err := s.root.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+schema+` TO `+quotedRole); err != nil {
		t.Fatal(err)
	}
	if _, err := s.root.ExecContext(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA `+schema+` TO `+quotedRole); err != nil {
		t.Fatal(err)
	}

	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+quotedRole); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', 'tenant_legacy', true)`); err != nil {
		t.Fatal(err)
	}
	var visible int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM learners`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 1 {
		t.Fatalf("tenant A visible learners = %d, want 1", visible)
	}
	for name, statement := range map[string]string{
		"update": `UPDATE learners SET objective = 'leak' WHERE id = 'L2'`,
		"delete": `DELETE FROM domains WHERE id = 'domain-b'`,
	} {
		result, err := tx.ExecContext(ctx, statement)
		if err != nil {
			t.Fatalf("%s hidden row: %v", name, err)
		}
		if affected, _ := result.RowsAffected(); affected != 0 {
			t.Fatalf("cross-tenant %s affected %d rows, want 0", name, affected)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO domains
        (id, learner_id, tenant_id, name, graph_json)
        VALUES ('domain-b2', 'L2', 'tenant-b', 'B2', '{"concepts":[],"prerequisites":{}}')`); err == nil {
		t.Fatal("RLS WITH CHECK accepted an insert for another tenant")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// A fresh transaction on a pooled connection has no tenant until SET LOCAL
	// is called again. Missing scope fails closed instead of inheriting A.
	tx, err = s.root.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+quotedRole); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM learners`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("pooled connection leaked tenant scope: visible=%d, want 0", visible)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', 'tenant-b', true)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM learners`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 1 {
		t.Fatalf("tenant B visible learners = %d, want 1", visible)
	}
}
