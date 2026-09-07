// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestPedagogyJournalsPostgresForcedRLS(t *testing.T) {
	dsn := os.Getenv("TUTOR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TUTOR_TEST_PG_DSN is not configured")
	}
	s := setupTestPG(t, dsn)
	ctx := context.Background()
	owner := reviewWriter(ownerPrincipal(t, s))
	f, a := reviewAttemptForTest(t, s, "L2", "journal-rls", "failed")
	if _, err := s.exec(ctx, `UPDATE learners SET email_verified_at = ? WHERE id = ?`, time.Now().UTC(), "L2"); err != nil {
		t.Fatal(err)
	}
	review := recordReviewForTest(t, s, owner, a.ID, "journal-rls")
	adjudicator, sign := adjudicatorForTest(t, s, owner.TenantID)
	if _, _, err := adjudicator.AdjudicateAssessment(ctx, owner, a.ID, sign(review, "rls-cert", "accept", 0)); err != nil {
		t.Fatal(err)
	}
	learner, err := s.GetPrincipal(ctx, models.LegacyPrincipal("L2").TenantScope(), []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordLearningEvent(ctx, learner, models.LearningEventRequest{EventKey: "rls-feedback", DomainID: f.domain.ID, ConceptID: "a", AttemptID: a.ID, Kind: "feedback"}); err != nil {
		t.Fatal(err)
	}
	material, err := s.GetCurriculumReviewMaterial(ctx, owner, f.domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordCurriculumReview(ctx, owner, f.domain.ID, 1, "rls-curriculum", material.MaterialHash, curriculumFindingsForTest(t, material.Snapshot)); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := s.root.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf(`"tutor_pedagogy_rls_%d"`, testDBCounter.Add(1))
	if _, err := s.root.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.root.ExecContext(context.Background(), `DROP OWNED BY `+role)
		_, _ = s.root.ExecContext(context.Background(), `DROP ROLE `+role)
	}()
	for _, grant := range []string{`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role, `GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role} {
		if _, err := s.root.ExecContext(ctx, grant); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"learning_events", "assessment_adjudications", "curriculum_review_opinions"} {
		t.Run(table, func(t *testing.T) {
			var enabled, forced bool
			if err := s.root.QueryRowContext(ctx, `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, table).Scan(&enabled, &forced); err != nil || !enabled || !forced {
				t.Fatalf("RLS flags: %t %t %v", enabled, forced, err)
			}
			tx, err := s.root.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, statement := range []string{`SET LOCAL ROLE ` + role, `SELECT set_config('app.current_tenant','tenant_legacy',true)`} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			var raw string
			if err := tx.QueryRowContext(ctx, `SELECT to_jsonb(row)::text FROM `+table+` row LIMIT 1`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant','foreign',true)`); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("foreign read: %d %v", count, err)
			}
			for _, statement := range []string{`UPDATE ` + table + ` SET id=id`, `DELETE FROM ` + table} {
				result, err := tx.ExecContext(ctx, statement)
				if err != nil {
					t.Fatal(err)
				}
				if n, _ := result.RowsAffected(); n != 0 {
					t.Fatal("foreign mutation succeeded")
				}
			}
			if _, err := tx.ExecContext(ctx, `SAVEPOINT wrong_tenant`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO `+table+` SELECT (jsonb_populate_record(NULL::`+table+`,$1::jsonb)).*`, raw)
			// BEFORE INSERT guards can reject first when RLS hides the
			// referenced parent. Uniqueness failures do not count as isolation.
			if err == nil || (!strings.Contains(err.Error(), "row-level security") && !strings.Contains(err.Error(), "scope mismatch")) {
				t.Fatalf("insert must fail at the tenant boundary: %v", err)
			}
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT wrong_tenant`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant','tenant_legacy',true)`); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 1 {
				t.Fatalf("own rows changed: %d %v", count, err)
			}
		})
	}
}
