// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

// Issue #1: applyInteraction holds the BKT → FSRS → IRT read-modify-write
// core but is only covered indirectly (via record_interaction handler tests)
// or under concurrency (TestRecordInteraction_NoLostUpdateUnderConcurrency).
// These focused unit tests exercise the success and failure paths directly
// and assert that the persisted concept_state moves in the expected
// direction, complementing — not duplicating — the concurrency test.

// A correct answer must push mastery up from the default and advance the
// FSRS card (Reps incremented, a next review scheduled).
func TestApplyInteraction_CorrectAnswer_MasteryUp(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math") // concepts: ["a","b"]

	now := time.Now().UTC()
	cs, _, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept:             "a",
		ActivityType:        "RECALL_EXERCISE",
		Success:             true,
		ResponseTimeSeconds: 5.0,
		Confidence:          0.8,
		DomainID:            domain.ID,
	}, now)
	if err != nil {
		t.Fatalf("applyInteraction: %v", err)
	}

	// Mastery must climb above the bootstrap default of 0.1.
	if cs.PMastery <= 0.1 {
		t.Fatalf("expected PMastery > 0.1 after a success, got %f", cs.PMastery)
	}

	// FSRS card must have advanced: one review recorded and a future review
	// scheduled.
	if cs.Reps != 1 {
		t.Fatalf("expected reps=1 after one success, got %d", cs.Reps)
	}
	if cs.LastReview == nil || !cs.LastReview.Equal(now) {
		t.Fatalf("expected LastReview=%v, got %v", now, cs.LastReview)
	}
	if cs.NextReview == nil || !cs.NextReview.After(now) {
		t.Fatalf("expected a future NextReview, got %v", cs.NextReview)
	}

	// The write-back must have been persisted, not just returned.
	stored, err := store.GetConceptStateInDomain(context.Background(), "L_owner", domain.ID, "a")
	if err != nil {
		t.Fatalf("GetConceptState: %v", err)
	}
	if stored.PMastery != cs.PMastery || stored.Reps != cs.Reps {
		t.Fatalf("persisted state diverges from returned: stored=%+v returned PMastery=%f reps=%d",
			stored, cs.PMastery, cs.Reps)
	}

	// The interaction row must have been created.
	recents, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
	if err != nil {
		t.Fatalf("GetRecentInteractionsByLearner: %v", err)
	}
	if len(recents) != 1 || !recents[0].Success || recents[0].Concept != "a" {
		t.Fatalf("expected 1 successful interaction on 'a', got %+v", recents)
	}
}

// An incorrect answer on an already-mastered concept must drop mastery and
// register the failure (an FSRS Again rating lapses the card).
func TestApplyInteraction_IncorrectAnswer_MasteryDown(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math") // concepts: ["a","b"]

	// Seed a high-mastery concept state in the Review FSRS state so the
	// failure has clear room to move things downward.
	now := time.Now().UTC()
	last := now.Add(-48 * time.Hour)
	seed := &models.ConceptState{
		LearnerID:     "L_owner",
		DomainID:      domain.ID,
		Concept:       "a",
		Stability:     10.0,
		Difficulty:    5.0,
		ElapsedDays:   2,
		ScheduledDays: 5,
		Reps:          4,
		Lapses:        0,
		CardState:     "review",
		LastReview:    &last,
		PMastery:      0.9,
		PLearn:        0.15,
		PForget:       0.05,
		PSlip:         0.1,
		PGuess:        0.2,
	}
	if err := store.UpsertConceptState(context.Background(), seed); err != nil {
		t.Fatalf("seed concept state: %v", err)
	}

	cs, _, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept:             "a",
		ActivityType:        "RECALL_EXERCISE",
		Success:             false, // → FSRS rating Again
		ResponseTimeSeconds: 30.0,
		Confidence:          0.2,
		DomainID:            domain.ID,
	}, now)
	if err != nil {
		t.Fatalf("applyInteraction: %v", err)
	}

	// A failure on a near-mastered concept must lower the BKT estimate.
	if cs.PMastery >= seed.PMastery {
		t.Fatalf("expected PMastery to drop below %f after a failure, got %f", seed.PMastery, cs.PMastery)
	}

	// FSRS records the attempt (Reps bumped) and an Again rating lapses the
	// review card.
	if cs.Reps != seed.Reps+1 {
		t.Fatalf("expected reps=%d after the failed review, got %d", seed.Reps+1, cs.Reps)
	}
	if cs.Lapses <= seed.Lapses {
		t.Fatalf("expected lapses to increase on an Again rating, got %d (was %d)", cs.Lapses, seed.Lapses)
	}

	// The drop must be persisted.
	stored, err := store.GetConceptStateInDomain(context.Background(), "L_owner", domain.ID, "a")
	if err != nil {
		t.Fatalf("GetConceptState: %v", err)
	}
	if stored.PMastery != cs.PMastery {
		t.Fatalf("persisted PMastery=%f diverges from returned %f", stored.PMastery, cs.PMastery)
	}

	// The failed interaction row must have been recorded.
	recents, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
	if err != nil {
		t.Fatalf("GetRecentInteractionsByLearner: %v", err)
	}
	if len(recents) != 1 || recents[0].Success {
		t.Fatalf("expected 1 failed interaction on 'a', got %+v", recents)
	}
}

