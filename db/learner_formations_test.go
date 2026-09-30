// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func learnerCohort(t *testing.T, policy string) (*Store, models.Principal, models.Principal, *models.FormationVersionDetail, *models.Cohort) {
	t.Helper()
	s := setupTestDB(t)
	owner := ownerPrincipal(t, s)
	d, c := seedFormationBridge(t, s, owner)
	if err := s.SetFormationEnrollmentPolicy(t.Context(), owner, d.Formation.ID, policy); err != nil {
		t.Fatal(err)
	}
	return s, owner, formationStudent(t, s, "self@formation.test"), d, c
}

func cohortSeats(t *testing.T, s *Store, owner models.Principal, id string) int {
	t.Helper()
	var seats int
	err := s.WithTenantTx(t.Context(), owner.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		return scoped.(*Store).queryRow(ctx, `SELECT reserved_seats FROM cohorts WHERE tenant_id = ? AND id = ?`, owner.TenantID, id).Scan(&seats)
	})
	if err != nil {
		t.Fatal(err)
	}
	return seats
}

func TestLearnerFormationLeaveRejoinPreservesEvidenceAndRetries(t *testing.T) {
	s, owner, student, detail, cohort := learnerCohort(t, "open")
	ctx := t.Context()
	joined, replay, err := s.JoinFormation(ctx, student, "join-1", cohort.ID)
	if err != nil || replay || joined.Status != "active" {
		t.Fatalf("join: %+v %v %v", joined, replay, err)
	}
	e := joined.Enrollment
	session, err := s.OpenLearningSession(ctx, student.LearnerID, e.DomainID, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAssessmentAttempt(ctx, &models.AssessmentAttempt{ID: "unfinished-formation-attempt", LearnerID: student.LearnerID, DomainID: e.DomainID, ConceptID: "numbers", ActivityID: "formation-activity", ActivityVersion: 1, ActivityType: "PRACTICE", Observable: "Compare", TaskText: "Compare two numbers", RubricJSON: `{}`, PassingScore: .7, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	state := models.NewConceptStateInDomain(student.LearnerID, e.DomainID, "numbers")
	state.PMastery = .73
	if err := s.UpsertConceptState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInteraction(ctx, &models.Interaction{LearnerID: student.LearnerID, DomainID: e.DomainID, Concept: "numbers", ActivityType: "PRACTICE", Success: true}); err != nil {
		t.Fatal(err)
	}
	second, _, err := s.JoinFormation(ctx, student, "join-duplicate", cohort.ID)
	if err != nil || second.Enrollment.ID != e.ID || cohortSeats(t, s, owner, cohort.ID) != 1 {
		t.Fatalf("duplicate join: %+v %v", second, err)
	}
	left, _, err := s.LeaveFormation(ctx, student, "leave-1", cohort.ID)
	if err != nil || left.Status != "cancelled" || cohortSeats(t, s, owner, cohort.ID) != 0 {
		t.Fatalf("leave: %+v %v", left, err)
	}
	domain, err := s.GetDomainByID(ctx, e.DomainID)
	if err != nil || !domain.Archived {
		t.Fatalf("archive: %+v %v", domain, err)
	}
	if err := s.UnarchiveDomain(ctx, e.DomainID, student.LearnerID); err == nil {
		t.Fatal("unarchive bypassed inactive enrollment")
	}
	if err := s.UpsertConceptState(ctx, state); err == nil {
		t.Fatal("inactive enrollment accepted learning writes")
	}
	if err := s.SubmitAssessmentAttempt(ctx, student.LearnerID, "unfinished-formation-attempt", "answer", "", time.Now()); err == nil {
		t.Fatal("cancelled enrollment accepted an old prepared response")
	}
	closed, err := s.GetLearningSession(ctx, student.LearnerID, session.ID)
	if err != nil || closed.Status != models.LearningSessionStatusClosed {
		t.Fatalf("leave kept a resumable session: %+v %v", closed, err)
	}
	if _, _, err := s.LeaveFormation(ctx, student, "leave-again", cohort.ID); err != nil || cohortSeats(t, s, owner, cohort.ID) != 0 {
		t.Fatalf("repeated leave: %v", err)
	}
	resumed, _, err := s.JoinFormation(ctx, student, "join-2", cohort.ID)
	if err != nil || resumed.Enrollment.ID != e.ID || resumed.Enrollment.DomainID != e.DomainID || resumed.Enrollment.FormationVersionID != detail.Version.ID {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	if _, replay, err := s.LeaveFormation(ctx, student, "leave-1", cohort.ID); err != nil || !replay {
		t.Fatalf("old leave replay: %v %v", replay, err)
	}
	current, err := s.GetLearnerFormation(ctx, student, cohort.ID)
	if err != nil || current.Offering.Enrollment.Status != "active" || cohortSeats(t, s, owner, cohort.ID) != 1 {
		t.Fatalf("old leave cancelled rejoin: %+v %v", current, err)
	}
	report, err := s.GetCohortReport(ctx, owner, cohort.ID)
	if err != nil || report.EnrollmentCount != 1 || report.ActiveCount != 1 || report.AverageMastery < .36 {
		t.Fatalf("lost state: %+v %v", report, err)
	}
	var observations int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM interactions WHERE enrollment_id = ?`, e.ID).Scan(&observations); err != nil || observations != 1 {
		t.Fatalf("lost history: %d %v", observations, err)
	}
	if _, err := s.RemoveFormation(ctx, owner, detail.Formation.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LeaveFormation(ctx, student, "leave-archived", cohort.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.JoinFormation(ctx, student, "join-archived", cohort.ID); !errors.Is(err, storeport.ErrFormationUnavailable) {
		t.Fatalf("archived join: %v", err)
	}
	if _, err := s.GetLearnerFormation(ctx, student, cohort.ID); err != nil {
		t.Fatalf("archived program lost: %v", err)
	}
}

func TestLearnerFormationAdmissionPolicies(t *testing.T) {
	for _, policy := range []string{"invitation", "approval"} {
		t.Run(policy, func(t *testing.T) {
			s, owner, student, _, c := learnerCohort(t, policy)
			ctx := t.Context()
			out, _, err := s.JoinFormation(ctx, student, "initial", c.ID)
			decision, version := "invited", int64(0)
			if policy == "invitation" {
				if !errors.Is(err, storeport.ErrFormationAdmissionRequired) {
					t.Fatalf("uninvited join: %v", err)
				}
			} else {
				if err != nil || out.Status != "pending" || out.Enrollment != nil {
					t.Fatalf("approval: %+v %v", out, err)
				}
				if _, _, err := s.LeaveFormation(ctx, student, "cancel-request", c.ID); err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.DecideFormationAdmission(ctx, owner, "stale-decision", c.ID, student.MembershipID, "approved", 1); err == nil {
					t.Fatal("cancelled request approved with stale version")
				}
				if _, _, err := s.JoinFormation(ctx, student, "new-request", c.ID); err != nil {
					t.Fatal(err)
				}
				decision, version = "approved", 3
			}
			if cohortSeats(t, s, owner, c.ID) != 0 {
				t.Fatal("pending admission consumed a seat")
			}
			if _, _, err := s.DecideFormationAdmission(ctx, student, "self-approve", c.ID, student.MembershipID, decision, version); !errors.Is(err, storeport.ErrInvalidPrincipal) {
				t.Fatalf("self approval: %v", err)
			}
			a, _, err := s.DecideFormationAdmission(ctx, owner, "decision", c.ID, student.MembershipID, decision, version)
			if err != nil {
				t.Fatal(err)
			}
			if cohortSeats(t, s, owner, c.ID) != 0 {
				t.Fatal("grant consumed a seat before learner confirmation")
			}
			out, _, err = s.JoinFormation(ctx, student, "confirm", c.ID)
			if err != nil || out.Status != "active" {
				t.Fatalf("confirmed: %+v %v", out, err)
			}
			if _, _, err := s.DecideFormationAdmission(ctx, owner, "revoke", c.ID, student.MembershipID, "revoked", a.Version); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.LeaveFormation(ctx, student, "leave", c.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.JoinFormation(ctx, student, "revoked", c.ID); !errors.Is(err, storeport.ErrFormationAdmissionRequired) {
				t.Fatalf("revoked rejoin: %v", err)
			}
		})
	}
}

func TestLearnerFormationAuthorizationAndPagination(t *testing.T) {
	s, owner, student, detail, c := learnerCohort(t, "open")
	ctx := t.Context()
	other := formationStudent(t, s, "other@formation.test")
	private, err := s.CreateDomain(ctx, other.LearnerID, "Private objective", "Do not share", models.KnowledgeSpace{Concepts: []string{"private"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLearnerFormation(ctx, student, "domain_cohort_"+private.ID); !errors.Is(err, storeport.ErrFormationUnavailable) {
		t.Fatalf("private domain program exposed: %v", err)
	}
	if _, _, err := s.JoinFormation(ctx, student, "private-receipt", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.JoinFormation(ctx, other, "private-receipt", c.ID); err == nil {
		t.Fatal("another learner replayed private receipt")
	}
	mine, err := s.ListLearnerFormations(ctx, other, true, "", 20)
	if err != nil || len(mine.Items) != 0 {
		t.Fatalf("another learner sees enrollment: %+v %v", mine, err)
	}
	d, err := s.GetLearnerFormation(ctx, other, c.ID)
	if err != nil || d.Offering.Enrollment != nil {
		t.Fatalf("program leaked enrollment: %+v %v", d, err)
	}
	if _, err := s.ListFormationAdmissions(ctx, student, c.ID, "", 20); err == nil {
		t.Fatal("learner read admissions")
	}
	readOnly := student
	readOnly.Scopes = []string{models.OAuthScopeLearnerRead}
	if _, _, err := s.JoinFormation(ctx, readOnly, "private-receipt", c.ID); err == nil {
		t.Fatal("read grant replayed write")
	}
	staffScope := student
	staffScope.Scopes = []string{models.OAuthScopeFormationWrite}
	if _, _, err := s.JoinFormation(ctx, staffScope, "private-receipt", c.ID); err == nil {
		t.Fatal("formation grant replayed learner mutation")
	}
	stale := student
	stale.TokenVersion++
	if _, _, err := s.LeaveFormation(ctx, stale, "stale", c.ID); err == nil {
		t.Fatal("stale principal left")
	}
	foreign := student
	foreign.TenantID = "another-tenant"
	if _, err := s.GetLearnerFormation(ctx, foreign, c.ID); err == nil {
		t.Fatal("foreign tenant read program")
	}
	if _, _, err := s.JoinFormation(ctx, owner, "staff-join", c.ID); err == nil {
		t.Fatal("staff without learner joined")
	}
	for i := range 2 {
		if _, err := s.CreateCohort(ctx, owner, detail.Version.ID, fmt.Sprintf("Extra %d", i), 2, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	after := ""
	for {
		page, err := s.ListLearnerFormations(ctx, student, false, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range page.Items {
			if seen[f.CohortID] {
				t.Fatal("duplicate pagination row")
			}
			seen[f.CohortID] = true
		}
		after = page.NextAfter
		if after == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatalf("cohorts: %v", seen)
	}
	if _, _, err := s.CreateFormationDraft(ctx, owner, "Secret draft", "Hidden"); err != nil {
		t.Fatal(err)
	}
	if len(d.Concepts) != 2 {
		t.Fatal("missing published program")
	}
}

func TestLearnerFormationAdmissionAssignmentRecheckedOnReplay(t *testing.T) {
	s, owner, student, detail, c := learnerCohort(t, "approval")
	ctx := t.Context()
	manager := formationStudent(t, s, "manager@admission.test")
	if _, err := s.SetMembershipAuthorization(ctx, manager.TenantScope(), models.MembershipStatusActive, []string{models.RolePedagogyManager}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMembershipMFAVerification(ctx, manager.TenantScope(), time.Now()); err != nil {
		t.Fatal(err)
	}
	manager, err := s.GetPrincipalForLearner(ctx, manager.LearnerID, []string{models.OAuthScopeFormationWrite})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.JoinFormation(ctx, student, "request", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListFormationAdmissions(ctx, manager, c.ID, "", 20); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("unassigned requests: %v", err)
	}
	if err := s.AssignFormationTrainer(ctx, owner, detail.Formation.ID, manager.MembershipID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideFormationAdmission(ctx, manager, "approve", c.ID, student.MembershipID, "approved", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignFormationTrainer(ctx, owner, detail.Formation.ID, manager.MembershipID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideFormationAdmission(ctx, manager, "approve", c.ID, student.MembershipID, "approved", 1); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("unassigned receipt replayed: %v", err)
	}
}

func TestLearnerFormationCrossTenantWithRealMemberships(t *testing.T) {
	s := institutionTestStore(t)
	ctx := t.Context()
	owner, _ := verifiedOwner(t, s, seedInstitutionOwner(t, s, "first", "owner@first.test"))
	_, cohort := seedFormationBridge(t, s, owner)
	secondOwner, _ := verifiedOwner(t, s, seedInstitutionOwner(t, s, "second", "owner@second.test"))
	_, token, err := s.CreateTenantInvitation(ctx, secondOwner, "learner@second.test", []string{models.RoleLearner}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.AcceptTenantInvitationWithNewUser(ctx, models.InvitationCredential{Token: token}, "hash")
	if err != nil {
		t.Fatal(err)
	}
	student, err := s.GetPrincipal(ctx, models.TenantScope{TenantID: m.TenantID, UserID: m.UserID, MembershipID: m.ID, LearnerID: m.LearnerID}, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListLearnerFormations(ctx, student, false, "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("foreign catalog visible: %+v %v", page, err)
	}
	if _, err := s.GetLearnerFormation(ctx, student, cohort.ID); err == nil {
		t.Fatal("cross-tenant program read")
	}
	if _, _, err := s.JoinFormation(ctx, student, "cross", cohort.ID); err == nil {
		t.Fatal("cross-tenant join")
	}
	if _, _, err := s.LeaveFormation(ctx, student, "cross-leave", cohort.ID); err == nil {
		t.Fatal("cross-tenant leave")
	}
	if _, _, err := s.DecideFormationAdmission(ctx, secondOwner, "cross-invite", cohort.ID, student.MembershipID, "invited", 0); err == nil {
		t.Fatal("cross-tenant invitation")
	}
}

func TestLearnerFormationLastSeatConcurrentAndExpiredCohort(t *testing.T) {
	s, owner, first, detail, _ := learnerCohort(t, "open")
	ctx := t.Context()
	c, err := s.CreateCohort(ctx, owner, detail.Version.ID, "Last seat", 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second := formationStudent(t, s, "race@formation.test")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, p := range []models.Principal{first, second} {
		wg.Go(func() { _, _, err := s.JoinFormation(ctx, p, fmt.Sprintf("race-%d", i), c.ID); errs <- err })
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, storeport.ErrCohortCapacityReached) {
			t.Fatal(err)
		}
	}
	if winners != 1 || cohortSeats(t, s, owner, c.ID) != 1 {
		t.Fatalf("winners: %d", winners)
	}
	end := time.Now().Add(-time.Hour)
	expired, err := s.CreateCohort(ctx, owner, detail.Version.ID, "Ended", 10, nil, &end)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.JoinFormation(ctx, first, "ended", expired.ID); !errors.Is(err, storeport.ErrFormationUnavailable) {
		t.Fatalf("expired: %v", err)
	}
}

func TestLearnerFormationMigrationCannotResurrectSource(t *testing.T) {
	s, owner, student, detail, c := learnerCohort(t, "open")
	ctx := t.Context()
	e, _, err := s.JoinFormation(ctx, student, "join", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.CloneFormationVersion(ctx, owner, detail.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, next.Version.ID); err != nil {
		t.Fatal(err)
	}
	target, err := s.CreateCohort(ctx, owner, next.Version.ID, "Next", 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateFormationEnrollment(ctx, owner, e.Enrollment.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.JoinFormation(ctx, student, "resurrect", c.ID); !errors.Is(err, storeport.ErrEnrollmentTransition) {
		t.Fatalf("resurrect migrated: %v", err)
	}
	if cohortSeats(t, s, owner, c.ID) != 0 || cohortSeats(t, s, owner, target.ID) != 1 {
		t.Fatal("wrong migration capacity")
	}
}

func TestLearnerFormationLeaveWaitsForInFlightEvidence(t *testing.T) {
	s, owner, student, _, c := learnerCohort(t, "open")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	joined, _, err := s.JoinFormation(ctx, student, "join", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	written, left := make(chan error, 1), make(chan error, 1)
	go func() {
		written <- s.WithTenantTx(ctx, student.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
			txs := scoped.(*Store)
			if _, _, err := txs.formationLearningScope(txCtx, student.LearnerID, joined.Enrollment.DomainID, "numbers"); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			state := models.NewConceptStateInDomain(student.LearnerID, joined.Enrollment.DomainID, "numbers")
			state.PMastery = .8
			return txs.UpsertConceptState(txCtx, state)
		})
	}()
	select {
	case <-locked:
	case err := <-written:
		t.Fatalf("learning transaction: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { _, _, err := s.LeaveFormation(ctx, student, "leave", c.ID); left <- err }()
	select {
	case err := <-left:
		t.Fatalf("leave passed in-flight evidence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-left; err != nil {
		t.Fatal(err)
	}
	if cohortSeats(t, s, owner, c.ID) != 0 {
		t.Fatal("seat not released")
	}
	if _, _, err := s.JoinFormation(ctx, student, "rejoin", c.ID); err != nil {
		t.Fatal(err)
	}
	state, err := s.GetConceptStateInDomain(ctx, student.LearnerID, joined.Enrollment.DomainID, "numbers")
	if err != nil || state.PMastery != .8 {
		t.Fatalf("in-flight evidence lost: %+v %v", state, err)
	}
}
