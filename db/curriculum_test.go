// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestCurriculumSnapshot_StableIdentityAndImmutability(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	domain, err := s.CreateDomain(ctx, "L1", "Mathematics", "", models.KnowledgeSpace{
		Concepts:      []string{"derivative"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	baseline, err := s.GetCurriculumSnapshot(ctx, "L1", domain.ID, 1)
	if err != nil {
		t.Fatalf("get baseline: %v", err)
	}
	if len(baseline.Concepts) != 1 || baseline.Concepts[0].ID == "" {
		t.Fatalf("baseline lacks stable concept identity: %+v", baseline)
	}
	stableID := baseline.Concepts[0].ID
	stableKey := baseline.Concepts[0].Key

	next := models.CloneCurriculumSnapshot(baseline)
	next.Concepts[0].Label = "Differentiation"
	next.Operation = models.CurriculumOperation{
		Type:             models.CurriculumOperationRename,
		SourceConceptIDs: []string{stableID},
		Rationale:        "use the domain's canonical terminology",
	}
	next.Provenance = models.CurriculumProvenance{
		SourceType: "expert",
		Author:     "L1",
		Rationale:  "align terminology without changing competency identity",
	}
	next.Review = models.CurriculumReview{Status: models.CurriculumReviewUnreviewed}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", domain.ID, 1, next); err != nil {
		t.Fatalf("publish rename: %v", err)
	}

	got, err := s.GetCurriculumSnapshot(ctx, "L1", domain.ID, 2)
	if err != nil {
		t.Fatalf("get rename: %v", err)
	}
	if got.Concepts[0].ID != stableID || got.Concepts[0].Key != stableKey || got.Concepts[0].Label != "Differentiation" {
		t.Fatalf("rename changed identity or lost label: %+v", got.Concepts[0])
	}
	if got.Graph.Concepts[0] != stableKey {
		t.Fatalf("engine graph key changed during display rename: %v", got.Graph.Concepts)
	}

	// Both dialects reject mutation rather than silently pretending it worked.
	if _, err := s.root.ExecContext(ctx, rb(s, `UPDATE curriculum_versions SET snapshot_json = '{}' WHERE domain_id = ? AND version = 1`), domain.ID); err == nil {
		t.Fatal("immutable curriculum update was silently accepted")
	}
	unchanged, err := s.GetCurriculumSnapshot(ctx, "L1", domain.ID, 1)
	if err != nil {
		t.Fatalf("read immutable baseline: %v", err)
	}
	if unchanged.Concepts[0].ID != stableID || unchanged.Concepts[0].Label != "derivative" {
		t.Fatalf("immutable baseline was modified: %+v", unchanged)
	}
	if _, err := s.root.ExecContext(ctx, rb(s, `DELETE FROM curriculum_versions WHERE domain_id = ? AND version = 1`), domain.ID); err == nil {
		t.Fatal("immutable curriculum delete was silently accepted")
	}
	history, err := s.ListCurriculumSnapshots(ctx, "L1", domain.ID, 50)
	if err != nil || len(history) != 2 {
		t.Fatalf("immutable history deleted: len=%d err=%v", len(history), err)
	}
}

