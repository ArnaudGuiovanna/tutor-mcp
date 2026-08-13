// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"

	"tutor-mcp/models"
)

func TestCatalogAdminMutationsAreIdempotentAndConflictSafe(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	formation, version, replayed, err := s.CreateFormationDraftIdempotent(ctx, owner,
		"request-create-1", "Idempotent formation", "description")
	if err != nil || replayed {
		t.Fatalf("first create formation=%#v version=%#v replayed=%v err=%v", formation, version, replayed, err)
	}
	replayFormation, replayVersion, replayed, err := s.CreateFormationDraftIdempotent(ctx, owner,
		"request-create-1", "Idempotent formation", "description")
	if err != nil || !replayed || replayFormation.ID != formation.ID || replayVersion.ID != version.ID {
		t.Fatalf("replay formation=%#v version=%#v replayed=%v err=%v", replayFormation, replayVersion, replayed, err)
	}
	if _, _, _, err := s.CreateFormationDraftIdempotent(ctx, owner,
		"request-create-1", "Different payload", "description"); err == nil {
		t.Fatal("idempotency key reused with another payload")
	}
	moduleID, replayed, err := s.AddFormationModuleIdempotent(ctx, owner, "request-module-1",
		version.ID, models.FormationModuleInput{StableKey: "module", Title: "Module"})
	if err != nil || replayed || moduleID == "" {
		t.Fatalf("module id=%q replayed=%v err=%v", moduleID, replayed, err)
	}
	replayModuleID, replayed, err := s.AddFormationModuleIdempotent(ctx, owner, "request-module-1",
		version.ID, models.FormationModuleInput{StableKey: "module", Title: "Module"})
	if err != nil || !replayed || replayModuleID != moduleID {
		t.Fatalf("module replay id=%q replayed=%v err=%v", replayModuleID, replayed, err)
	}
	conceptID, _, err := s.AddFormationConceptIdempotent(ctx, owner, "request-concept-1",
		version.ID, models.FormationConceptInput{ModuleStableKey: "module", StableKey: "concept", Label: "Concept"})
	if err != nil || conceptID == "" {
		t.Fatal(err)
	}
	published, replayed, err := s.PublishFormationVersionIdempotent(ctx, owner, "request-publish-1", version.ID)
	if err != nil || replayed || published.Status != "published" {
		t.Fatalf("published=%#v replayed=%v err=%v", published, replayed, err)
	}
	replayedPublished, replayed, err := s.PublishFormationVersionIdempotent(ctx, owner, "request-publish-1", version.ID)
	if err != nil || !replayed || replayedPublished.ID != published.ID {
		t.Fatalf("publish replay=%#v replayed=%v err=%v", replayedPublished, replayed, err)
	}
	var versionCount, mutationCount int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM formation_versions WHERE tenant_id = ? AND formation_id = ?`,
		owner.TenantID, formation.ID).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM catalog_admin_mutations WHERE tenant_id = ?`,
		owner.TenantID).Scan(&mutationCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 || mutationCount != 4 {
		t.Fatalf("version count=%d mutation count=%d", versionCount, mutationCount)
	}
}

func TestCatalogAdminPaginationEnrollmentAndReport(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	for i, name := range []string{"One", "Two", "Three"} {
		if _, _, _, err := s.CreateFormationDraftIdempotent(ctx, owner,
			"page-formation-"+name, name, ""); err != nil {
			t.Fatalf("formation %d: %v", i, err)
		}
	}
	first, err := s.ListFormations(ctx, owner, "", 2)
	if err != nil || len(first.Items) != 2 || first.NextAfter == "" {
		t.Fatalf("first page=%#v err=%v", first, err)
	}
	second, err := s.ListFormations(ctx, owner, first.NextAfter, 2)
	if err != nil || len(second.Items) < 1 || second.NextAfter != "" {
		t.Fatalf("second page=%#v err=%v", second, err)
	}

	_, version, err := s.CreateFormationDraft(ctx, owner, "Report formation", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationModule(ctx, owner, version.ID, models.FormationModuleInput{StableKey: "m", Title: "M"}); err != nil {
		t.Fatal(err)
	}
	conceptID, err := s.AddFormationConcept(ctx, owner, version.ID, models.FormationConceptInput{ModuleStableKey: "m", StableKey: "c", Label: "C"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, version.ID); err != nil {
		t.Fatal(err)
	}
	cohort, replayed, err := s.CreateCohortIdempotent(ctx, owner, "report-cohort", version.ID, "Report", 2, nil, nil)
	if err != nil || replayed {
		t.Fatalf("cohort=%#v replay=%v err=%v", cohort, replayed, err)
	}
	enrollment, replayed, err := s.EnrollMembershipIdempotent(ctx, owner, "report-enrollment", cohort.ID, owner.MembershipID, `{}`)
	if err != nil || replayed {
		t.Fatalf("enrollment=%#v replay=%v err=%v", enrollment, replayed, err)
	}
	scope := owner.TenantScope()
	scope.EnrollmentID = enrollment.ID
	state := models.NewConceptStateInDomain(owner.LearnerID, "", "C")
	state.PMastery = 0.8
	if err := s.UpsertEnrollmentConceptState(ctx, scope, conceptID, state); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListCohortEnrollments(ctx, owner, cohort.ID, "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != enrollment.ID {
		t.Fatalf("enrollments=%#v err=%v", page, err)
	}
	report, err := s.GetCohortReport(ctx, owner, cohort.ID)
	if err != nil || report.EnrollmentCount != 1 || report.ActiveCount != 1 || report.AverageMastery != 0.8 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}
