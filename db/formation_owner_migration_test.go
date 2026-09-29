// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestPostgresFormationOwnerBackfillRespectsForcedRLS(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("requires PostgreSQL")
	}
	s := setupTestDB(t)
	ctx := t.Context()
	want := map[string]string{}
	for i := range 2 {
		scope := seedInstitutionOwner(t, s, fmt.Sprintf("backfill-%d", i), fmt.Sprintf("owner%d@backfill.test", i))
		if _, err := s.RecordMembershipMFAVerification(ctx, scope, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		p, err := s.GetPrincipal(ctx, scope, []string{models.OAuthScopeLearner})
		if err != nil {
			t.Fatal(err)
		}
		f, _, err := s.CreateFormationDraft(ctx, p, "Existing formation", "")
		if err != nil {
			t.Fatal(err)
		}
		want[f.ID] = p.MembershipID
		if _, err := s.exec(ctx, `UPDATE formations SET owner_membership_id = '' WHERE id = ?`, f.ID); err != nil {
			t.Fatal(err)
		}
	}
	var schema string
	if err := s.root.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf(`"tutor_formation_backfill_%d"`, testDBCounter.Add(1))
	if _, err := s.root.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.root.ExecContext(context.Background(), `DROP OWNED BY `+role)
		_, _ = s.root.ExecContext(context.Background(), `DROP ROLE `+role)
	}()
	for _, q := range []string{`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role, `GRANT SELECT ON tenants, tenant_memberships, formations TO ` + role, `GRANT UPDATE ON formations TO ` + role} {
		if _, err := s.root.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{`SET LOCAL ROLE ` + role, `SELECT set_config('app.current_tenant', 'unrelated-tenant', true)`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// Reproduce the old unscoped migration: forced RLS hides existing rows.
	result, err := tx.ExecContext(ctx, `UPDATE formations SET owner_membership_id = 'wrong' WHERE owner_membership_id = ''`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := result.RowsAffected(); n != 0 {
		t.Fatal("fixture did not enforce tenant isolation")
	}
	for range 2 {
		if _, err := tx.ExecContext(ctx, postgresFormationOwnerBackfill); err != nil {
			t.Fatal(err)
		}
	}
	var restored string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('app.current_tenant', true)`).Scan(&restored); err != nil || restored != "unrelated-tenant" {
		t.Fatalf("migration leaked tenant scope: %s %v", restored, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for id, membership := range want {
		var got string
		if err := s.queryRow(ctx, `SELECT owner_membership_id FROM formations WHERE id = ?`, id).Scan(&got); err != nil || got != membership {
			t.Fatalf("backfill = %s, want %s: %v", got, membership, err)
		}
	}
	for _, table := range []string{"formations", "tenant_memberships"} {
		var enabled, forced bool
		if err := s.root.QueryRowContext(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, table).Scan(&enabled, &forced); err != nil || !enabled || !forced {
			t.Fatalf("isolation changed on %s: %v", table, err)
		}
	}
}
