// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func createPublishedEnrollmentConcept(t *testing.T, s *Store, owner models.Principal, name string) (*models.Enrollment, string) {
	t.Helper()
	ctx := context.Background()
	_, version, err := s.CreateFormationDraft(ctx, owner, name, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationModule(ctx, owner, version.ID, models.FormationModuleInput{
		StableKey: "shared-module", Title: "Shared module",
	}); err != nil {
		t.Fatal(err)
	}
	conceptID, err := s.AddFormationConcept(ctx, owner, version.ID, models.FormationConceptInput{
		ModuleStableKey: "shared-module", StableKey: "shared-concept", Label: "Shared concept",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, version.ID); err != nil {
		t.Fatal(err)
	}
	cohort, err := s.CreateCohort(ctx, owner, version.ID, name+" cohort", 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := s.EnrollMembership(ctx, owner, cohort.ID, owner.MembershipID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	return enrollment, conceptID
}

func TestEnrollmentConceptStateNeverMixesTwoEnrollments(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	enrollmentA, conceptA := createPublishedEnrollmentConcept(t, s, owner, "Formation A")
	enrollmentB, conceptB := createPublishedEnrollmentConcept(t, s, owner, "Formation B")

	scopeA := owner.TenantScope()
	scopeA.EnrollmentID = enrollmentA.ID
	scopeB := owner.TenantScope()
	scopeB.EnrollmentID = enrollmentB.ID
	stateA := models.NewConceptStateInDomain(owner.LearnerID, "", "shared-concept")
	stateA.PMastery = 0.2
	stateB := models.NewConceptStateInDomain(owner.LearnerID, "", "shared-concept")
	stateB.PMastery = 0.9
	if err := s.UpsertEnrollmentConceptState(ctx, scopeA, conceptA, stateA); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEnrollmentConceptState(ctx, scopeB, conceptB, stateB); err != nil {
		t.Fatal(err)
	}
	gotA, err := s.GetEnrollmentConceptState(ctx, scopeA, conceptA)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := s.GetEnrollmentConceptState(ctx, scopeB, conceptB)
	if err != nil {
		t.Fatal(err)
	}
	if gotA.PMastery != 0.2 || gotB.PMastery != 0.9 ||
		gotA.EnrollmentID == gotB.EnrollmentID || gotA.FormationConceptID == gotB.FormationConceptID {
		t.Fatalf("states contaminated: A=%#v B=%#v", gotA, gotB)
	}
	if _, err := s.GetEnrollmentConceptState(ctx, scopeA, conceptB); err == nil {
		t.Fatal("concept from another formation version accepted in enrollment A")
	}

	// Direct SQL cannot bypass the enrollment/version/concept relation either.
	err = s.WithTenantTx(ctx, scopeA, func(txCtx context.Context, scoped storeport.Store) error {
		_, err := scoped.(*Store).exec(txCtx, `UPDATE learner_concept_states
			SET formation_concept_id = ? WHERE tenant_id = ? AND enrollment_id = ?
			  AND formation_concept_id = ?`, conceptB, owner.TenantID, enrollmentA.ID, conceptA)
		return err
	})
	if err == nil {
		t.Fatal("cross-version concept relation accepted by SQL")
	}
}

func TestLegacyConceptStateDualWritesCanonicalEnrollmentState(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	domain, err := s.CreateDomain(ctx, "L1", "Legacy bridge", "", models.KnowledgeSpace{
		Concepts: []string{"bridge-concept"}, Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := models.NewConceptStateInDomain("L1", domain.ID, "bridge-concept")
	state.PMastery = 0.73
	state.UpdatedAt = time.Now().UTC()
	if err := s.UpsertConceptState(ctx, state); err != nil {
		t.Fatal(err)
	}
	var count int
	var mastery float64
	if err := s.queryRow(ctx, `SELECT COUNT(*), COALESCE(MAX(p_mastery), 0)
		FROM learner_concept_states WHERE tenant_id = ? AND learner_id = ?
		  AND legacy_domain_id = ? AND legacy_concept = ?`, models.LegacyTenantID,
		"L1", domain.ID, "bridge-concept").Scan(&count, &mastery); err != nil {
		t.Fatal(err)
	}
	if count != 1 || mastery != 0.73 {
		t.Fatalf("canonical legacy bridge count=%d mastery=%v", count, mastery)
	}
}
