// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestPostgresEvidenceHotPathIndexes(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("set TUTOR_TEST_PG_DSN")
	}
	store := setupTestDB(t)
	indexes := map[string][]string{
		"idx_assessment_attempts_evaluated_recent": {
			"learner_id", "domain_id", "concept_id", "evaluated_at DESC", "status", "submitted_at IS NOT NULL",
		},
		"idx_interactions_domain_recent": {
			"learner_id", "domain_id", "created_at DESC",
		},
	}
	for name, fragments := range indexes {
		var definition string
		if err := store.queryRow(context.Background(), `SELECT indexdef FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = ?`, name).Scan(&definition); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(definition, fragment) {
				t.Fatalf("%s missing %q: %s", name, fragment, definition)
			}
		}
	}
}

func TestEvidenceBatchReadsMatchSingleConceptReads(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	domain, err := store.CreateDomain(ctx, "L1", "evidence batch", "", models.KnowledgeSpace{
		Concepts: []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	otherDomain, err := store.CreateDomain(ctx, "L1", "other evidence", "", models.KnowledgeSpace{
		Concepts: []string{"alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDomainHighStakes(ctx, domain.ID, "L1"); err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for conceptIndex, concept := range []string{"alpha", "beta"} {
		for i := 0; i < 3; i++ {
			method := models.EvaluationMethodDeterministic
			if i == 2 {
				method = models.EvaluationMethodHumanReview
			}
			seedBatchAssessment(
				t, store, domain.ID, concept,
				base.Add(time.Duration(conceptIndex*10+i)*time.Minute), method,
			)
		}
		for i := 0; i < 2; i++ {
			seedBatchTransfer(t, store, domain.ID, concept, "context", float64(i)/2, base.Add(time.Duration(conceptIndex*10+i)*time.Minute))
		}
	}
	// Same learner and concept in another domain must never enter the batch.
	seedBatchAssessment(t, store, otherDomain.ID, "alpha", base.Add(24*time.Hour), models.EvaluationMethodHumanReview)
	seedBatchTransfer(t, store, otherDomain.ID, "alpha", "other-domain", 1, base.Add(24*time.Hour))

	concepts := []string{"alpha", "beta", "alpha", ""}
	batchedTransfers, err := store.GetTransferScoresBatchInDomain(ctx, "L1", domain.ID, concepts)
	if err != nil {
		t.Fatalf("batch transfers: %v", err)
	}
	batchedAssessments, err := store.GetEvaluatedAssessmentAttemptsBatchInDomain(ctx, "L1", domain.ID, concepts, 2)
	if err != nil {
		t.Fatalf("batch assessments: %v", err)
	}

	for _, concept := range []string{"alpha", "beta"} {
		wantTransfers, err := store.GetTransferScoresInDomain(ctx, "L1", domain.ID, concept)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(batchedTransfers[concept], wantTransfers) {
			t.Fatalf("%s transfer batch differs from single read:\nbatch=%+v\nsingle=%+v", concept, batchedTransfers[concept], wantTransfers)
		}

		wantAssessments, err := store.GetEvaluatedAssessmentAttemptsInDomain(ctx, "L1", domain.ID, concept, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(batchedAssessments[concept], wantAssessments) {
			t.Fatalf("%s assessment batch differs from single read:\nbatch=%+v\nsingle=%+v", concept, batchedAssessments[concept], wantAssessments)
		}
		if len(batchedAssessments[concept]) != 2 {
			t.Fatalf("%s assessment limit=%d, want 2 per concept", concept, len(batchedAssessments[concept]))
		}
		// On a high-stakes domain the newest human review remains trusted,
		// while deterministic evaluations are downgraded by both read paths.
		if !batchedAssessments[concept][0].TrustedEvaluation || batchedAssessments[concept][1].TrustedEvaluation {
			t.Fatalf("%s effective trust policy changed in batch: %+v", concept, batchedAssessments[concept])
		}
	}
}

func seedBatchTransfer(t *testing.T, store *Store, domainID, concept, contextType string, score float64, createdAt time.Time) {
	t.Helper()
	if _, err := store.exec(context.Background(), `INSERT INTO transfer_records
		(learner_id, domain_id, concept_id, context_type, score, session_id, created_at)
		VALUES (?, ?, ?, ?, ?, '', ?)`,
		"L1", domainID, concept, contextType, score, createdAt,
	); err != nil {
		t.Fatalf("seed transfer %s/%s: %v", domainID, concept, err)
	}
}

func seedBatchAssessment(t *testing.T, store *Store, domainID, concept string, evaluatedAt time.Time, trustedMethod models.EvaluationMethod) {
	t.Helper()
	ctx := context.Background()
	id := domainID + "-" + concept + "-" + evaluatedAt.Format("150405.000000")
	attempt := &models.AssessmentAttempt{
		ID: id, LearnerID: "L1", DomainID: domainID, ConceptID: concept,
		ActivityID: id + "-activity", ActivityVersion: 1,
		ActivityType: string(models.ActivityRecall), Observable: "answer",
		TaskText: "task", RubricJSON: `{"criteria":[{"id":"correct"}]}`,
		PassingScore: 0.6, CreatedAt: evaluatedAt.Add(-2 * time.Minute),
	}
	if err := store.CreateAssessmentAttempt(ctx, attempt); err != nil {
		t.Fatalf("create assessment %s: %v", id, err)
	}
	if err := store.SubmitAssessmentAttempt(ctx, "L1", id, "answer", "", evaluatedAt.Add(-time.Minute)); err != nil {
		t.Fatalf("submit assessment %s: %v", id, err)
	}
	if err := store.CompleteAssessmentEvaluation(
		ctx, "L1", id, `{"correct":1}`, "fixture", models.EvaluationMethodHostLLM,
		"", 1, true, evaluatedAt,
	); err != nil {
		t.Fatalf("evaluate assessment %s: %v", id, err)
	}
	if _, err := store.exec(ctx, `UPDATE assessment_attempts
		SET trusted_evaluation = 1, evaluation_method = ? WHERE id = ?`,
		string(trustedMethod), id,
	); err != nil {
		t.Fatalf("set trusted method for %s: %v", id, err)
	}
}
