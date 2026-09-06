// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestCurriculumReconciliationMigrationPreservesReferencesAndForeignKeys(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("preexisting_broken_fk=%t", broken), func(t *testing.T) {
			ctx := context.Background()
			raw, err := OpenDB(filepath.Join(t.TempDir(), "upgrade.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			if err := ensureSchemaMigrationsTable(raw); err != nil {
				t.Fatal(err)
			}
			for _, migration := range buildMigrations() {
				if migration.Version == "0065_curriculum_reconciliation" {
					break
				}
				if err := applyMigration(raw, migration); err != nil {
					t.Fatalf("old migration %s: %v", migration.Version, err)
				}
			}
			s := NewStore(raw)
			f := newPedagogicalDecisionFixture(t, s, "L1", "upgrade")
			a := f.attempt(t, "pre-upgrade-attempt")
			if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
				t.Fatal(err)
			}
			next := models.CloneCurriculumSnapshot(f.curriculum)
			next.Version, next.ParentVersion, next.CreatedAt = 2, 1, time.Now().UTC()
			next.Operation = models.CurriculumOperation{Type: models.CurriculumOperationRename, Rationale: "Before upgrade"}
			if _, err := s.insertCurriculumSnapshot(ctx, "L1", next, false); err != nil {
				t.Fatal(err)
			}
			before := map[int]string{}
			for _, version := range []int{1, 2} {
				var payload string
				if err := raw.QueryRow(`SELECT snapshot_json FROM curriculum_versions WHERE domain_id = ? AND version = ?`, f.domain.ID, version).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				before[version] = payload
			}
			if broken {
				// Emulate legacy corruption to prove the rebuild aborts instead of
				// committing with disabled constraints or silently dropping history.
				if _, err := raw.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`INSERT INTO pedagogical_decisions
				 (id, tenant_id, learner_id, domain_id, session_id, curriculum_version, snapshot_json, created_at)
				 VALUES ('orphan', 'tenant_legacy', 'L1', ?, ?, 999, '{}', ?)`, f.domain.ID, f.decision.SessionID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`PRAGMA foreign_keys = ON`); err != nil {
					t.Fatal(err)
				}
			}
			err = Migrate(raw)
			if (err != nil) != broken {
				t.Fatalf("migration error=%v broken=%t", err, broken)
			}
			var enabled, applied int
			if err := raw.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
				t.Fatalf("foreign keys not restored: %d %v", enabled, err)
			}
			if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = '0065_curriculum_reconciliation'`).Scan(&applied); err != nil || (applied == 0) != broken {
				t.Fatalf("invalid migration ledger commit: %d %v", applied, err)
			}
			for version, payload := range before {
				var got string
				if err := raw.QueryRow(`SELECT snapshot_json FROM curriculum_versions WHERE domain_id = ? AND version = ?`, f.domain.ID, version).Scan(&got); err != nil || got != payload {
					t.Fatalf("version %d changed/lost: %v", version, err)
				}
			}
			if !broken {
				if err := Migrate(raw); err != nil {
					t.Fatalf("second migration: %v", err)
				}
				attempt, err := s.GetAssessmentAttempt(ctx, "L1", a.ID)
				if err != nil || attempt.CurriculumInvalidatedVersion != 0 || attempt.CurriculumVersion != 1 {
					t.Fatalf("existing assessment changed: %+v %v", attempt, err)
				}
				if _, err := raw.Exec(`UPDATE curriculum_versions SET snapshot_json = '{}' WHERE domain_id = ?`, f.domain.ID); err == nil {
					t.Fatal("immutable-history trigger lost on rebuild")
				}
				if _, err := raw.Exec(`DELETE FROM curriculum_versions WHERE domain_id = ?`, f.domain.ID); err == nil {
					t.Fatal("immutable-history delete guard lost on rebuild")
				}
				rows, err := raw.Query(`PRAGMA foreign_key_check`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				if rows.Next() {
					t.Fatal("foreign-key reference damaged by rebuild")
				}
			}
		})
	}
}
