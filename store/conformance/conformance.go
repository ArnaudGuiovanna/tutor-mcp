// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Package conformance holds behaviour tests every store.Store implementation
// must pass. Phase 2's PostgresStore reuses RunConformance unchanged.
package conformance

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"tutor-mcp/models"
	"tutor-mcp/store"
)

// RunConformance exercises the cross-implementation contract. newStore must
// return a fresh, migrated, empty Store on each call.
func RunConformance(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()

	t.Run("learner round-trip", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ctx := context.Background()

		l, err := s.CreateLearner(ctx, "alice@example.com", "hash1", "learn Go", "")
		if err != nil {
			t.Fatalf("CreateLearner: %v", err)
		}
		if l == nil || l.ID == "" {
			t.Fatal("CreateLearner returned nil or empty ID")
		}

		got, err := s.GetLearnerByID(ctx, l.ID)
		if err != nil {
			t.Fatalf("GetLearnerByID: %v", err)
		}
		if got == nil {
			t.Fatal("GetLearnerByID returned nil")
		}
		if got.Email != "alice@example.com" {
			t.Errorf("email: got %q, want %q", got.Email, "alice@example.com")
		}
		if got.ID != l.ID {
			t.Errorf("ID: got %q, want %q", got.ID, l.ID)
		}
	})

	t.Run("concept state upsert is read-back consistent", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ctx := context.Background()

		l, err := s.CreateLearner(ctx, "bob@example.com", "hash2", "learn algorithms", "")
		if err != nil {
			t.Fatalf("CreateLearner: %v", err)
		}

		cs := models.NewConceptState(l.ID, "c1")
		cs.PMastery = 0.42

		if err := s.UpsertConceptState(ctx, cs); err != nil {
			t.Fatalf("UpsertConceptState: %v", err)
		}

		got, err := s.GetConceptState(ctx, l.ID, "c1")
		if err != nil {
			t.Fatalf("GetConceptState: %v", err)
		}
		if got == nil {
			t.Fatal("GetConceptState returned nil")
		}
		if got.PMastery != 0.42 {
			t.Errorf("PMastery: got %v, want 0.42", got.PMastery)
		}
	})

	t.Run("WithTx commits on success", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ctx := context.Background()

		l, err := s.CreateLearner(ctx, "carol@example.com", "hash3", "learn concurrency", "")
		if err != nil {
			t.Fatalf("CreateLearner: %v", err)
		}

		cs := models.NewConceptState(l.ID, "tx-commit")
		cs.PMastery = 0.77

		err = s.WithTx(ctx, func(tx store.Store) error {
			return tx.UpsertConceptState(ctx, cs)
		})
		if err != nil {
			t.Fatalf("WithTx: %v", err)
		}

		// Read outside the tx — must see the committed row.
		got, err := s.GetConceptState(ctx, l.ID, "tx-commit")
		if err != nil {
			t.Fatalf("GetConceptState after commit: %v", err)
		}
		if got.PMastery != 0.77 {
			t.Errorf("PMastery after commit: got %v, want 0.77", got.PMastery)
		}
	})

	t.Run("WithTx rolls back on error", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ctx := context.Background()

		l, err := s.CreateLearner(ctx, "dave@example.com", "hash4", "learn databases", "")
		if err != nil {
			t.Fatalf("CreateLearner: %v", err)
		}

		cs := models.NewConceptState(l.ID, "rollback")
		cs.PMastery = 0.99

		txErr := s.WithTx(ctx, func(tx store.Store) error {
			if err := tx.UpsertConceptState(ctx, cs); err != nil {
				return err
			}
			return errors.New("boom")
		})
		if txErr == nil {
			t.Fatal("WithTx should have returned an error")
		}
		if txErr.Error() != "boom" {
			t.Errorf("WithTx error: got %q, want %q", txErr.Error(), "boom")
		}

		// The row must NOT be visible after the rollback.
		got, err := s.GetConceptState(ctx, l.ID, "rollback")
		if err == nil {
			// If no error, the row was persisted — that is a bug.
			t.Errorf("GetConceptState after rollback: expected error (absent row), got PMastery=%v", got.PMastery)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			// The error must wrap sql.ErrNoRows — any other error is unexpected.
			t.Errorf("GetConceptState after rollback: expected sql.ErrNoRows-wrapped error, got: %v", err)
		}
	})
}
