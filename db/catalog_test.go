// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestFormationPublicationIsAtomicAndImmutable(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	formation, version, err := s.CreateFormationDraft(ctx, owner, "Algebra", "Foundations")
	if err != nil {
		t.Fatal(err)
	}
	if formation.TenantID != owner.TenantID || version.Status != "draft" {
		t.Fatalf("formation/version = %#v / %#v", formation, version)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, version.ID); err == nil {
		t.Fatal("empty formation version published")
	}
	if _, err := s.AddFormationModule(ctx, owner, version.ID, models.FormationModuleInput{
		StableKey: "module-1", Title: "Module 1", Position: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationConcept(ctx, owner, version.ID, models.FormationConceptInput{
		ModuleStableKey: "module-1", StableKey: "linear", Label: "Linear equations", Position: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationConcept(ctx, owner, version.ID, models.FormationConceptInput{
		ModuleStableKey: "module-1", StableKey: "quadratic", Label: "Quadratics", Position: 1,
		Prerequisites: []string{"linear"},
	}); err != nil {
		t.Fatal(err)
	}
	published, err := s.PublishFormationVersion(ctx, owner, version.ID)
	if err != nil || published.Status != "published" || published.PublishedAt == nil {
		t.Fatalf("publish=%#v err=%v", published, err)
	}
	if _, err := s.AddFormationModule(ctx, owner, version.ID, models.FormationModuleInput{
		StableKey: "late", Title: "Late", Position: 2,
	}); !errors.Is(err, storeport.ErrFormationVersionImmutable) {
		t.Fatalf("add after publication = %v, want immutable", err)
	}
	if _, err := s.root.Exec(`UPDATE formation_concepts SET label = 'tampered' WHERE stable_key = 'linear'`); err == nil {
		t.Fatal("published concept updated directly")
	}
	if _, err := s.root.Exec(`INSERT INTO formation_modules
        (id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
        VALUES ('late-direct', 'tenant_legacy', ?, 'late-direct', 'late', 3, '{}')`, version.ID); err == nil {
		t.Fatal("content inserted directly into published version")
	}
}

func TestCohortCapacityTrainerScopeAndTenantCompositeFKs(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	_, version, err := s.CreateFormationDraft(ctx, owner, "Course", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationModule(ctx, owner, version.ID, models.FormationModuleInput{StableKey: "m", Title: "M"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFormationConcept(ctx, owner, version.ID, models.FormationConceptInput{ModuleStableKey: "m", StableKey: "c", Label: "C"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, version.ID); err != nil {
		t.Fatal(err)
	}
	cohort, err := s.CreateCohort(ctx, owner, version.ID, "September", 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedMembership := func(id, role string) {
		t.Helper()
		if _, err := s.exec(ctx, `INSERT INTO users
            (id, email, normalized_email, password_hash, status, email_verified_at,
             token_version, created_at, updated_at)
            VALUES (?, ?, ?, 'hash', 'active', ?, 1, ?, ?)`, id, id+"@example.com", id+"@example.com", now, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.exec(ctx, `INSERT INTO tenant_memberships
            (id, tenant_id, user_id, roles_json, status, version, created_at, updated_at)
            VALUES (?, ?, ?, ?, 'active', 1, ?, ?)`, "membership-"+id, owner.TenantID,
			id, `["`+role+`"]`, now, now); err != nil {
			t.Fatal(err)
		}
	}
	seedMembership("trainer", models.RoleTrainer)
	seedMembership("student-a", models.RoleLearner)
	seedMembership("student-b", models.RoleLearner)
	if err := s.AssignCohortTrainer(ctx, owner, cohort.ID, "membership-trainer"); err != nil {
		t.Fatal(err)
	}
	first, err := s.EnrollMembership(ctx, owner, cohort.ID, "membership-student-a", `{"goal":"pass"}`)
	if err != nil || first.FormationVersionID != version.ID {
		t.Fatalf("first enrollment=%#v err=%v", first, err)
	}
	if _, err := s.EnrollMembership(ctx, owner, cohort.ID, "membership-student-b", `{}`); !errors.Is(err, storeport.ErrCohortCapacityReached) {
		t.Fatalf("over-capacity enrollment = %v", err)
	}
	var reserved int
	if err := s.queryRow(ctx, `SELECT reserved_seats FROM cohorts WHERE tenant_id = ? AND id = ?`, owner.TenantID, cohort.ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 1 {
		t.Fatalf("reserved seats = %d, want 1", reserved)
	}

	if _, err := s.exec(ctx, `INSERT INTO tenants
        (id, slug, name, status, region, policy_json, created_at, updated_at)
        VALUES ('tenant-x', 'tenant-x', 'X', 'active', 'default', '{}', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.root.Exec(`INSERT INTO formation_modules
        (id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
        VALUES ('cross', 'tenant-x', ?, 'cross', 'Cross', 0, '{}')`, version.ID); err == nil {
		t.Fatal("cross-tenant module/version relation accepted")
	}
}
