package db

import (
	"context"
	"testing"

	"tutor-mcp/models"
)

// TestActiveDomainConceptSet_FiltersOutDeletedDomain reproduces the orphan-concept
// bug observed by the cosmos client: after delete_domain, concept_states for that
// domain remain in DB (by design — progression is preserved). The set returned by
// ActiveDomainConceptSet must NOT include those orphan concepts so that downstream
// signals (priority_concept, alerts) don't surface them.
func TestActiveDomainConceptSet_FiltersOutDeletedDomain(t *testing.T) {
	store := setupTestDB(t)

	cuisine, err := store.CreateDomain(context.Background(), "L1", "cuisine", "", models.KnowledgeSpace{
		Concepts:      []string{"Bases de la cuisine", "Sauces"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create cuisine: %v", err)
	}
	if _, err := store.CreateDomain(context.Background(), "L1", "info", "", models.KnowledgeSpace{
		Concepts:      []string{"Variables", "Boucles"},
		Prerequisites: map[string][]string{},
	}); err != nil {
		t.Fatalf("create info: %v", err)
	}

	// Seed orphan history under cuisine before deleting it.
	seedConceptState(t, store, "Bases de la cuisine", 0.4)
	seedConceptState(t, store, "Variables", 0.6)

	if err := store.DeleteDomain(context.Background(), cuisine.ID, "L1"); err != nil {
		t.Fatalf("delete cuisine: %v", err)
	}

	set, err := store.ActiveDomainConceptSet(context.Background(), "L1")
	if err != nil {
		t.Fatalf("active concept set: %v", err)
	}
	if set["Bases de la cuisine"] {
		t.Errorf("expected deleted-domain concept to be absent, got present")
	}
	if !set["Variables"] || !set["Boucles"] {
		t.Errorf("expected active-domain concepts to be present, got %v", set)
	}

	// concept_states themselves are still there — that's the design intent.
	states, _ := store.GetConceptStatesByLearner(context.Background(), "L1")
	if len(states) != 2 {
		t.Errorf("expected 2 concept_states preserved after delete, got %d", len(states))
	}
}

// TestActiveDomainConceptSet_ExcludesArchived confirms archived domains don't leak
// into the active set either — the dashboard/priority surface should only mention
// concepts the learner currently has visible.
func TestActiveDomainConceptSet_ExcludesArchived(t *testing.T) {
	store := setupTestDB(t)

	d, err := store.CreateDomain(context.Background(), "L1", "info", "", models.KnowledgeSpace{
		Concepts:      []string{"Variables"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if err := store.ArchiveDomain(context.Background(), d.ID, "L1"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	set, err := store.ActiveDomainConceptSet(context.Background(), "L1")
	if err != nil {
		t.Fatalf("active concept set: %v", err)
	}
	if set["Variables"] {
		t.Errorf("archived-domain concept should not be in active set")
	}
}

// TestActiveDomainConceptSet_NoDomains returns an empty (non-nil) set so downstream
// filters can rely on len(set) == 0 to drop everything.
func TestActiveDomainConceptSet_NoDomains(t *testing.T) {
	store := setupTestDB(t)

	set, err := store.ActiveDomainConceptSet(context.Background(), "L1")
	if err != nil {
		t.Fatalf("active concept set: %v", err)
	}
	if set == nil {
		t.Errorf("expected non-nil empty set, got nil")
	}
	if len(set) != 0 {
		t.Errorf("expected empty set, got %d entries", len(set))
	}
}
