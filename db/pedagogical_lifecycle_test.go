// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"tutor-mcp/models"
)

func pedagogicalLifecycleFixture(t *testing.T, s *Store, owner models.Principal, now time.Time) *models.PedagogicalDecision {
	t.Helper()
	ctx := context.Background()
	domain, err := s.CreateDomain(ctx, owner.LearnerID, "lifecycle", "practice", models.KnowledgeSpace{Concepts: []string{"A"}})
	if err != nil {
		t.Fatal(err)
	}
	curriculum, err := s.EnsureCurriculumBaseline(ctx, owner.LearnerID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.OpenLearningSession(ctx, owner.LearnerID, domain.ID, "", now)
	if err != nil {
		t.Fatal(err)
	}
	return &models.PedagogicalDecision{
		LearnerID: owner.LearnerID, DomainID: domain.ID, SessionID: session.ID,
		CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion,
		Contract: models.PedagogicalContract{
			TargetConcept: "A", RecommendedActivityType: models.ActivityPractice,
			CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion,
			Competency: &curriculum.Concepts[0],
		},
	}
}

func createLifecycleDecision(t *testing.T, s *Store, owner models.Principal, template *models.PedagogicalDecision, id string, createdAt time.Time) *models.PedagogicalDecision {
	t.Helper()
	decision := *template
	decision.ID, decision.Contract.DecisionID, decision.CreatedAt = id, id, createdAt
	if err := s.CreatePedagogicalDecision(context.Background(), owner.TenantScope(), &decision); err != nil {
		t.Fatal(err)
	}
	return &decision
}

func createLifecycleAssessment(t *testing.T, s *Store, decision *models.PedagogicalDecision) {
	t.Helper()
	competency, err := json.Marshal(decision.Contract.Competency)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAssessmentAttempt(context.Background(), &models.AssessmentAttempt{
		ID: "attempt-" + decision.ID, LearnerID: decision.LearnerID, DomainID: decision.DomainID,
		ConceptID: decision.Contract.TargetConcept, SessionID: decision.SessionID,
		ActivityID: "activity-" + decision.ID, ActivityVersion: 1, ActivityType: string(decision.Contract.RecommendedActivityType),
		Observable: "application", TaskText: "generated task", TaskContentHash: "hash",
		RubricJSON: `{"criteria":[{"id":"correctness","description":"Correct application.","max_score":1}],"passing_score":0.7}`, PassingScore: .7,
		CreatedAt: decision.CreatedAt, DecisionID: decision.ID, CurriculumVersion: decision.CurriculumVersion,
		CurriculumConceptJSON: string(competency), OutcomeIDsJSON: "[]",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPedagogicalDecisionsExportedAndErasedBeforeSessions(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_checkpoint=%t", legacy), func(t *testing.T) { testPedagogicalDecisionDSAR(t, legacy) })
	}
}

func testPedagogicalDecisionDSAR(t *testing.T, legacy bool) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	now := time.Now().UTC()
	template := pedagogicalLifecycleFixture(t, s, owner, now)
	decision := createLifecycleDecision(t, s, owner, template, "decision-dsar", now)
	createLifecycleAssessment(t, s, decision)
	curriculum, err := s.GetCurriculumSnapshot(ctx, owner.LearnerID, decision.DomainID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwapCurriculum(ctx, owner.LearnerID, decision.DomainID, curriculum.Version, revisionForTest(curriculum)); err != nil {
		t.Fatal(err)
	}
	worker := models.WorkerPrincipal{ActorID: "pedagogical-lifecycle-worker"}
	scope := owner.TenantScope()
	scope.UserID, scope.MembershipID = "worker_"+worker.ActorID, "worker_process"
	export, err := s.RequestTenantDSAR(ctx, owner, owner.LearnerID, "export", "access request")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTenantDSARExport(ctx, scope, worker, export.ID); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.queryRow(ctx, `SELECT result_json FROM tenant_dsar_requests WHERE id = ?`, export.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Counts map[string]int64 `json:"counts"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Counts["pedagogical_decisions"] != 1 {
		t.Fatalf("decision missing from export manifest: %s", raw)
	}
	erasure, err := s.RequestTenantDSAR(ctx, owner, owner.LearnerID, "erase", "verified erasure request")
	if err != nil {
		t.Fatal(err)
	}
	if legacy {
		// Simulate a durable pre-upgrade request without the new checkpoint.
		if _, err := s.exec(ctx, `DELETE FROM tenant_dsar_phases WHERE tenant_id = ? AND request_id = ? AND phase = 'pedagogical_decisions'`, owner.TenantID, erasure.ID); err != nil {
			t.Fatal(err)
		}
	}
	complete := false
	for batch := 0; batch < 100 && !complete; batch++ {
		complete, _, err = s.ProcessTenantDSARErasureBatch(ctx, scope, worker, erasure.ID, 1, now)
		if err != nil {
			t.Fatalf("DSAR batch %d: %v", batch, err)
		}
	}
	if !complete {
		t.Fatal("DSAR erasure did not finish")
	}
	for _, table := range []string{"assessment_attempts", "pedagogical_decisions", "learning_sessions"} {
		if got := retentionCountTable(t, s, table); got != 0 {
			t.Fatalf("%s: %d learner rows remain after erasure", table, got)
		}
	}
}

func TestPedagogicalDecisionRetentionPreservesReferencedFreshAndHeld(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -60)
	template := pedagogicalLifecycleFixture(t, s, owner, now)
	createLifecycleDecision(t, s, owner, template, "decision-old-unused", old)
	referenced := createLifecycleDecision(t, s, owner, template, "decision-old-referenced", old)
	createLifecycleAssessment(t, s, referenced)
	createLifecycleDecision(t, s, owner, template, "decision-fresh", now)
	policy := RetentionPolicy{PedagogicalSnapshotDays: 30}
	if err := s.CreateRetentionLegalHold(ctx, RetentionLegalHold{
		HoldID: "pedagogical-hold", LearnerID: owner.LearnerID, Reason: "pending review", CreatedBy: "legal", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	held, err := s.RunDataRetention(ctx, policy, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if held.PedagogicalDecisions.Held != 1 || held.PedagogicalDecisions.Applied != 0 {
		t.Fatalf("legal hold did not preserve unused decision: %+v", held.PedagogicalDecisions)
	}
	if _, err := s.ReleaseRetentionLegalHold(ctx, "pedagogical-hold", "legal", "review closed", now); err != nil {
		t.Fatal(err)
	}
	dry, err := s.RunDataRetention(ctx, policy, now, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionMetric(t, "unused decisions dry-run", dry.PedagogicalDecisions, 1, 0)
	if got := retentionCountTable(t, s, "pedagogical_decisions"); got != 3 {
		t.Fatalf("dry-run mutated %d decisions", got)
	}
	result, err := s.RunDataRetention(ctx, policy, now, true)
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionMetric(t, "unused decisions applied", result.PedagogicalDecisions, 1, 1)
	eligible, applied, heldTotal := RetentionReportTotals(result)
	if eligible != 1 || applied != 1 || heldTotal != 0 {
		t.Fatalf("decision lifecycle missing from phase checkpoint totals: eligible=%d applied=%d held=%d", eligible, applied, heldTotal)
	}
	if got := retentionCountTable(t, s, "pedagogical_decisions"); got != 2 {
		t.Fatalf("retention should preserve referenced and fresh decisions, got %d", got)
	}
	for _, id := range []string{referenced.ID, "decision-fresh"} {
		if _, err := s.GetPedagogicalDecision(ctx, owner.TenantScope(), id); err != nil {
			t.Fatalf("protected decision %s: %v", id, err)
		}
	}
}