func TestApplyInteraction_NonEvidenceActivitiesDoNotMutateCognitiveState(t *testing.T) {
	for _, activityType := range []models.ActivityType{
		models.ActivityRest,
		models.ActivitySetupDomain,
		models.ActivityCloseSession,
	} {
		t.Run(string(activityType), func(t *testing.T) {
			store, deps := setupToolsTest(t)
			domain := makeOwnerDomain(t, store, "L_owner", "math-"+string(activityType))

			now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			lastReview := now.Add(-48 * time.Hour)
			seed := &models.ConceptState{
				LearnerID:     "L_owner",
				DomainID:      domain.ID,
				Concept:       "a",
				PMastery:      0.73,
				PLearn:        0.15,
				PForget:       0.05,
				PSlip:         0.10,
				PGuess:        0.20,
				Stability:     8.5,
				Difficulty:    6.25,
				ElapsedDays:   2,
				ScheduledDays: 7,
				Reps:          4,
				Lapses:        1,
				CardState:     "review",
				LastReview:    &lastReview,
				Theta:         0.42,
			}
			if err := store.UpsertConceptState(context.Background(), seed); err != nil {
				t.Fatalf("seed concept state: %v", err)
			}

			_, _, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
				Concept:      "a",
				DomainID:     domain.ID,
				ActivityType: string(activityType),
				Success:      true,
				Confidence:   1,
			}, now)
			if err == nil || !strings.Contains(err.Error(), "not learner evidence") {
				t.Fatalf("applyInteraction error = %v, want explicit non-evidence rejection", err)
			}

			got, err := store.GetConceptStateInDomain(context.Background(), "L_owner", domain.ID, "a")
			if err != nil {
				t.Fatalf("reload concept state: %v", err)
			}
			if got.PMastery != seed.PMastery ||
				got.Stability != seed.Stability ||
				got.Difficulty != seed.Difficulty ||
				got.ElapsedDays != seed.ElapsedDays ||
				got.ScheduledDays != seed.ScheduledDays ||
				got.Reps != seed.Reps ||
				got.Lapses != seed.Lapses ||
				got.Theta != seed.Theta ||
				got.CardState != seed.CardState ||
				got.LastReview == nil ||
				!got.LastReview.Equal(lastReview) {
				t.Fatalf("non-evidence activity mutated BKT/FSRS/IRT: before=%+v after=%+v", seed, got)
			}

			interactions, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
			if err != nil {
				t.Fatalf("list interactions: %v", err)
			}
			if len(interactions) != 0 {
				t.Fatalf("non-evidence activity persisted as learner evidence: %+v", interactions)
			}
		})
	}
}