func TestCurriculumMetadataIDs_CannotMoveOrChangeKind(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	domain, err := s.CreateDomain(ctx, "L1", "Metadata identities", "", models.KnowledgeSpace{
		Concepts:      []string{"a", "b"},
		Prerequisites: map[string][]string{"b": {"a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := s.GetCurriculumSnapshot(ctx, "L1", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	next := models.CloneCurriculumSnapshot(baseline)
	next.Concepts[0].Outcomes = []models.CurriculumOutcome{{ID: "outcome_stable", Statement: "Demonstrate a"}}
	next.Concepts[0].Criteria = []models.CurriculumCriterion{{ID: "criterion_stable", Description: "Evidence for a"}}
	next.Operation = models.CurriculumOperation{
		Type:             models.CurriculumOperationUpdateMetadata,
		SourceConceptIDs: []string{next.Concepts[0].ID},
		Rationale:        "add observable metadata",
	}
	next.Provenance = models.CurriculumProvenance{SourceType: "test", Author: "L1", Rationale: "verify stable metadata identities"}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", domain.ID, 1, next); err != nil {
		t.Fatalf("publish metadata: %v", err)
	}

	v2, err := s.GetCurriculumSnapshot(ctx, "L1", domain.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Direct database writes cannot mutate or erase the stable metadata registry.
	if _, err := s.root.ExecContext(ctx, rb(s, `UPDATE curriculum_metadata_ids
		SET concept_id = ?, kind = 'criterion' WHERE id = ?`), v2.Concepts[1].ID, "outcome_stable"); err == nil {
		t.Fatal("immutable metadata identity update was silently accepted")
	}
	if _, err := s.root.ExecContext(ctx, rb(s, `DELETE FROM curriculum_metadata_ids WHERE id = ?`), "criterion_stable"); err == nil {
		t.Fatal("immutable metadata identity delete was silently accepted")
	}
	for _, want := range []struct {
		id   string
		kind string
	}{{"outcome_stable", "outcome"}, {"criterion_stable", "criterion"}} {
		var owner, kind string
		if err := s.root.QueryRowContext(ctx, rb(s, `SELECT concept_id, kind FROM curriculum_metadata_ids WHERE id = ?`), want.id).Scan(&owner, &kind); err != nil {
			t.Fatalf("immutable metadata registry lost %s: %v", want.id, err)
		}
		if owner != v2.Concepts[0].ID || kind != want.kind {
			t.Fatalf("metadata registry changed for %s: owner=%s kind=%s", want.id, owner, kind)
		}
	}
	moved := models.CloneCurriculumSnapshot(v2)
	moved.Concepts[0].Outcomes = nil
	moved.Concepts[0].Criteria = nil
	moved.Concepts[1].Outcomes = []models.CurriculumOutcome{{ID: "criterion_stable", Statement: "Illegally reclassified"}}
	moved.Concepts[1].Criteria = []models.CurriculumCriterion{{ID: "outcome_stable", Description: "Illegally moved"}}
	moved.Operation = models.CurriculumOperation{
		Type:             models.CurriculumOperationUpdateMetadata,
		SourceConceptIDs: []string{moved.Concepts[1].ID},
		Rationale:        "attempt metadata identity reuse",
	}
	moved.Provenance = models.CurriculumProvenance{SourceType: "test", Author: "L1", Rationale: "verify registry enforcement"}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", domain.ID, 2, moved); err == nil || !strings.Contains(err.Error(), "cannot change owner or kind") {
		t.Fatalf("metadata identity was moved or reclassified: %v", err)
	}
	fresh, err := s.GetDomainByID(ctx, domain.ID)
	if err != nil || fresh.GraphVersion != 2 {
		t.Fatalf("rejected metadata reuse changed active version: domain=%+v err=%v", fresh, err)
	}
	if _, err := s.GetCurriculumSnapshot(ctx, "L1", domain.ID, 3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected metadata reuse left a snapshot: %v", err)
	}
}

func TestEnsureCurriculumBaseline_ImportsLegacyVersionAboveOne(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.root.ExecContext(ctx, rb(s, `INSERT INTO domains
		(id, learner_id, name, personal_goal, graph_json, graph_version, created_at)
		VALUES (?, ?, ?, '', ?, ?, ?)`),
		"legacy-v3", "L1", "Legacy", `{"concepts":["a","b"],"prerequisites":{"b":["a"]}}`, 3, now); err != nil {
		t.Fatalf("insert legacy domain: %v", err)
	}
	snapshot, err := s.EnsureCurriculumBaseline(ctx, "L1", "legacy-v3")
	if err != nil {
		t.Fatalf("import legacy baseline: %v", err)
	}
	if snapshot.Version != 3 || snapshot.ParentVersion != 0 || snapshot.Operation.Type != models.CurriculumOperationBaselineImport {
		t.Fatalf("legacy lineage was invented or lost: %+v", snapshot)
	}
	if len(snapshot.Concepts) != 2 || snapshot.Concepts[0].ID == "" || snapshot.Concepts[1].ID == "" {
		t.Fatalf("legacy import lacks stable identities: %+v", snapshot.Concepts)
	}
}

func TestDeleteDomain_MaterializesLegacyCurriculumBeforeTombstone(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.root.ExecContext(ctx, rb(s, `INSERT INTO domains
		(id, learner_id, name, personal_goal, graph_json, graph_version, created_at)
		VALUES (?, ?, ?, '', ?, ?, ?)`),
		"legacy-delete", "L1", "Legacy deletion", `{"concepts":["a"],"prerequisites":{}}`, 4, now); err != nil {
		t.Fatalf("insert legacy domain: %v", err)
	}

	if err := s.DeleteDomain(ctx, "legacy-delete", "L1"); err != nil {
		t.Fatalf("tombstone legacy domain: %v", err)
	}
	history, err := s.ListCurriculumSnapshots(ctx, "L1", "legacy-delete", 50)
	if err != nil || len(history) != 1 {
		t.Fatalf("legacy curriculum was not preserved: len=%d err=%v", len(history), err)
	}
	if history[0].Version != 4 || history[0].Operation.Type != models.CurriculumOperationBaselineImport {
		t.Fatalf("unexpected legacy tombstone snapshot: %+v", history[0])
	}
}

func TestCompareAndSwapCurriculum_RejectsCyclicGraph(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	domain, err := s.CreateDomain(ctx, "L1", "Graph", "", models.KnowledgeSpace{
		Concepts:      []string{"a", "b"},
		Prerequisites: map[string][]string{"b": {"a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := s.GetCurriculumSnapshot(ctx, "L1", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	next := models.CloneCurriculumSnapshot(baseline)
	next.Graph.Prerequisites["a"] = []string{"b"}
	next.Operation = models.CurriculumOperation{Type: models.CurriculumOperationLegacyUpdate, Rationale: "invalid cycle regression probe"}
	next.Provenance = models.CurriculumProvenance{SourceType: "test", Author: "L1", Rationale: "verify invariant"}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", domain.ID, 1, next); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cyclic curriculum accepted: %v", err)
	}
	fresh, err := s.GetDomainByID(ctx, domain.ID)
	if err != nil || fresh.GraphVersion != 1 {
		t.Fatalf("rejected cycle changed active version: domain=%+v err=%v", fresh, err)
	}
}

func TestCompareAndSwapCurriculum_ConcurrentSingleWinner(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	domain, err := s.CreateDomain(ctx, "L1", "Science", "", models.KnowledgeSpace{
		Concepts:      []string{"observation"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	baseline, err := s.GetCurriculumSnapshot(ctx, "L1", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}

	makeCandidate := func(key string) *models.CurriculumSnapshot {
		candidate, buildErr := reconcileLegacyGraph(baseline, models.KnowledgeSpace{
			Concepts:      []string{"observation", key},
			Prerequisites: map[string][]string{key: {"observation"}},
		}, "L1")
		if buildErr != nil {
			t.Fatalf("build candidate: %v", buildErr)
		}
		return candidate
	}
	candidates := []*models.CurriculumSnapshot{makeCandidate("experiment"), makeCandidate("measurement")}

	start := make(chan struct{})
	results := make(chan error, len(candidates))
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- s.CompareAndSwapCurriculum(ctx, "L1", domain.ID, 1, candidate)
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storeport.ErrCurriculumVersionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected contender error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS results: successes=%d conflicts=%d", successes, conflicts)
	}
	history, err := s.ListCurriculumSnapshots(ctx, "L1", domain.ID, 50)
	if err != nil || len(history) != 2 || history[1].Version != 2 {
		t.Fatalf("unexpected history after race: %+v err=%v", history, err)
	}
}

func TestDeleteDomain_TombstonePreservesAssessmentAndCurriculumAudit(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	domain, err := s.CreateDomain(ctx, "L1", "Regulated subject", "", models.KnowledgeSpace{
		Concepts:      []string{"safety"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt := &models.AssessmentAttempt{
		ID:              "attempt-before-delete",
		LearnerID:       "L1",
		DomainID:        domain.ID,
		ConceptID:       "safety",
		ActivityID:      "activity-v1",
		ActivityVersion: 1,
		ActivityType:    string(models.ActivityMasteryChallenge),
		Observable:      "independent safe performance",
		TaskText:        "Demonstrate the safety procedure.",
		TaskContentHash: "task-hash",
		RubricJSON:      `{"criteria":[{"id":"safe","max_score":1}]}`,
		PassingScore:    1,
		Status:          models.AssessmentAttemptPrepared,
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.CreateAssessmentAttempt(ctx, attempt); err != nil {
		t.Fatalf("create assessment: %v", err)
	}
	if err := s.DeleteDomain(ctx, domain.ID, "L1"); err != nil {
		t.Fatalf("tombstone domain with evidence: %v", err)
	}
	if _, err := s.GetDomainByID(ctx, domain.ID); err == nil {
		t.Fatal("tombstoned domain remained visible to runtime readers")
	}
	if _, err := s.GetAssessmentAttempt(ctx, "L1", attempt.ID); err != nil {
		t.Fatalf("assessment evidence was not preserved: %v", err)
	}
	history, err := s.ListCurriculumSnapshots(ctx, "L1", domain.ID, 50)
	if err != nil || len(history) != 1 {
		t.Fatalf("curriculum audit was not preserved: len=%d err=%v", len(history), err)
	}
	var deletedAt time.Time
	if err := s.root.QueryRowContext(ctx, rb(s, `SELECT deleted_at FROM domains WHERE id = ?`), domain.ID).Scan(&deletedAt); err != nil || deletedAt.IsZero() {
		t.Fatalf("domain tombstone missing: deleted_at=%v err=%v", deletedAt, err)
	}
}
