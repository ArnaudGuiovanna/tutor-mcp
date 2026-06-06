// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/models"
	"tutor-mcp/store"
)

// TestLoadConcurrentInteractions is a Phase 6 load harness for the persistence
// layer: it simulates many learners concurrently running the hot record-
// interaction path (WithTx + GetConceptStateForUpdate + UpsertConceptState +
// CreateInteraction) and reports throughput. It demonstrates that the dialect-
// aware store sustains concurrent writers — on Postgres this is the horizontal
// path; on SQLite it is serialized by the single writer.
//
// Skipped unless TUTOR_LOAD_TEST=1. Point it at Postgres with TUTOR_TEST_PG_DSN
// to exercise the real concurrent path:
//
//	TUTOR_LOAD_TEST=1 TUTOR_TEST_PG_DSN="postgres://tutor:dev@localhost:55432/tutor_test" \
//	  go test ./db/ -run TestLoadConcurrentInteractions -v -timeout 5m
func TestLoadConcurrentInteractions(t *testing.T) {
	if os.Getenv("TUTOR_LOAD_TEST") == "" {
		t.Skip("set TUTOR_LOAD_TEST=1 to run the load harness")
	}
	s := setupTestDB(t)
	ctx := context.Background()

	const (
		learners            = 50
		interactionsPerUser = 40
	)
	// Seed learners + one concept state each.
	for i := 0; i < learners; i++ {
		lid := fmt.Sprintf("load_%d", i)
		seedLearner(t, s, lid)
		if err := s.UpsertConceptState(ctx, models.NewConceptState(lid, "C")); err != nil {
			t.Fatal(err)
		}
	}

	var done int64
	var errCount int64
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < learners; i++ {
		lid := fmt.Sprintf("load_%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < interactionsPerUser; j++ {
				err := s.WithTx(ctx, func(tx store.Store) error {
					cs, err := tx.GetConceptStateForUpdate(ctx, lid, "C")
					if err != nil {
						return err
					}
					cs.Reps++
					cs.PMastery = 0.1 + 0.8*float64(cs.Reps)/float64(interactionsPerUser)
					if err := tx.UpsertConceptState(ctx, cs); err != nil {
						return err
					}
					return tx.CreateInteraction(ctx, &models.Interaction{
						LearnerID:    lid,
						Concept:      "C",
						ActivityType: string(models.ActivityRecall),
						Success:      true,
						CreatedAt:    time.Now().UTC(),
					})
				})
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					t.Errorf("tx: %v", err)
					return
				}
				atomic.AddInt64(&done, 1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := int64(learners * interactionsPerUser)
	if done != total {
		t.Fatalf("completed %d/%d interactions (%d errors)", done, total, errCount)
	}
	// Correctness under concurrency: every learner's reps must equal the count.
	for i := 0; i < learners; i++ {
		lid := fmt.Sprintf("load_%d", i)
		cs, err := s.GetConceptState(ctx, lid, "C")
		if err != nil {
			t.Fatal(err)
		}
		if cs.Reps != interactionsPerUser {
			t.Fatalf("learner %s reps=%d, want %d (lost update under load)", lid, cs.Reps, interactionsPerUser)
		}
	}
	tps := float64(total) / elapsed.Seconds()
	t.Logf("LOAD: %d interactions across %d learners in %s = %.0f tx/s (errors=%d)",
		total, learners, elapsed.Round(time.Millisecond), tps, errCount)
}
