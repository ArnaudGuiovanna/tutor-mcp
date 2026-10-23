// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func restoreChecksumOperator() models.ControlPlanePrincipal {
	return models.ControlPlanePrincipal{ActorID: "restore-content-test", Roles: []string{models.RolePlatformAdmin},
		Reason: "isolated restore verification", RequestID: "restore-content-1"}
}

func TestRestoreChecksumsComparePersistedContent(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *statisticsCohort)
	}{
		{"unchanged", func(*testing.T, *statisticsCohort) {}},
		{"mastery", func(t *testing.T, c *statisticsCohort) {
			mustExecRestoreChange(t, c, `UPDATE concept_states SET p_mastery = .123 WHERE tenant_id = ?`, c.owner.TenantID)
		}},
		{"canonical enrollment state", func(t *testing.T, c *statisticsCohort) {
			mustExecRestoreChange(t, c, `UPDATE learner_concept_states SET p_mastery = .234 WHERE tenant_id = ?`, c.owner.TenantID)
		}},
		{"cohort capacity", func(t *testing.T, c *statisticsCohort) {
			mustExecRestoreChange(t, c, `UPDATE cohorts SET capacity = capacity + 1 WHERE tenant_id = ? AND id = ?`, c.owner.TenantID, c.cohort.ID)
		}},
		{"enrollment objectives", func(t *testing.T, c *statisticsCohort) {
			mustExecRestoreChange(t, c, `UPDATE enrollments SET objectives_json = '{"changed":true}' WHERE tenant_id = ? AND id = ?`, c.owner.TenantID, c.enrolls[0].ID)
		}},
		{"membership status", func(t *testing.T, c *statisticsCohort) {
			mustExecRestoreChange(t, c, `UPDATE tenant_memberships SET status = 'suspended' WHERE tenant_id = ? AND id = ?`, c.owner.TenantID, c.members[0].MembershipID)
		}},
		{"narrative ciphertext with unchanged checksum", func(t *testing.T, c *statisticsCohort) {
			mustExecRestoreChange(t, c, `UPDATE narrative_objects SET ciphertext = 'changed-ciphertext' WHERE tenant_id = ?`, c.owner.TenantID)
		}},
		{"narrative encryption metadata", func(t *testing.T, c *statisticsCohort) {
			mustExecRestoreChange(t, c, `UPDATE narrative_objects SET key_id = 'changed-key' WHERE tenant_id = ?`, c.owner.TenantID)
		}},
		{"deleted row", func(t *testing.T, c *statisticsCohort) {
			mustExecRestoreChange(t, c, `DELETE FROM concept_states WHERE tenant_id = ? AND learner_id = ?`, c.owner.TenantID, c.members[0].LearnerID)
		}},
		{"added rows", func(t *testing.T, c *statisticsCohort) { c.add(t, .8) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newStatisticsCohort(t)
			c.add(t, .7)
			now := time.Now().UTC()
			if _, err := c.s.exec(t.Context(), `INSERT INTO narrative_objects
				(tenant_id, enrollment_id, learner_id, scope, domain_id, object_key, ciphertext,
				 key_id, version, checksum, size_bytes, created_at, updated_at)
				VALUES (?, ?, ?, 'memory', '', '', 'original-ciphertext', 'original-key', 1, ?, 19, ?, ?)`,
				c.owner.TenantID, c.enrolls[0].ID, c.members[0].LearnerID, strings.Repeat("a", 64), now, now); err != nil {
				t.Fatal(err)
			}
			operator := restoreChecksumOperator()
			tables, objects, err := c.s.ComputeTenantChecksums(t.Context(), operator, c.owner.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := c.s.RequestTenantRestoreVerification(t.Context(), operator, c.owner.TenantID, "content-backup", tables, objects)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, c)
			matched, err := c.s.VerifyTenantRestore(t.Context(), operator, c.owner.TenantID, manifest.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.name == "unchanged"
			if matched != want {
				t.Fatalf("restore matched=%v, want %v", matched, want)
			}
			var status string
			if err := c.s.queryRow(t.Context(), `SELECT status FROM tenant_restore_manifests WHERE tenant_id = ? AND id = ?`, c.owner.TenantID, manifest.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != map[bool]string{true: "verified", false: "failed"}[want] {
				t.Fatalf("manifest status=%s", status)
			}
			if _, err := c.s.VerifyTenantRestore(t.Context(), operator, c.owner.TenantID, manifest.ID); err == nil {
				t.Fatal("terminal manifest was verified a second time")
			}
		})
	}
}

func mustExecRestoreChange(t *testing.T, c *statisticsCohort, query string, args ...any) {
	t.Helper()
	result, err := c.s.exec(t.Context(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		t.Fatalf("mutation changed no row: count=%d err=%v", n, err)
	}
}

func TestRestoreChecksumsIgnoreOtherTenantsAndRejectOldKeyOnlyFormat(t *testing.T) {
	c := newStatisticsCohort(t)
	c.add(t, .7)
	operator := restoreChecksumOperator()
	tables, objects, err := c.s.ComputeTenantChecksums(t.Context(), operator, c.owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	other := seedInstitutionOwner(t, c.s, "restore-other", "other@restore.test")
	if _, err := c.s.exec(t.Context(), `UPDATE tenant_memberships SET status = 'suspended' WHERE tenant_id = ?`, other.TenantID); err != nil {
		t.Fatal(err)
	}
	afterTables, afterObjects, err := c.s.ComputeTenantChecksums(t.Context(), operator, c.owner.TenantID)
	if err != nil || tables != afterTables || objects != afterObjects {
		t.Fatalf("another tenant changed the snapshot: equal=%v/%v err=%v", tables == afterTables, objects == afterObjects, err)
	}
	var oldFormat map[string]string
	if err := json.Unmarshal([]byte(tables), &oldFormat); err != nil {
		t.Fatal(err)
	}
	for table, value := range oldFormat {
		if !strings.HasPrefix(value, tenantChecksumFormat+":") {
			t.Fatalf("unversioned checksum for %s: %s", table, value)
		}
		oldFormat[table] = strings.TrimPrefix(value, tenantChecksumFormat+":")
	}
	oldTables, _ := json.Marshal(oldFormat)
	manifest, err := c.s.RequestTenantRestoreVerification(t.Context(), operator, c.owner.TenantID, "old-key-only-backup", string(oldTables), objects)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := c.s.VerifyTenantRestore(t.Context(), operator, c.owner.TenantID, manifest.ID); err != nil || matched {
		t.Fatalf("old key-only checksums must not pass content verification: matched=%v err=%v", matched, err)
	}
}

func TestRestoreTableChecksumIsUnambiguousAndIndependentOfRowOrder(t *testing.T) {
	s := setupTestDB(t)
	if _, err := s.exec(t.Context(), `CREATE TABLE restore_checksum_probe (tenant_id TEXT, left_value TEXT, right_value TEXT, nullable_value TEXT)`); err != nil {
		t.Fatal(err)
	}
	insert := func(left, right string, nullable any) {
		t.Helper()
		if _, err := s.exec(t.Context(), `INSERT INTO restore_checksum_probe VALUES ('probe', ?, ?, ?)`, left, right, nullable); err != nil {
			t.Fatal(err)
		}
	}
	checksum := func() string {
		t.Helper()
		value, err := s.tenantTableContentChecksum(t.Context(), "restore_checksum_probe", "probe")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	insert("a:b", "c", nil)
	insert("a", "b:c", "")
	first := checksum()
	if _, err := s.exec(t.Context(), `DELETE FROM restore_checksum_probe`); err != nil {
		t.Fatal(err)
	}
	insert("a", "b:c", "")
	insert("a:b", "c", nil)
	if got := checksum(); got != first {
		t.Fatalf("insertion order changed the checksum: %s != %s", got, first)
	}
	if _, err := s.exec(t.Context(), `UPDATE restore_checksum_probe SET left_value='a', right_value='b:c' WHERE left_value='a:b'`); err != nil {
		t.Fatal(err)
	}
	if got := checksum(); got == first {
		t.Fatal("ambiguous colon-bearing fields collided")
	}
	beforeNull := checksum()
	if _, err := s.exec(t.Context(), `UPDATE restore_checksum_probe SET nullable_value='' WHERE nullable_value IS NULL`); err != nil {
		t.Fatal(err)
	}
	if got := checksum(); got == beforeNull {
		t.Fatal("SQL NULL and empty text collided")
	}
}

func TestRestoreChecksumsUseOnePostgresSnapshot(t *testing.T) {
	s := setupTestDB(t)
	if s.dialect != DialectPostgres {
		t.Skip("concurrent PostgreSQL snapshot regression")
	}
	owner := ownerPrincipal(t, s)
	operator := restoreChecksumOperator()
	var before string
	err := s.withTenantRestoreSnapshot(t.Context(), owner.TenantID, operator.ActorID, true, func(txs *Store) error {
		var err error
		before, _, err = txs.computeTenantChecksums(t.Context(), owner.TenantID)
		if err != nil {
			return err
		}
		// Commit on a separate connection between two scans of the live snapshot.
		if err := s.withTenantControlTx(t.Context(), owner.TenantID, operator.ActorID, func(writer *Store) error {
			_, err := writer.exec(t.Context(), `UPDATE tenant_memberships SET status = 'suspended' WHERE tenant_id = ? AND id = ?`, owner.TenantID, owner.MembershipID)
			return err
		}); err != nil {
			return err
		}
		after, _, err := txs.computeTenantChecksums(t.Context(), owner.TenantID)
		if err == nil && before != after {
			return fmt.Errorf("one snapshot mixed committed states")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := s.ComputeTenantChecksums(t.Context(), operator, owner.TenantID)
	if err != nil || before == after {
		t.Fatalf("new snapshot did not see committed change: same=%v err=%v", before == after, err)
	}
	// An inherited READ COMMITTED transaction cannot silently weaken the scan.
	err = s.inTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(txs *Store) error {
		_, _, err := txs.ComputeTenantChecksums(t.Context(), operator, owner.TenantID)
		if err == nil {
			return fmt.Errorf("weak inherited transaction was accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
