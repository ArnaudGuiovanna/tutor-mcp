// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"testing"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Row-level security separates tenants, not learners: the learner predicate
// in these queries is the only guard between learners of one tenant.
func TestLearnerScopedDomainAccess(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	other, err := s.CreateLearner(ctx, "neighbour@example.test", "hash", "", "")
	if err != nil {
		t.Fatal(err)
	}
	domain, err := s.CreateDomain(ctx, "L1", "Go", "", models.KnowledgeSpace{
		Concepts: []string{"a"}, Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got, err := s.GetLearnerDomainByID(ctx, "L1", domain.ID); err != nil || got.ID != domain.ID {
		t.Fatalf("owner lookup=%v err=%v", got, err)
	}
	if _, err := s.GetLearnerDomainByID(ctx, other.ID, domain.ID); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreign lookup err=%v, want ErrNotFound", err)
	}
	if _, err := s.MergeDomainGoalRelevance(ctx, other.ID, domain.ID, map[string]float64{"a": 0.1}); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreign merge err=%v, want ErrNotFound", err)
	}
	if relevance, err := s.GetDomainGoalRelevance(ctx, "L1", domain.ID); err != nil || relevance != nil {
		t.Fatalf("foreign merge wrote relevance=%+v err=%v", relevance, err)
	}
	if _, err := s.GetDomainGoalRelevance(ctx, other.ID, domain.ID); err == nil {
		t.Fatal("foreign learner read the goal relevance")
	}
}
