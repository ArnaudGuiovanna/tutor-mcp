// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"sync"
	"testing"

	"tutor-mcp/models"
)

func mkDomain(t *testing.T, store *Store, concepts []string) *models.Domain {
	t.Helper()
	graph := models.KnowledgeSpace{
		Concepts:      concepts,
		Prerequisites: map[string][]string{},
	}
	d, err := store.CreateDomain(context.Background(), "L1", "TestDomain", "ship a Go backend", graph)
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	return d
}

func TestMergeDomainGoalRelevance_FreshDomain(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"Goroutines", "Channels", "Interfaces"})

	merged, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{
		"Goroutines": 0.9,
		"Channels":   0.7,
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := merged.Relevance["Goroutines"]; got != 0.9 {
		t.Errorf("Goroutines: want 0.9, got %v", got)
	}
	if merged.ForGraphVersion != 1 {
		t.Errorf("ForGraphVersion: want 1, got %d", merged.ForGraphVersion)
	}
	if len(merged.Relevance) != 2 {
		t.Errorf("Relevance size: want 2, got %d", len(merged.Relevance))
	}
}

func TestMergeDomainGoalRelevance_IncrementalKeepsExisting(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A", "B", "C"})

	if _, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"A": 0.9, "B": 0.5}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	merged, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"C": 0.3})
	if err != nil {
		t.Fatalf("second set: %v", err)
	}
	if len(merged.Relevance) != 3 {
		t.Errorf("post-merge size: want 3, got %d (relevance=%+v)", len(merged.Relevance), merged.Relevance)
	}
	if merged.Relevance["A"] != 0.9 || merged.Relevance["B"] != 0.5 || merged.Relevance["C"] != 0.3 {
		t.Errorf("merge lost prior entries: %+v", merged.Relevance)
	}
}

func TestMergeDomainGoalRelevance_OverwritesSameConcept(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A", "B"})

	_, _ = store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"A": 0.9, "B": 0.5})
	merged, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"A": 0.2})
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if merged.Relevance["A"] != 0.2 {
		t.Errorf("A overwrite: want 0.2, got %v", merged.Relevance["A"])
	}
	if merged.Relevance["B"] != 0.5 {
		t.Errorf("B preserved: want 0.5, got %v", merged.Relevance["B"])
	}
}

func TestMergeDomainGoalRelevance_IncrementsVersion(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A"})

	for i := 1; i <= 3; i++ {
		if _, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"A": 0.5}); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
		fresh, _ := store.GetDomainByID(context.Background(), d.ID)
		if fresh.GoalRelevanceVersion != i {
			t.Errorf("after set %d: GoalRelevanceVersion want %d, got %d", i, i, fresh.GoalRelevanceVersion)
		}
	}
}

func TestConcurrentGraphAndGoalRelevanceMutationsPreserveBothWriters(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A"})
	graph := models.KnowledgeSpace{
		Concepts:      []string{"A", "B"},
		Prerequisites: map[string][]string{"B": {"A"}},
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- store.UpdateDomainGraph(context.Background(), d.ID, graph)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"A": 0.9})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent mutation failed: %v", err)
		}
	}

	fresh, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if fresh.GraphVersion != 2 || len(fresh.Graph.Concepts) != 2 || fresh.Graph.Concepts[0] != "A" || fresh.Graph.Concepts[1] != "B" {
		t.Fatalf("graph writer was lost: %+v", fresh)
	}
	relevance, err := store.GetDomainGoalRelevance(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("get goal relevance: %v", err)
	}
	if relevance == nil || relevance.Relevance["A"] != 0.9 {
		t.Fatalf("goal-relevance writer was lost: %+v", relevance)
	}
	if relevance.ForGraphVersion != 1 && relevance.ForGraphVersion != 2 {
		t.Fatalf("goal relevance references impossible graph version: %+v", relevance)
	}
	if relevance.ForGraphVersion == 1 && !fresh.IsGoalRelevanceStale() {
		t.Fatalf("concurrent graph winner was not exposed as stale: domain=%+v relevance=%+v", fresh, relevance)
	}
}

func TestUpdateDomainGraph_BumpsGraphVersion(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A"})
	if d.GraphVersion != 1 {
		t.Fatalf("initial GraphVersion: want 1, got %d", d.GraphVersion)
	}

	d.Graph.Concepts = append(d.Graph.Concepts, "B")
	if err := store.UpdateDomainGraph(context.Background(), d.ID, d.Graph); err != nil {
		t.Fatalf("update graph: %v", err)
	}

	fresh, _ := store.GetDomainByID(context.Background(), d.ID)
	if fresh.GraphVersion != 2 {
		t.Errorf("after add: GraphVersion want 2, got %d", fresh.GraphVersion)
	}
}

func TestIsGoalRelevanceStale_AfterAddConcepts(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A", "B"})

	if _, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"A": 0.9, "B": 0.4}); err != nil {
		t.Fatalf("set: %v", err)
	}
	d1, _ := store.GetDomainByID(context.Background(), d.ID)
	if d1.IsGoalRelevanceStale() {
		t.Error("immediately after set, should NOT be stale")
	}

	d1.Graph.Concepts = append(d1.Graph.Concepts, "C")
	_ = store.UpdateDomainGraph(context.Background(), d1.ID, d1.Graph)

	d2, _ := store.GetDomainByID(context.Background(), d1.ID)
	if !d2.IsGoalRelevanceStale() {
		t.Error("after add_concepts, should be stale (graph_version > for_graph_version)")
	}
	uncov := d2.UncoveredConcepts()
	if len(uncov) != 1 || uncov[0] != "C" {
		t.Errorf("uncovered: want [C], got %v", uncov)
	}
}

func TestGetDomainGoalRelevance_EmptyReturnsNil(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A"})

	gr, err := store.GetDomainGoalRelevance(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gr != nil {
		t.Errorf("empty domain: want nil, got %+v", gr)
	}
}

func TestGetDomainGoalRelevance_RoundTrip(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A", "B"})

	_, _ = store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"A": 0.9, "B": 0.4})
	gr, err := store.GetDomainGoalRelevance(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gr == nil || gr.Relevance["A"] != 0.9 || gr.Relevance["B"] != 0.4 {
		t.Errorf("roundtrip mismatch: %+v", gr)
	}
}

func TestMergeDomainGoalRelevance_RejectsNil(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A"})

	if _, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, nil); err == nil {
		t.Error("expected error for nil relevance map")
	}
}
