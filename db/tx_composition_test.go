// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"

	"tutor-mcp/models"
	"tutor-mcp/store"
)

// TestNestedTxComposition guards against the nil-root panic: methods that open
// their own transaction internally (DeleteDomain, MergeDomainGoalRelevance,
// etc.) must compose when invoked from inside a WithTx callback rather than
// dereferencing a nil root *sql.DB. Before the fix, the tx-scoped Store handed
// to the callback had root == nil and any such method panicked with a
// nil-pointer dereference.
func TestNestedTxComposition(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	d, err := s.CreateDomain(ctx, "L1", "math", "", models.KnowledgeSpace{
		Concepts:      []string{"a", "b"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}

	// DeleteDomain inside WithTx must not panic and must take effect on commit.
	if err := s.WithTx(ctx, func(tx store.Store) error {
		return tx.DeleteDomain(ctx, d.ID, "L1")
	}); err != nil {
		t.Fatalf("DeleteDomain inside WithTx: %v", err)
	}
	if _, err := s.GetDomainByID(ctx, d.ID); err == nil {
		t.Fatalf("domain still present after DeleteDomain inside WithTx")
	}

	// MergeDomainGoalRelevance inside WithTx must also compose.
	d2, err := s.CreateDomain(ctx, "L1", "info", "", models.KnowledgeSpace{
		Concepts:      []string{"x"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create domain 2: %v", err)
	}
	if err := s.WithTx(ctx, func(tx store.Store) error {
		_, mErr := tx.MergeDomainGoalRelevance(ctx, d2.ID, map[string]float64{"x": 0.9})
		return mErr
	}); err != nil {
		t.Fatalf("MergeDomainGoalRelevance inside WithTx: %v", err)
	}
	gr, err := s.GetDomainGoalRelevance(ctx, d2.ID)
	if err != nil {
		t.Fatalf("get goal relevance: %v", err)
	}
	if gr == nil || gr.Relevance["x"] != 0.9 {
		t.Fatalf("goal relevance not persisted from nested tx: %+v", gr)
	}
}
