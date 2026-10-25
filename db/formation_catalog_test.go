// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"tutor-mcp/models"
)

func TestFormationCatalogReviewAndVersionDiscovery(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerLearnerPrincipal(t, s)
	f, v, err := s.CreateFormationDraft(ctx, owner, "Confitures au XIIe siècle", "")
	if err != nil {
		t.Fatal(err)
	}
	review, err := s.ReviewFormationVersion(ctx, owner, v.ID)
	if err != nil || review.Publishable || len(review.Issues) == 0 {
		t.Fatalf("empty draft review=%#v err=%v", review, err)
	}
	page, err := s.ListFormationVersions(ctx, owner, f.ID, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != v.ID {
		t.Fatalf("versions=%#v err=%v", page, err)
	}
	if _, err = s.GetFormationVersion(ctx, owner, "an-intention-id"); err == nil {
		t.Fatal("an intention was accepted as a version")
	}
	if _, err = s.AddFormationModule(ctx, owner, v.ID, models.FormationModuleInput{StableKey: "m", Title: "Histoire"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddFormationConcept(ctx, owner, v.ID, models.FormationConceptInput{ModuleStableKey: "m", StableKey: "fruit", Label: "Conservation des fruits"}); err != nil {
		t.Fatal(err)
	}
	review, err = s.ReviewFormationVersion(ctx, owner, v.ID)
	if err != nil || !review.Publishable {
		t.Fatalf("complete draft review=%#v err=%v", review, err)
	}
	clone, err := s.CloneFormationVersion(ctx, owner, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ListFormationVersions(ctx, owner, f.ID, "", 1)
	if err != nil || first.Items[0].ID != clone.Version.ID || first.NextAfter != "2" {
		t.Fatalf("newest page=%#v err=%v", first, err)
	}
	second, err := s.ListFormationVersions(ctx, owner, f.ID, first.NextAfter, 1)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != v.ID || second.NextAfter != "" {
		t.Fatalf("older page=%#v err=%v", second, err)
	}
	other := owner
	other.TenantID = "tenant-other"
	if _, err = s.ListFormationVersions(ctx, other, f.ID, "", 20); err == nil {
		t.Fatal("cross-tenant version disclosure")
	}
}

func TestFormationCatalogPublicationIsAtomicVisibleAndDoesNotEnroll(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerLearnerPrincipal(t, s)
	var baselineEnrollments int
	if err := s.queryRow(ctx, `SELECT count(*) FROM enrollments WHERE tenant_id=?`, owner.TenantID).Scan(&baselineEnrollments); err != nil {
		t.Fatal(err)
	}
	f, v, err := s.CreateFormationDraft(ctx, owner, "Ouverte aux membres", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PublishFormationToCatalogIdempotent(ctx, owner, "catalog-incomplete", v.ID); err == nil {
		t.Fatal("published an empty draft")
	}
	if _, err = s.AddFormationModule(ctx, owner, v.ID, models.FormationModuleInput{StableKey: "m", Title: "Module"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddFormationConcept(ctx, owner, v.ID, models.FormationConceptInput{ModuleStableKey: "m", StableKey: "c", Label: "Concept"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetFormationEnrollmentPolicy(ctx, owner, f.ID, "open"); err != nil {
		t.Fatal(err)
	}
	publication, replayed, err := s.PublishFormationToCatalogIdempotent(ctx, owner, "catalog-publish", v.ID)
	if err != nil || replayed || publication.Version.Status != "published" || publication.Cohort.Status != "open" {
		t.Fatalf("publication=%#v replay=%v err=%v", publication, replayed, err)
	}
	replay, replayed, err := s.PublishFormationToCatalogIdempotent(ctx, owner, "catalog-publish", v.ID)
	if err != nil || !replayed || replay.Cohort.ID != publication.Cohort.ID {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	again, _, err := s.PublishFormationToCatalogIdempotent(ctx, owner, "catalog-again", v.ID)
	if err != nil || again.Cohort.ID != publication.Cohort.ID {
		t.Fatalf("duplicate offering=%#v err=%v", again, err)
	}
	var policy string
	var enrollments, cohorts int
	if err = s.queryRow(ctx, `SELECT enrollment_policy FROM formations WHERE id=?`, f.ID).Scan(&policy); err != nil {
		t.Fatal(err)
	}
	if err = s.queryRow(ctx, `SELECT count(*) FROM enrollments WHERE tenant_id=?`, owner.TenantID).Scan(&enrollments); err != nil {
		t.Fatal(err)
	}
	if err = s.queryRow(ctx, `SELECT count(*) FROM cohorts WHERE tenant_id=? AND formation_version_id=?`, owner.TenantID, v.ID).Scan(&cohorts); err != nil {
		t.Fatal(err)
	}
	if policy != "open" || enrollments != baselineEnrollments || cohorts != 1 {
		t.Fatalf("policy=%s enrollments=%d offerings=%d", policy, enrollments, cohorts)
	}
	available, err := s.ListLearnerFormations(ctx, owner, false, "", 20)
	if err != nil || len(available.Items) != 1 || available.Items[0].VersionID != v.ID {
		t.Fatalf("learner catalogue=%#v err=%v", available, err)
	}
	student := formationStudent(t, s, "catalog-student@formation.test")
	joined, _, err := s.JoinFormation(ctx, student, "catalog-student-join", publication.Cohort.ID)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := s.CloneFormationVersion(ctx, owner, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	newer, _, err := s.PublishFormationToCatalogIdempotent(ctx, owner, "catalog-new-version", clone.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	available, err = s.ListLearnerFormations(ctx, owner, false, "", 20)
	if err != nil || len(available.Items) != 1 || available.Items[0].VersionID != newer.Version.ID {
		t.Fatalf("latest catalogue=%#v err=%v", available, err)
	}
	mine, err := s.ListLearnerFormations(ctx, student, true, "", 20)
	if err != nil || len(mine.Items) != 1 || mine.Items[0].VersionID != v.ID {
		t.Fatalf("existing learner migrated: %#v %v", mine, err)
	}
	state := models.NewConceptStateInDomain(student.LearnerID, joined.Enrollment.DomainID, "c")
	state.PMastery = .7
	if err = s.UpsertConceptState(ctx, state); err != nil {
		t.Fatal("new publication blocked existing learning", err)
	}
}
