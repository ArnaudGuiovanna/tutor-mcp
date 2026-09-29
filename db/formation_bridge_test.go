// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func seedFormationBridge(t *testing.T, s *Store, actor models.Principal) (*models.FormationVersionDetail, *models.Cohort) {
	t.Helper()
	ctx := t.Context()
	_, v, err := s.CreateFormationDraft(ctx, actor, "Shared algebra", "Learn algebra")
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.AddFormationConceptsIdempotent(ctx, actor, "contents-"+v.ID, v.ID, nil, []models.FormationConceptInput{
		{StableKey: "equations", Label: "Equations", Description: "Solve linear equations", Position: 1, Prerequisites: []string{"numbers"}, Outcomes: []models.CurriculumOutcome{{ID: "solve", Statement: "Solve an equation"}}, Criteria: []models.CurriculumCriterion{{ID: "check", Description: "Check by substitution"}}},
		{StableKey: "numbers", Label: "Numbers", Description: "Compare numbers", Position: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, actor, v.ID); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCohort(ctx, actor, v.ID, "September", 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, c
}

func formationStudent(t *testing.T, s *Store, email string) models.Principal {
	t.Helper()
	learner, err := s.CreateLearner(t.Context(), email, "hash", "Learn", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPrincipalForLearner(t.Context(), learner.ID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFormationEnrollmentSharesConceptsAndLocksCurriculum(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	students := []models.Principal{formationStudent(t, s, "alice@bridge.test"), formationStudent(t, s, "bob@bridge.test")}
	var first *models.Enrollment
	for _, student := range students {
		e, err := s.EnrollMembership(ctx, owner, cohort.ID, student.MembershipID, "{}")
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = e
		} else if e.DomainID == first.DomainID || e.ID == first.ID {
			t.Fatal("learners share a domain or enrollment")
		}
		d, err := s.GetDomainByID(ctx, e.DomainID)
		if err != nil {
			t.Fatal(err)
		}
		if d.FormationVersionID != detail.Version.ID || d.FormationEnrollmentID != e.ID {
			t.Fatalf("domain source = %+v", d)
		}
		for _, c := range detail.Concepts {
			state := models.NewConceptStateInDomain(student.LearnerID, d.ID, c.StableKey)
			state.PMastery = 0.7
			if err := s.UpsertConceptState(ctx, state); err != nil {
				t.Fatal(err)
			}
			scope, err := s.resolveLearningScope(ctx, student.LearnerID, d.ID, c.StableKey)
			if err != nil || scope.FormationConceptID != c.ID || scope.EnrollmentID != e.ID {
				t.Fatalf("shared scope = %+v %v", scope, err)
			}
			if err := s.CreateInteraction(ctx, &models.Interaction{LearnerID: student.LearnerID, DomainID: d.ID, Concept: c.StableKey, ActivityType: "PRACTICE", Success: true}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.resolveLearningScope(ctx, student.LearnerID, d.ID, "invented"); err == nil {
			t.Fatal("unknown formation concept quarantined instead of rejected")
		}
		if err := s.UpdateDomainGraph(ctx, d.ID, models.KnowledgeSpace{Concepts: []string{"invented"}}); !errors.Is(err, storeport.ErrFormationDomainLocked) {
			t.Fatalf("graph edit = %v", err)
		}
		current, err := s.GetCurriculumSnapshot(ctx, student.LearnerID, d.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CompareAndSwapCurriculum(ctx, student.LearnerID, d.ID, current.Version, current); !errors.Is(err, storeport.ErrFormationDomainLocked) {
			t.Fatalf("curriculum edit = %v", err)
		}
		if err := s.DeleteDomain(ctx, d.ID, student.LearnerID); !errors.Is(err, storeport.ErrFormationDomainLocked) {
			t.Fatalf("formation history deleted: %v", err)
		}
	}
	report, err := s.GetCohortReport(ctx, owner, cohort.ID)
	if err != nil || report.EnrollmentCount != 2 || report.ActiveCount != 2 || report.AverageMastery < .699 || report.AverageMastery > .701 {
		t.Fatalf("report counts concepts instead of learners: %+v %v", report, err)
	}
	var enrollments, concepts int
	if err := s.queryRow(ctx, `SELECT COUNT(DISTINCT enrollment_id), COUNT(DISTINCT formation_concept_id) FROM interactions WHERE domain_id IN (?, ?)`, first.DomainID, "unused").Scan(&enrollments, &concepts); err != nil || enrollments != 1 || concepts != 2 {
		t.Fatalf("observations have incorrect catalogue dimensions: %d %d %v", enrollments, concepts, err)
	}
	if _, err := s.EnrollMembership(ctx, owner, cohort.ID, students[0].MembershipID, "{}"); err == nil {
		t.Fatal("duplicate enrollment accepted")
	}
	var seats int
	if err := s.queryRow(ctx, `SELECT reserved_seats FROM cohorts WHERE tenant_id = ? AND id = ?`, owner.TenantID, cohort.ID).Scan(&seats); err != nil || seats != 2 {
		t.Fatalf("failed enrollment consumed a seat: %d %v", seats, err)
	}
	if status, err := s.RemoveFormation(ctx, owner, detail.Formation.ID); err != nil || status != "archived" {
		t.Fatalf("archive = %s %v", status, err)
	}
	if _, err := s.EnrollMembership(ctx, owner, cohort.ID, formationStudent(t, s, "late@bridge.test").MembershipID, "{}"); err == nil {
		t.Fatal("archived formation accepted enrollment")
	}
	if _, err := s.resolveLearningScope(ctx, students[0].LearnerID, first.DomainID, "numbers"); err != nil {
		t.Fatalf("archive broke existing enrollment: %v", err)
	}
}

func TestFormationMigrationPreservesHistoryAndReconcilesDefinitions(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	student := formationStudent(t, s, "migration@bridge.test")
	source, err := s.EnrollMembership(ctx, owner, cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"numbers", "equations"} {
		state := models.NewConceptStateInDomain(student.LearnerID, source.DomainID, key)
		state.PMastery, state.Reps = .9, 8
		if err := s.UpsertConceptState(ctx, state); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateInteraction(ctx, &models.Interaction{LearnerID: student.LearnerID, DomainID: source.DomainID, Concept: key, ActivityType: "PRACTICE", Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	clone, err := s.CloneFormationVersion(ctx, owner, detail.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	var changed models.FormationConceptInput
	for _, c := range clone.Concepts {
		if c.StableKey == "equations" {
			changed = c.FormationConceptInput
		}
	}
	changed.Description = "Solve nonlinear equations"
	if _, _, err := s.AddFormationConceptsIdempotent(ctx, owner, "changed-definition", clone.Version.ID, nil, []models.FormationConceptInput{changed}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, clone.Version.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetDomainByID(ctx, source.DomainID)
	if before.FormationVersionID != detail.Version.ID {
		t.Fatal("publication migrated an enrollment")
	}
	targetCohort, err := s.CreateCohort(ctx, owner, clone.Version.ID, "October", 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.MigrateFormationEnrollment(ctx, owner, source.ID, targetCohort.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Enrollment.ID == source.ID || out.Enrollment.DomainID != source.DomainID || len(out.InvalidatedConceptKeys) != 1 || out.InvalidatedConceptKeys[0] != "equations" {
		t.Fatalf("migration = %+v", out)
	}
	for _, key := range []string{"numbers", "equations"} {
		state, err := s.GetConceptStateInDomain(ctx, student.LearnerID, source.DomainID, key)
		if err != nil {
			t.Fatal(err)
		}
		want := .9
		if key == "equations" {
			want = .1
		}
		if state.PMastery != want {
			t.Fatalf("%s mastery = %v, want %v", key, state.PMastery, want)
		}
		var enrollmentID string
		var invalidation int
		if err := s.queryRow(ctx, `SELECT enrollment_id, curriculum_invalidated_version FROM interactions WHERE domain_id = ? AND concept = ?`, source.DomainID, key).Scan(&enrollmentID, &invalidation); err != nil {
			t.Fatal(err)
		}
		if enrollmentID != source.ID || (invalidation > 0) != (key == "equations") {
			t.Fatalf("history rewritten: %s %d", enrollmentID, invalidation)
		}
	}
	replay, err := s.MigrateFormationEnrollment(ctx, owner, source.ID, targetCohort.ID)
	if err != nil || !reflect.DeepEqual(replay, out) {
		t.Fatalf("migration replay = %+v %v", replay, err)
	}
	var seats int
	if err := s.queryRow(ctx, `SELECT SUM(reserved_seats) FROM cohorts WHERE tenant_id = ? AND id IN (?, ?)`, owner.TenantID, cohort.ID, targetCohort.ID).Scan(&seats); err != nil || seats != 1 {
		t.Fatalf("seat movement = %d %v", seats, err)
	}
}

func TestFormationAssignmentsRecheckedOnReplayAndReads(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	manager := formationStudent(t, s, "manager@bridge.test")
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
	f, v, err := s.CreateFormationDraft(ctx, owner, "Private draft", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFormationVersion(ctx, manager, v.ID); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("unassigned read = %v", err)
	}
	if err := s.AssignFormationTrainer(ctx, owner, f.ID, manager.MembershipID, true); err != nil {
		t.Fatal(err)
	}
	input := models.FormationModuleInput{StableKey: "main", Title: "Main"}
	if _, _, err := s.AddFormationModuleIdempotent(ctx, manager, "assigned-module", v.ID, input); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignFormationTrainer(ctx, owner, f.ID, manager.MembershipID, false); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := s.AddFormationModuleIdempotent(ctx, manager, "assigned-module", v.ID, input); replayed || !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("revoked assignment replay = %v %v", replayed, err)
	}
	if page, err := s.ListFormations(ctx, manager, "", 10); err != nil || len(page.Items) != 0 {
		t.Fatalf("unassigned list = %+v %v", page, err)
	}
	if status, err := s.RemoveFormation(ctx, owner, f.ID); err != nil || status != "deleted" {
		t.Fatalf("draft deletion = %s %v", status, err)
	}
}

func TestStaffOAuthWithoutLearnerProfile(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	scope := seedInstitutionOwner(t, s, "staff-only", "staff@bridge.test")
	owner, _ := verifiedOwner(t, s, scope)
	principal, err := s.GetPrincipal(ctx, scope, []string{models.OAuthScopeFormationRead, models.OAuthScopeFormationWrite})
	if err != nil || principal.LearnerID != "" {
		t.Fatalf("staff principal = %+v %v", principal, err)
	}
	if err := s.CreateOAuthClient(ctx, "staff-client", "Staff", `["https://client.test/callback"]`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAuthCodeForPrincipal(ctx, "staff-code", principal, "challenge", "S256", "staff-client", "https://client.test/callback", testOAuthResource, "formation:read formation:write", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAuthCode(ctx, "staff-code", "staff-client"); err != nil {
		t.Fatal(err)
	}
	_, rt, err := s.ExchangeAuthCodeForRefreshToken(ctx, "staff-code", "staff-client")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRefreshToken(ctx, rt.Token); err != nil {
		t.Fatal(err)
	}
	next, err := s.RotateRefreshTokenWithScope(ctx, rt.Token, "staff-client", testOAuthResource, models.OAuthScopeFormationRead)
	if err != nil || next.LearnerID != "" {
		t.Fatalf("staff refresh = %+v %v", next, err)
	}
	if _, err := s.RotateRefreshTokenWithScope(ctx, next.Token, "staff-client", testOAuthResource, models.OAuthScopeFormationWrite); err == nil {
		t.Fatal("refresh widened formation rights")
	}
	if err := s.ValidatePrincipal(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, scope, models.MembershipStatusSuspended, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidatePrincipal(ctx, principal); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("suspended staff accepted: %v", err)
	}
	if _, err := s.RotateRefreshTokenWithScope(ctx, next.Token, "staff-client", testOAuthResource, ""); err == nil {
		t.Fatal("suspended membership rotated a token")
	}
}

func TestFormationRightsAndFreeDomainPolicy(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	student := formationStudent(t, s, "rights@bridge.test")
	manager := reviewWriter(reviewRoleForTest(t, s, "other-formation-manager", models.RolePedagogyManager))
	detail, cohort := seedFormationBridge(t, s, owner)
	enrollment, err := s.EnrollMembership(ctx, owner, cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []models.Principal{student, manager} {
		if _, err := s.GetFormationVersion(ctx, actor, detail.Version.ID); err == nil {
			t.Fatal("unassigned actor read version")
		}
		if _, err := s.PublishFormationVersion(ctx, actor, detail.Version.ID); err == nil {
			t.Fatal("unassigned actor published")
		}
		if _, err := s.GetCohortReport(ctx, actor, cohort.ID); err == nil {
			t.Fatal("unassigned actor read report")
		}
		if _, err := s.GetCurriculumReviewMaterial(ctx, actor, enrollment.DomainID, 1); err == nil {
			t.Fatal("unassigned actor read curriculum review")
		}
	}
	if err := s.AssignFormationTrainer(ctx, owner, detail.Formation.ID, manager.MembershipID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCurriculumReviewMaterial(ctx, manager, enrollment.DomainID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignFormationTrainer(ctx, owner, detail.Formation.ID, manager.MembershipID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCurriculumReviewMaterial(ctx, manager, enrollment.DomainID, 1); err == nil {
		t.Fatal("revoked assignment still reads reviews")
	}
	if err := s.SetInstitutionLearningPolicy(ctx, manager, true); err == nil {
		t.Fatal("manager changed tenant policy")
	}
	if err := s.SetInstitutionLearningPolicy(ctx, owner, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDomain(ctx, student.LearnerID, "Unapproved", "", models.KnowledgeSpace{Concepts: []string{"x"}}); err == nil {
		t.Fatal("disabled free domain created")
	}
	if err := s.SetInstitutionLearningPolicy(ctx, owner, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDomain(ctx, student.LearnerID, "Approved", "", models.KnowledgeSpace{Concepts: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
}

func TestFormationInvalidBatchAndFullTargetRollback(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	student := formationStudent(t, s, "rollback@bridge.test")
	source, err := s.EnrollMembership(ctx, owner, cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	clone, err := s.CloneFormationVersion(ctx, owner, detail.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.AddFormationConceptsIdempotent(ctx, owner, "cycle", clone.Version.ID, nil, []models.FormationConceptInput{{StableKey: "numbers", Label: "Numbers", Prerequisites: []string{"equations"}}})
	if err == nil {
		t.Fatal("cyclic curriculum accepted")
	}
	after, err := s.GetFormationVersion(ctx, owner, clone.Version.ID)
	if err != nil || !reflect.DeepEqual(after.Concepts, clone.Concepts) {
		t.Fatal("invalid batch partially rewrote the draft")
	}
	if _, err := s.PublishFormationVersion(ctx, owner, clone.Version.ID); err != nil {
		t.Fatal(err)
	}
	target, err := s.CreateCohort(ctx, owner, clone.Version.ID, "Full target", 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := formationStudent(t, s, "occupant@bridge.test")
	if _, err := s.EnrollMembership(ctx, owner, target.ID, other.MembershipID, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateFormationEnrollment(ctx, owner, source.ID, target.ID); !errors.Is(err, storeport.ErrCohortCapacityReached) {
		t.Fatalf("full target: %v", err)
	}
	domain, err := s.GetDomainByID(ctx, source.DomainID)
	if err != nil || domain.FormationEnrollmentID != source.ID || domain.FormationVersionID != detail.Version.ID || domain.GraphVersion != 1 {
		t.Fatalf("failed migration changed source: %+v %v", domain, err)
	}
}
