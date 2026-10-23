// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestErasureMigrationRepairsOldSeatCountersAndPreservesPasswordSync(t *testing.T) {
	raw, err := sql.Open("sqlite", fmt.Sprintf("file:erasure_upgrade_%d?mode=memory&cache=shared", testDBCounter.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if err := ensureSchemaMigrationsTable(raw); err != nil {
		t.Fatal(err)
	}
	var upgrade migration
	for _, m := range buildMigrations() {
		if m.Version == "0080_tenant_erasure_identity" {
			upgrade = m
			break
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("old schema %s: %v", m.Version, err)
		}
	}
	if upgrade.Version == "" {
		t.Fatal("erasure upgrade not found")
	}
	now := time.Now().UTC()
	if _, err := raw.Exec(`INSERT INTO learners (id, email, password_hash, objective, created_at, email_verified_at)
 VALUES ('L1', 'upgrade-erasure@example.test', 'old-password-hash', 'test', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	s := NewStore(raw)
	owner := ownerPrincipal(t, s)
	_, cohort := seedFormationBridge(t, s, owner)
	student := formationStudent(t, s, "upgrade-student@example.test")
	enrollment, err := s.EnrollMembership(t.Context(), owner, cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	// The previous erasure cleared only this flag, leaving the cohort full.
	if _, err := s.exec(t.Context(), `UPDATE enrollments SET status = 'cancelled', seat_reserved = 0 WHERE id = ?`, enrollment.ID); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(raw, upgrade); err != nil {
		t.Fatal(err)
	}
	var seats int
	if err := s.queryRow(t.Context(), `SELECT reserved_seats FROM cohorts WHERE id = ?`, cohort.ID).Scan(&seats); err != nil || seats != 0 {
		t.Fatalf("upgrade did not repair ghost reservation: %d %v", seats, err)
	}
	// The exception is limited to a running tenant erasure: a normal password
	// update still synchronizes the account and revokes its previous sessions.
	if _, err := s.exec(t.Context(), `UPDATE learners SET password_hash = 'new-password-hash' WHERE id = 'L1'`); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := s.queryRow(t.Context(), `SELECT password_hash FROM users WHERE id = ?`, owner.UserID).Scan(&hash); err != nil || hash != "new-password-hash" {
		t.Fatalf("normal password synchronization changed: %q %v", hash, err)
	}
	if err := s.ValidatePrincipal(t.Context(), owner); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("normal password update did not revoke previous principal: %v", err)
	}
}

func erasureWorkerScope(owner models.Principal) (models.TenantScope, models.WorkerPrincipal) {
	worker := models.WorkerPrincipal{ActorID: "erasure-safety-worker"}
	scope := owner.TenantScope()
	scope.UserID, scope.MembershipID = "worker_"+worker.ActorID, "worker_process"
	return scope, worker
}

func finishSafetyErasure(t *testing.T, s *Store, owner models.Principal, requestID string) {
	t.Helper()
	scope, worker := erasureWorkerScope(owner)
	for batch := 0; batch < 200; batch++ {
		done, _, err := s.ProcessTenantDSARErasureBatch(t.Context(), scope, worker, requestID, 1, time.Now().UTC())
		if err != nil {
			t.Fatalf("erasure batch %d: %v", batch, err)
		}
		if done {
			return
		}
	}
	t.Fatal("erasure did not finish")
}

func TestErasurePreservesSharedGlobalIdentityAndOtherMembership(t *testing.T) {
	s := institutionTestStore(t)
	original := formationStudent(t, s, "shared-erasure@example.test")
	ownerScope := seedInstitutionOwner(t, s, "other-erasure", "owner-erasure@example.test")
	owner, _ := verifiedOwner(t, s, ownerScope)
	_, token, err := s.CreateTenantInvitation(t.Context(), owner, "shared-erasure@example.test", []string{models.RoleLearner}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.AcceptTenantInvitation(t.Context(), token, original.UserID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetLocalUserByEmail(t.Context(), "shared-erasure@example.test")
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.RequestTenantDSAR(t.Context(), owner, member.LearnerID, "erase", "tenant profile erasure")
	if err != nil {
		t.Fatal(err)
	}
	finishSafetyErasure(t, s, owner, request.ID)
	after, err := s.GetLocalUserByEmail(t.Context(), "shared-erasure@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if after.PasswordHash != before.PasswordHash || after.TokenVersion != before.TokenVersion || after.Status != before.Status {
		t.Fatalf("tenant erasure changed global identity: before=%+v after=%+v", before, after)
	}
	if err := s.ValidatePrincipal(t.Context(), original); err != nil {
		t.Fatalf("other institution's original principal was revoked: %v", err)
	}
	var localHash, localEmail string
	if err := s.queryRow(t.Context(), `SELECT password_hash, email FROM learners WHERE tenant_id = ? AND id = ?`, member.TenantID, member.LearnerID).Scan(&localHash, &localEmail); err != nil {
		t.Fatal(err)
	}
	if localHash != "erased" || localEmail == before.Email {
		t.Fatal("tenant-local credential was not scrubbed")
	}
}

func TestErasureRevokesAndReleasesSeatsBeforeFirstDelete(t *testing.T) {
	c := newStatisticsCohort(t)
	c.add(t, .7)
	c.add(t, .4)
	victim := c.members[0]
	if _, err := c.s.CreateRefreshToken(t.Context(), victim.LearnerID, "erasure-client", "https://resource.example.test"); err != nil {
		t.Fatal(err)
	}
	request, err := c.s.RequestTenantDSAR(t.Context(), c.owner, victim.LearnerID, "erase", "early revocation")
	if err != nil {
		t.Fatal(err)
	}
	// Model a request created by the old binary: the new first checkpoint must
	// be installed ahead of already-pending credential phases.
	if _, err := c.s.exec(t.Context(), `DELETE FROM tenant_dsar_phases WHERE tenant_id = ? AND request_id = ? AND phase = 'revoke_access'`, c.owner.TenantID, request.ID); err != nil {
		t.Fatal(err)
	}
	scope, worker := erasureWorkerScope(c.owner)
	if done, _, err := c.s.ProcessTenantDSARErasureBatch(t.Context(), scope, worker, request.ID, 1, time.Now().UTC()); err != nil || done {
		t.Fatalf("first phase done=%v err=%v", done, err)
	}
	if err := c.s.ValidatePrincipal(t.Context(), victim); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("previously issued principal survived first phase: %v", err)
	}
	if _, err := c.s.GetPrincipal(t.Context(), victim.TenantScope(), victim.Scopes); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("revoked membership minted a fresh principal: %v", err)
	}
	var credentials, learningRows, seats, reserved int
	for table, target := range map[string]*int{"refresh_tokens": &credentials, "concept_states": &learningRows} {
		if err := c.s.queryRow(t.Context(), `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = ? AND learner_id = ?`, victim.TenantID, victim.LearnerID).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if credentials == 0 || learningRows == 0 {
		t.Fatal("first phase deleted data before demonstrating access revocation")
	}
	if err := c.s.queryRow(t.Context(), `SELECT reserved_seats FROM cohorts WHERE tenant_id = ? AND id = ?`, victim.TenantID, c.cohort.ID).Scan(&seats); err != nil {
		t.Fatal(err)
	}
	if err := c.s.queryRow(t.Context(), `SELECT seat_reserved FROM enrollments WHERE tenant_id = ? AND id = ?`, victim.TenantID, c.enrolls[0].ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if seats != 1 || reserved != 0 {
		t.Fatalf("first phase did not release exactly the victim's place: counter=%d victim=%d", seats, reserved)
	}
	if _, err := c.s.EnrollMembership(t.Context(), c.owner, c.cohort.ID, victim.MembershipID, "{}"); err == nil {
		t.Fatal("staff could re-enroll a learner being erased")
	}
	finishSafetyErasure(t, c.s, c.owner, request.ID)
	// A repeated completed call and a second request cannot consume another
	// learner's reservation.
	finishSafetyErasure(t, c.s, c.owner, request.ID)
	second, err := c.s.RequestTenantDSAR(t.Context(), c.owner, victim.LearnerID, "erase", "idempotent erasure")
	if err != nil {
		t.Fatal(err)
	}
	finishSafetyErasure(t, c.s, c.owner, second.ID)
	if err := c.s.queryRow(t.Context(), `SELECT reserved_seats FROM cohorts WHERE tenant_id = ? AND id = ?`, victim.TenantID, c.cohort.ID).Scan(&seats); err != nil || seats != 1 {
		t.Fatalf("replaying erasure consumed a bystander's place: seats=%d err=%v", seats, err)
	}
	if err := c.s.ValidatePrincipal(t.Context(), c.members[1]); err != nil {
		t.Fatalf("bystander was revoked: %v", err)
	}
}

func TestErasureLegalHoldAndRollbackPreserveAccessAndSeats(t *testing.T) {
	c := newStatisticsCohort(t)
	c.add(t, .7)
	victim := c.members[0]
	request, err := c.s.RequestTenantDSAR(t.Context(), c.owner, victim.LearnerID, "erase", "atomic revocation")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := c.s.CreateRetentionLegalHold(t.Context(), RetentionLegalHold{HoldID: "erasure-safety-hold", LearnerID: victim.LearnerID, Reason: "legal hold", CreatedBy: "legal", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	scope, worker := erasureWorkerScope(c.owner)
	if _, _, err := c.s.ProcessTenantDSARErasureBatch(t.Context(), scope, worker, request.ID, 1, now); !errors.Is(err, storeport.ErrTenantDSARBlocked) {
		t.Fatalf("hold did not block first phase: %v", err)
	}
	if err := c.s.ValidatePrincipal(t.Context(), victim); err != nil {
		t.Fatalf("hold revoked access before erasure started: %v", err)
	}
	if _, err := c.s.ReleaseRetentionLegalHold(t.Context(), "erasure-safety-hold", "legal", "closed", now); err != nil {
		t.Fatal(err)
	}
	if err := c.s.ResumeTenantDSAR(t.Context(), c.owner, request.ID); err != nil {
		t.Fatal(err)
	}
	trigger := `CREATE TRIGGER reject_erasure_seat BEFORE UPDATE OF reserved_seats ON cohorts
 WHEN NEW.reserved_seats < OLD.reserved_seats BEGIN SELECT RAISE(ABORT, 'injected erasure failure'); END`
	if c.s.dialect == DialectPostgres {
		trigger = `CREATE FUNCTION reject_erasure_seat() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.reserved_seats < OLD.reserved_seats THEN RAISE EXCEPTION 'injected erasure failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_erasure_seat BEFORE UPDATE OF reserved_seats ON cohorts FOR EACH ROW EXECUTE FUNCTION reject_erasure_seat()`
	}
	if _, err := c.s.exec(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.s.ProcessTenantDSARErasureBatch(t.Context(), scope, worker, request.ID, 1, now); err == nil {
		t.Fatal("injected seat failure did not abort first phase")
	}
	if err := c.s.ValidatePrincipal(t.Context(), victim); err != nil {
		t.Fatalf("rolled-back erasure left access revoked: %v", err)
	}
	var seats, archived int
	if err := c.s.queryRow(t.Context(), `SELECT reserved_seats FROM cohorts WHERE id = ?`, c.cohort.ID).Scan(&seats); err != nil {
		t.Fatal(err)
	}
	if err := c.s.queryRow(t.Context(), `SELECT archived FROM domains WHERE id = ?`, c.enrolls[0].DomainID).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if seats != 1 || archived != 0 {
		t.Fatalf("rollback lost seats/domain: %d/%d", seats, archived)
	}
	statement := `DROP TRIGGER reject_erasure_seat`
	if c.s.dialect == DialectPostgres {
		statement += ` ON cohorts`
	}
	if _, err := c.s.exec(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
	finishSafetyErasure(t, c.s, c.owner, request.ID)
}

func TestErasureCompletesWithDeliveredPostgresWorkerPrivileges(t *testing.T) {
	s := setupTestDB(t)
	if s.dialect != DialectPostgres {
		t.Skip("PostgreSQL role enforcement")
	}
	owner := ownerPrincipal(t, s)
	student := formationStudent(t, s, "worker-erasure@example.test")
	_, cohort := seedFormationBridge(t, s, owner)
	if _, err := s.EnrollMembership(t.Context(), owner, cohort.ID, student.MembershipID, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRefreshToken(t.Context(), student.LearnerID, "worker-erasure-client", "https://resource.example.test"); err != nil {
		t.Fatal(err)
	}
	request, err := s.RequestTenantDSAR(t.Context(), owner, student.LearnerID, "erase", "production worker erasure")
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := s.queryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../deploy/postgres-roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	roleSQL := strings.ReplaceAll(string(script), "\\set ON_ERROR_STOP on", "")
	roleSQL = strings.ReplaceAll(roleSQL, "SCHEMA public", "SCHEMA "+schema)
	if _, err := s.root.ExecContext(t.Context(), roleSQL); err != nil {
		t.Fatal(err)
	}
	s.root.SetMaxOpenConns(1)
	s.root.SetMaxIdleConns(1)
	if _, err := s.root.ExecContext(t.Context(), `SET ROLE tutor_worker`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.root.Exec(`RESET ROLE`); err != nil {
			t.Errorf("reset worker role: %v", err)
		}
	}()
	// The fix must not grant access to a credential payload or the global
	// account just to make a bounded DELETE work.
	for _, query := range []string{`SELECT token FROM refresh_tokens LIMIT 1`, `SELECT password_hash FROM users LIMIT 1`} {
		rows, err := s.root.QueryContext(t.Context(), query)
		if err == nil {
			rows.Close()
			t.Fatalf("worker can read secrets: %s", query)
		}
		if !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("unexpected secret-read error: %v", err)
		}
	}
	finishSafetyErasure(t, s, owner, request.ID)
}
