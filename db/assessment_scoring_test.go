// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const completeBoundScore = `{"criteria_scores":[{"id":"correctness","score":1,"evidence":"The committed response meets the criterion."}]}`

func TestBoundAssessmentStoreRevalidatesFrozenScoring(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "scoring")
	a := f.attempt(t, "bound-score")
	a.PassingScore = 0.5
	if err := s.CreateAssessmentAttempt(ctx, a); err == nil {
		t.Fatal("storage accepted a passing rule different from the rubric")
	}
	a.PassingScore = 1
	validRubric := a.RubricJSON
	a.RubricJSON = `{"criteria":[{"id":"correctness","description":"Correct.","max_score":0,"max_score":1}],"passing_score":1}`
	if err := s.CreateAssessmentAttempt(ctx, a); err == nil {
		t.Fatal("storage accepted an ambiguous bound rubric")
	}
	a.RubricJSON = validRubric
	if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.SubmitAssessmentAttempt(ctx, "L1", a.ID, "Committed answer", "hash", now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, scores string
		total        float64
		passed       bool
	}{
		{"wrong pass", completeBoundScore, 1, false},
		{"wrong total", completeBoundScore, 0, true},
		{"non-finite total", completeBoundScore, math.NaN(), true},
		{"contradictory aggregate", `{"criteria_scores":[{"id":"correctness","score":1,"evidence":"Observed."}],"total":0}`, 1, true},
		{"missing evidence", `{"criteria_scores":[{"id":"correctness","score":1}]}`, 1, true},
		{"numeric string", `{"criteria_scores":[{"id":"correctness","score":"1","evidence":"Observed."}]}`, 1, true},
		{"duplicate score", `{"criteria_scores":[{"id":"correctness","score":0,"score":1,"evidence":"Observed."}]}`, 1, true},
		{"claim trust", `{"criteria_scores":[{"id":"correctness","score":1,"evidence":"Observed."}],"trusted_evaluation":true}`, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.CompleteAssessmentEvaluation(ctx, "L1", a.ID, tc.scores, "host", models.EvaluationMethodHostLLM, "{}", tc.total, tc.passed, now); err == nil {
				t.Fatal("storage accepted invalid scoring")
			}
			got, err := s.GetAssessmentAttempt(ctx, "L1", a.ID)
			if err != nil || got.Status != models.AssessmentAttemptSubmitted || got.EvaluatedAt != nil || got.RubricScoreJSON != "" {
				t.Fatalf("rejected scoring altered attempt: %+v err=%v", got, err)
			}
		})
	}
	if err := s.CompleteAssessmentEvaluation(ctx, "L2", a.ID, completeBoundScore, "host", models.EvaluationMethodHostLLM, "{}", 1, true, now); err == nil {
		t.Fatal("different learner completed attempt")
	}
	if err := s.CompleteAssessmentEvaluation(ctx, "L1", a.ID, completeBoundScore, "human-claim", models.EvaluationMethodHumanReview, "{}", 1, true, now); err == nil {
		t.Fatal("public persistence minted human review")
	}
	if err := s.CompleteAssessmentEvaluation(ctx, "L1", a.ID, completeBoundScore, "host", models.EvaluationMethodHostLLM, "{}", 1, true, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAssessmentAttempt(ctx, "L1", a.ID)
	if err != nil || got.Status != models.AssessmentAttemptEvaluated || got.TrustedEvaluation || !got.Passed || got.Score != 1 || got.RubricJSON != validRubric {
		t.Fatalf("valid scoring or frozen rubric altered: %+v err=%v", got, err)
	}
}

func TestBoundAssessmentStoreRejectRollsBackComposedWrites(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "rollback-score")
	a := f.attempt(t, "rollback-score")
	if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.SubmitAssessmentAttempt(ctx, "L1", a.ID, "Committed answer", "hash", now); err != nil {
		t.Fatal(err)
	}
	err := s.WithTx(ctx, func(tx storeport.Store) error {
		if _, err := tx.GetAssessmentAttemptForUpdate(ctx, "L1", a.ID); err != nil {
			return err
		}
		if err := tx.CreateInteraction(ctx, &models.Interaction{LearnerID: "L1", DomainID: f.domain.ID, Concept: "a", ActivityType: "PRACTICE", CreatedAt: now}); err != nil {
			return err
		}
		return tx.CompleteAssessmentEvaluation(ctx, "L1", a.ID, completeBoundScore, "host", models.EvaluationMethodHostLLM, "{}", 1, false, now)
	})
	if err == nil {
		t.Fatal("contradictory evaluation committed")
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM interactions WHERE learner_id = ? AND domain_id = ?`, "L1", f.domain.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected evaluation left interaction: count=%d err=%v", count, err)
	}
	got, err := s.GetAssessmentAttempt(ctx, "L1", a.ID)
	if err != nil || got.Status != models.AssessmentAttemptSubmitted {
		t.Fatalf("attempt=%+v err=%v", got, err)
	}
}

func TestBoundAssessmentStoreCompletionIsSingleUse(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "score-once")
	a := f.attempt(t, "score-once")
	if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.SubmitAssessmentAttempt(ctx, "L1", a.ID, "Committed answer", "hash", now); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- s.CompleteAssessmentEvaluation(ctx, "L1", a.ID, completeBoundScore, "host", models.EvaluationMethodHostLLM, "{}", 1, true, now)
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("completed %d times", accepted)
	}
}
