// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"errors"
	"fmt"
	"testing"
	"time"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func progressReader(p models.Principal) models.Principal {
	p.Scopes = []string{models.OAuthScopeProgressRead}
	return p
}

func TestInstitutionProgressEvidencePaginationAndThreshold(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	reader := progressReader(owner)
	var first *models.Enrollment
	var lastStudent models.Principal
	for i := 0; i < 5; i++ {
		student := formationStudent(t, s, fmt.Sprintf("student%d@progress.test", i))
		lastStudent = student
		e, err := s.EnrollMembership(ctx, owner, cohort.ID, student.MembershipID, "{}")
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = e
			empty, err := s.GetLearnerProgress(ctx, reader, e.ID, "", 1)
			if err != nil || empty.Learner.AverageMastery != nil || empty.Learner.ObservedConceptCount != 0 || len(empty.Concepts) != 1 || empty.Concepts[0].Mastery != nil || empty.NextAfter == "" {
				t.Fatalf("empty evidence: %+v %v", empty, err)
			}
			page, err := s.GetLearnerProgress(ctx, reader, e.ID, empty.NextAfter, 1)
			if err != nil || len(page.Concepts) != 1 || page.Concepts[0].ConceptID == empty.Concepts[0].ConceptID || page.NextAfter != "" {
				t.Fatalf("concept pagination: %+v %v", page, err)
			}
		}
		state := models.NewConceptStateInDomain(student.LearnerID, e.DomainID, "numbers")
		state.PMastery, state.Reps = .9, 3
		if i == 0 {
			last := time.Now().UTC().Add(-15 * 24 * time.Hour)
			state.LastReview = &last
		}
		if err := s.UpsertConceptState(ctx, state); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := s.OpenLearningSession(ctx, student.LearnerID, e.DomainID, "progress-session", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateInteraction(ctx, &models.Interaction{LearnerID: student.LearnerID, DomainID: e.DomainID, Concept: "numbers", ActivityType: "PRACTICE", Success: true}); err != nil {
				t.Fatal(err)
			}
		}
		out, err := s.GetCohortInsights(ctx, reader, cohort.ID, "", "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if out.Cohort.EnrollmentCount != i+1 || out.Cohort.ActiveCount != i+1 {
			t.Fatalf("counts: %+v", out.Cohort)
		}
		for _, c := range out.Concepts {
			if c.StableKey == "numbers" && (c.ObservedLearners != i+1 || (c.AverageMastery != nil) != (i == 4)) {
				t.Fatalf("threshold: %+v", c)
			}
			if c.StableKey == "equations" && (c.AverageMastery != nil || c.ObservedLearners != 0) {
				t.Fatalf("priors counted as evidence: %+v", c)
			}
		}
	}
	seen := map[string]bool{}
	after := ""
	for {
		out, err := s.GetCohortInsights(ctx, reader, cohort.ID, after, "", 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range out.Learners {
			if seen[item.EnrollmentID] {
				t.Fatal("duplicate pagination")
			}
			seen[item.EnrollmentID] = true
		}
		after = out.NextAfter
		if after == "" {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("missing learners: %v", seen)
	}
	one, err := s.GetLearnerProgress(ctx, reader, first.ID, "", 100)
	if err != nil || one.Learner.ObservedConceptCount != 1 || one.Learner.MasteredConceptCount != 1 || one.Learner.AverageMastery == nil || *one.Learner.AverageMastery != .9 || one.InteractionCount != 1 || len(one.RecentSessions) != 1 {
		t.Fatalf("learner evidence: %+v %v", one, err)
	}
	if one.Cohort.VersionID != detail.Version.ID || one.SynthesisGuidance == "" {
		t.Fatal("missing provenance/guidance")
	}
	if one.Learner.LastReviewAt == nil || len(one.Learner.AttentionSignals) != 1 || one.Learner.AttentionSignals[0] != "no_review_in_14_days" {
		t.Fatalf("review dates: %+v", one.Learner)
	}
	if _, _, err := s.LeaveFormation(ctx, lastStudent, "progress-leave", cohort.ID); err != nil {
		t.Fatal(err)
	}
	afterLeave, err := s.GetCohortInsights(ctx, reader, cohort.ID, "", "", 10)
	if err != nil || afterLeave.Cohort.ActiveCount != 4 || afterLeave.Cohort.EnrollmentCount != 5 {
		t.Fatalf("left counts: %+v %v", afterLeave, err)
	}
	for _, concept := range afterLeave.Concepts {
		if concept.AverageMastery != nil {
			t.Fatal("cancelled learner contributed to small cohort aggregate")
		}
	}
}

func TestInstitutionProgressAuthorizationAndRevocation(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	_, other := seedFormationBridge(t, s, owner)
	student := formationStudent(t, s, "one@progress.test")
	e, err := s.EnrollMembership(ctx, owner, cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	trainer := progressReader(reviewRoleForTest(t, s, "progress-trainer", models.RoleTrainer))
	if err := s.SetCohortTrainerAssignment(ctx, owner, cohort.ID, "progress-trainer@test.com", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCohortTrainerAssignment(ctx, owner, cohort.ID, "progress-trainer@test.com", true); err != nil {
		t.Fatalf("assignment replay: %v", err)
	}
	if err := s.SetCohortTrainerAssignment(ctx, progressReader(owner), cohort.ID, "progress-trainer@test.com", false); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("read token changed assignment: %v", err)
	}
	page, err := s.ListTrainerCohorts(ctx, trainer, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].CohortID != cohort.ID || page.NextAfter != "" {
		t.Fatalf("assigned cohort: %+v %v", page, err)
	}
	if _, err := s.GetLearnerProgress(ctx, trainer, e.ID, "", 10); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{other.ID, "missing"} {
		if _, err := s.GetCohortInsights(ctx, trainer, id, "", "", 10); !errors.Is(err, storeport.ErrNotFound) {
			t.Fatalf("resource disclosure: %v", err)
		}
	}
	for _, grant := range []string{models.OAuthScopeLearner, models.OAuthScopeFormationRead} {
		bad := trainer
		bad.Scopes = []string{grant}
		if _, err := s.GetLearnerProgress(ctx, bad, e.ID, "", 10); !errors.Is(err, storeport.ErrInvalidPrincipal) {
			t.Fatalf("scope %s: %v", grant, err)
		}
	}
	if _, err := s.GetLearnerProgress(ctx, progressReader(student), e.ID, "", 10); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("learner staff read: %v", err)
	}
	manager := progressReader(reviewRoleForTest(t, s, "progress-manager", models.RolePedagogyManager))
	if _, err := s.GetCohortInsights(ctx, manager, cohort.ID, "", "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatal("unassigned manager read")
	}
	if err := s.AssignFormationTrainer(ctx, owner, detail.Formation.ID, manager.MembershipID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCohortInsights(ctx, manager, cohort.ID, "", "", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignFormationTrainer(ctx, owner, detail.Formation.ID, manager.MembershipID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLearnerProgress(ctx, manager, e.ID, "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatal("revoked manager read")
	}
	if err := s.SetCohortTrainerAssignment(ctx, owner, cohort.ID, "progress-trainer@test.com", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLearnerProgress(ctx, trainer, e.ID, "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatal("revoked trainer read")
	}
	if _, err := s.SetMembershipAuthorization(ctx, trainer.TenantScope(), models.MembershipStatusSuspended, []string{models.RoleTrainer}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListTrainerCohorts(ctx, trainer, "", 10); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("stale membership: %v", err)
	}
	if _, err := s.ListTrainerCohorts(ctx, progressReader(owner), "", 101); !errors.Is(err, storeport.ErrInvalidProgressRequest) {
		t.Fatal("unbounded page")
	}
}

func TestInstitutionProgressMigrationAndTenantIsolation(t *testing.T) {
	s := institutionTestStore(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	student := formationStudent(t, s, "migration@progress.test")
	e, err := s.EnrollMembership(ctx, owner, cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	trainer := progressReader(reviewRoleForTest(t, s, "migration-trainer", models.RoleTrainer))
	if err := s.AssignCohortTrainer(ctx, owner, cohort.ID, trainer.MembershipID); err != nil {
		t.Fatal(err)
	}
	v, err := s.CloneFormationVersion(ctx, owner, detail.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, v.Version.ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.CreateCohort(ctx, owner, v.Version.ID, "Next", 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := s.MigrateFormationEnrollment(ctx, owner, e.ID, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := models.NewConceptStateInDomain(student.LearnerID, migrated.Enrollment.DomainID, "numbers")
	state.Reps, state.PMastery = 8, .99
	if err := s.UpsertConceptState(ctx, state); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetLearnerProgress(ctx, trainer, e.ID, "", 10)
	if err != nil || old.Cohort.VersionID != detail.Version.ID || old.Learner.AverageMastery != nil {
		t.Fatalf("migration leaked new progress: %+v %v", old, err)
	}
	if _, err := s.GetLearnerProgress(ctx, trainer, migrated.Enrollment.ID, "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("new cohort leaked: %v", err)
	}
	// Quarantine already-contaminated source estimates from older servers too.
	if _, err := s.exec(ctx, `UPDATE learner_concept_states SET reps = 9, p_mastery = .99, updated_at = ? WHERE tenant_id = ? AND enrollment_id = ?`, time.Now().UTC(), owner.TenantID, e.ID); err != nil {
		t.Fatal(err)
	}
	old, err = s.GetLearnerProgress(ctx, trainer, e.ID, "", 10)
	if err != nil || old.Learner.AverageMastery != nil || old.Learner.ObservedConceptCount != 0 || !old.Learner.HistoricalEstimatesUnavailable {
		t.Fatalf("historical contamination exposed: %+v %v", old, err)
	}
	// Genuine memberships in a separate tenant, not a forged principal.
	otherOwner, _ := verifiedOwner(t, s, seedInstitutionOwner(t, s, "progress-foreign", "owner@foreign.test"))
	otherOwner = progressReader(otherOwner)
	if _, err := s.GetCohortInsights(ctx, otherOwner, cohort.ID, "", "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreign cohort: %v", err)
	}
	if _, err := s.GetLearnerProgress(ctx, otherOwner, e.ID, "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreign learner: %v", err)
	}
}
