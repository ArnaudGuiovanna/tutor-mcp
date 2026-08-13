// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var (
	learnerContextBenchmarkOverview  *storeport.LearnerContextOverview
	learnerContextBenchmarkNarrative *storeport.LearnerContextNarrativeSignals
)

// BenchmarkLearnerContextReadPath compares the persistence work only. The
// legacy branch deliberately mirrors get_learner_context before its read-model
// migration; the read_model branch performs the four specialized reads plus
// the unchanged active-session lookup.
func BenchmarkLearnerContextReadPath(b *testing.B) {
	s, domain, now := setupLearnerContextBenchmark(b, 100, 5000)
	ctx := context.Background()

	b.Run("legacy_13_queries", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			learner, err := s.GetLearnerByID(ctx, "L1")
			if err != nil {
				b.Fatal(err)
			}
			if _, err = s.GetDomainsByLearner(ctx, "L1", false); err != nil {
				b.Fatal(err)
			}
			selected, err := s.GetDomainByLearner(ctx, "L1")
			if err != nil {
				b.Fatal(err)
			}
			if _, err = s.GetConceptStatesByLearner(ctx, "L1"); err != nil {
				b.Fatal(err)
			}
			if _, err = s.GetInteractionsSince(ctx, "L1", time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)); err != nil {
				b.Fatal(err)
			}
			if _, err = s.GetDomainsByLearner(ctx, "L1", true); err != nil {
				b.Fatal(err)
			}
			if _, err = s.ConceptMasteryDeltaInDomain(ctx, "L1", selected.ID, selected.Graph.Concepts, now.Add(-30*24*time.Hour), 3); err != nil {
				b.Fatal(err)
			}
			if _, err = s.CountLearnerSessionStreak(ctx, "L1"); err != nil {
				b.Fatal(err)
			}
			if _, err = s.MilestonesInWindowInDomain(ctx, "L1", selected.ID, selected.Graph.Concepts, now.Add(-7*24*time.Hour)); err != nil {
				b.Fatal(err)
			}
			if _, err = s.GetRecentAffectStates(ctx, "L1", 5); err != nil {
				b.Fatal(err)
			}
			_, _ = s.GetActiveLearningSession(ctx, learner.ID)
		}
	})

	b.Run("read_model_5_queries", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			var err error
			learnerContextBenchmarkOverview, err = s.GetLearnerContextOverview(ctx, "L1", now)
			if err != nil {
				b.Fatal(err)
			}
			learnerContextBenchmarkNarrative, err = s.GetLearnerContextNarrativeSignals(ctx, "L1", domain.ID, domain.Graph.Concepts, now)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = s.GetActiveLearningSession(ctx, "L1")
		}
	})
}

func setupLearnerContextBenchmark(b *testing.B, conceptCount, interactionCount int) (*Store, *models.Domain, time.Time) {
	b.Helper()
	template, err := sqliteTestDBTemplateBytes()
	if err != nil {
		b.Fatalf("build SQLite benchmark template: %v", err)
	}
	path := filepath.Join(b.TempDir(), "learner-context-benchmark.db")
	if err := os.WriteFile(path, template, 0o600); err != nil {
		b.Fatalf("copy SQLite benchmark template: %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatalf("open benchmark database: %v", err)
	}
	raw.SetMaxOpenConns(1)
	b.Cleanup(func() { _ = raw.Close() })
	s := NewStore(raw)
	ctx := context.Background()
	now := time.Now().UTC()
	concepts := make([]string, conceptCount)
	for i := range concepts {
		concepts[i] = fmt.Sprintf("concept-%03d", i)
	}
	domain, err := s.CreateDomain(ctx, "L1", "benchmark", "", models.KnowledgeSpace{
		Concepts:      concepts,
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		b.Fatalf("create benchmark domain: %v", err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin benchmark seed: %v", err)
	}
	stateStmt, err := tx.PrepareContext(ctx, rb(s, `INSERT INTO concept_states
		(learner_id, domain_id, concept, card_state, p_mastery, stability)
		VALUES (?, ?, ?, 'review', ?, 4)`))
	if err != nil {
		b.Fatalf("prepare state seed: %v", err)
	}
	for i, concept := range concepts {
		if _, err := stateStmt.ExecContext(ctx, "L1", domain.ID, concept, 0.2+float64(i%7)/10); err != nil {
			b.Fatalf("seed state: %v", err)
		}
	}
	_ = stateStmt.Close()
	interactionStmt, err := tx.PrepareContext(ctx, rb(s, `INSERT INTO interactions
		(learner_id, domain_id, concept, activity_type, success, created_at)
		VALUES (?, ?, ?, 'PRACTICE', ?, ?)`))
	if err != nil {
		b.Fatalf("prepare interaction seed: %v", err)
	}
	for i := 0; i < interactionCount; i++ {
		createdAt := now.Add(-time.Duration(i%90) * 24 * time.Hour).Add(-time.Duration(i%24) * time.Minute)
		if _, err := interactionStmt.ExecContext(ctx, "L1", domain.ID, concepts[i%len(concepts)], i%3 != 0, createdAt); err != nil {
			b.Fatalf("seed interaction: %v", err)
		}
	}
	_ = interactionStmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit benchmark seed: %v", err)
	}
	return s, domain, now
}
