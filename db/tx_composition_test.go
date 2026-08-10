// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"tutor-mcp/models"
	"tutor-mcp/store"
)

// TestTransactionsRollbackOnPanic verifies both transaction entry points
// release their connection and roll back writes when a callback panics. The
// pool is restricted to one connection so the follow-up write would time out
// if the panicking transaction retained it.
func TestTransactionsRollbackOnPanic(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Store, context.Context, func(store.Store) error) error
	}{
		{
			name: "WithTx",
			run: func(s *Store, ctx context.Context, fn func(store.Store) error) error {
				return s.WithTx(ctx, fn)
			},
		},
		{
			name: "inTx",
			run: func(s *Store, ctx context.Context, fn func(store.Store) error) error {
				return s.inTx(ctx, nil, func(tx *Store) error { return fn(tx) })
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			s.root.SetMaxOpenConns(1)

			// Keep the transaction context alive while testing connection reuse.
			// On a regression, the shorter follow-up deadline fails the test before
			// this context cancellation releases the leaked transaction.
			txCtx, cancelTx := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelTx()

			panicValue := &struct{ label string }{label: tc.name}
			rolledBack := models.NewConceptState("L1", "panic-rollback-"+tc.name)
			var callbackErr, runErr error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				runErr = tc.run(s, txCtx, func(tx store.Store) error {
					callbackErr = tx.UpsertConceptState(txCtx, rolledBack)
					if callbackErr != nil {
						return callbackErr
					}
					panic(panicValue)
				})
			}()

			if callbackErr != nil {
				t.Fatalf("write before panic: %v", callbackErr)
			}
			if runErr != nil {
				t.Fatalf("transaction returned instead of propagating panic: %v", runErr)
			}
			if recovered != panicValue {
				t.Fatalf("recovered panic = %#v, want original value %#v", recovered, panicValue)
			}

			followCtx, cancelFollow := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelFollow()
			afterPanic := models.NewConceptState("L1", "after-panic-"+tc.name)
			if err := s.UpsertConceptState(followCtx, afterPanic); err != nil {
				t.Fatalf("write after panic (transaction connection was not released): %v", err)
			}

			if _, err := s.GetConceptState(followCtx, "L1", rolledBack.Concept); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("panicking transaction was not rolled back: got error %v, want sql.ErrNoRows", err)
			}
			if _, err := s.GetConceptState(followCtx, "L1", afterPanic.Concept); err != nil {
				t.Fatalf("read write made after panic: %v", err)
			}
		})
	}
}

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
