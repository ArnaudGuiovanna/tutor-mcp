// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"
)

// TestCountInteractionsByConcept asserts the COUNT(*) wrapper returns 0 for
// missing learners/concepts and the correct positive count when rows exist.
func TestCountInteractionsByConcept(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	insertSimpleInteraction(t, store, "C1", true, now.Add(-1*time.Minute))
	insertSimpleInteraction(t, store, "C1", false, now.Add(-2*time.Minute))
	insertSimpleInteraction(t, store, "C2", true, now.Add(-3*time.Minute))

	cases := []struct {
		concept string
		want    int
	}{
		{"C1", 2},
		{"C2", 1},
		{"missing", 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.concept, func(t *testing.T) {
			got, err := store.CountInteractionsByConcept(context.Background(), "L1", tc.concept)
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if got != tc.want {
				t.Errorf("count(%s)=%d want %d", tc.concept, got, tc.want)
			}
		})
	}
}

// TestSelfInitiatedRatio covers the (a) zero-division guard when no rows exist
// and (b) the ratio numerator/denominator across two concepts.
func TestSelfInitiatedRatio(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	// Empty: 0 ratio.
	r, err := store.SelfInitiatedRatio(context.Background(), "L1", "C1")
	if err != nil {
		t.Fatalf("empty ratio: %v", err)
	}
	if r != 0 {
		t.Errorf("empty ratio = %v want 0", r)
	}

	// 3 interactions on C1, 2 self-initiated => 2/3.
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.root.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, self_initiated, created_at)
		 VALUES ('L1','C1','RECALL_EXERCISE',1,1,?)`,
		now,
	)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, self_initiated, created_at)
		 VALUES ('L1','C1','RECALL_EXERCISE',1,1,?)`,
		now,
	)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, self_initiated, created_at)
		 VALUES ('L1','C1','RECALL_EXERCISE',1,0,?)`,
		now,
	)

	r, err = store.SelfInitiatedRatio(context.Background(), "L1", "C1")
	if err != nil {
		t.Fatalf("ratio: %v", err)
	}
	if r < 0.6 || r > 0.7 {
		t.Errorf("ratio = %v want ~0.666", r)
	}
}

// TestCountLearnerSessionStreak covers all three branches of the streak loop:
// fresh streak, contiguous extension, and gap termination.
func TestCountLearnerSessionStreak(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC().Truncate(24 * time.Hour)

	// Insert today, yesterday, day before yesterday using SQL's text date format
	// so the substr(created_at, 1, 10) trick produces parseable YYYY-MM-DD strings.
	insertInteractionAtSQLTime(t, store, "C", 1, now.Add(1*time.Hour))
	insertInteractionAtSQLTime(t, store, "C", 1, now.AddDate(0, 0, -1).Add(1*time.Hour))
	insertInteractionAtSQLTime(t, store, "C", 1, now.AddDate(0, 0, -2).Add(1*time.Hour))

	streak, err := store.CountLearnerSessionStreak(context.Background(), "L1")
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if streak != 3 {
		t.Errorf("streak = %d want 3", streak)
	}

	// Empty learner: streak = 0.
	streak, err = store.CountLearnerSessionStreak(context.Background(), "L-missing")
	if err != nil {
		t.Fatalf("streak missing: %v", err)
	}
	if streak != 0 {
		t.Errorf("streak missing = %d", streak)
	}
}

func TestCountLearnerSessionStreak_StaleStart(t *testing.T) {
	store := setupTestDB(t)
	// Last interaction is 5 days ago — too stale to start a streak.
	insertInteractionAtSQLTime(t, store, "C", 1, time.Now().UTC().AddDate(0, 0, -5))
	streak, err := store.CountLearnerSessionStreak(context.Background(), "L1")
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if streak != 0 {
		t.Errorf("streak stale = %d want 0", streak)
	}
}

func TestCountLearnerSessionStreak_GapTerminates(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC().Truncate(24 * time.Hour)
	// Today and 3 days ago — gap at -1 and -2 — streak terminates at 1.
	insertInteractionAtSQLTime(t, store, "C", 1, now.Add(1*time.Hour))
	insertInteractionAtSQLTime(t, store, "C", 1, now.AddDate(0, 0, -3).Add(1*time.Hour))
	streak, err := store.CountLearnerSessionStreak(context.Background(), "L1")
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if streak != 1 {
		t.Errorf("streak gap = %d want 1", streak)
	}
}
