// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"encoding/json"
	"os"
	"testing"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise the identifiers the curriculum actually generates, all the way
// through a frozen decision, committed response and untrusted host grading.
func TestAssessmentUsesCurriculumCriterionIdentity(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			if driver == "postgres" && os.Getenv("TUTOR_TEST_PG_DSN") == "" {
				t.Skip("set TUTOR_TEST_PG_DSN to exercise PostgreSQL criterion identities")
			}
			s, deps := setupToolsTest(t)
			learnerID := "L_owner"
			if driver == "postgres" {
				s = postgresToolsTestStore(t, os.Getenv("TUTOR_TEST_PG_DSN"))
				learner, err := s.CreateLearner(t.Context(), "criterion@example.test", "hash", "Learn", "")
				if err != nil {
					t.Fatal(err)
				}
				learnerID, deps.Store = learner.ID, s
			}
			created := callTool(t, deps, registerInitDomain, learnerID, "init_domain", map[string]any{
				"name": "Equations", "concepts": []string{"equivalence"}, "prerequisites": map[string][]string{},
				"details": map[string]any{"equivalence": map[string]any{
					"label": "Preserve equality", "criteria": []map[string]any{{"description": "Apply the same operation to both sides"}},
				}},
			})
			if created.IsError {
				t.Fatal(resultText(created))
			}
			domainID := decodeResult(t, created)["domain_id"].(string)
			next := callNextActivity(t, deps, learnerID, map[string]any{"domain_id": domainID})
			if next.IsError {
				t.Fatal(resultText(next))
			}
			activity := decodeResult(t, next)
			decision, err := s.GetPedagogicalDecision(t.Context(), models.LegacyPrincipal(learnerID).TenantScope(), decisionIDOf(activity))
			if err != nil {
				t.Fatal(err)
			}
			criterion := decision.Contract.Competency.Criteria[0]
			rubric, _ := json.Marshal(map[string]any{
				"criteria": []map[string]any{{"id": criterion.ID, "description": criterion.Description, "max_score": 1}}, "passing_score": 1,
			})
			register := func(server *mcp.Server, deps *Deps) {
				addIdempotencyMiddleware(server, deps)
				registerPrepareAssessmentAttempt(server, deps)
			}
			args := map[string]any{
				"domain_id": domainID, "concept": decision.Contract.TargetConcept, "session_id": decision.SessionID, "decision_id": decision.ID,
				"activity_type": string(decision.Contract.RecommendedActivityType), "observable": criterion.Description,
				"task_text": "Solve x + 3 = 7, showing equivalent transformations.", "rubric": json.RawMessage(rubric), "idempotency_key": "freeze-criterion-once",
			}
			prepared := callTool(t, deps, register, learnerID, "prepare_assessment_attempt", args)
			if prepared.IsError {
				t.Fatalf("server rejected its own curriculum criterion: %s", resultText(prepared))
			}
			replayed := callTool(t, deps, register, learnerID, "prepare_assessment_attempt", args)
			if replayed.IsError || resultText(prepared) != resultText(replayed) {
				t.Fatal("retry did not return the same frozen attempt")
			}
			attemptID := decodeResult(t, prepared)["attempt_id"].(string)
			if result := callTool(t, deps, registerSubmitAssessmentAttempt, learnerID, "submit_assessment_attempt", map[string]any{
				"attempt_id": attemptID, "learner_response": "x + 3 - 3 = 7 - 3, so x = 4",
			}); result.IsError {
				t.Fatal(resultText(result))
			}
			grade := assessmentEvaluationArgs(domainID, decision.SessionID, attemptID, true)
			grade["concept"], grade["activity_type"] = decision.Contract.TargetConcept, string(decision.Contract.RecommendedActivityType)
			score, _ := json.Marshal(map[string]any{"criteria_scores": []map[string]any{{"id": criterion.ID, "score": 1, "evidence": "Subtracts 3 from both sides"}}})
			grade["rubric_score"] = json.RawMessage(score)
			if result := callTool(t, deps, registerRecordInteraction, learnerID, "record_interaction", grade); result.IsError {
				t.Fatal(resultText(result))
			}
			attempt, err := s.GetAssessmentAttempt(t.Context(), learnerID, attemptID)
			if err != nil || !attempt.Passed || attempt.TrustedEvaluation || attempt.Status != models.AssessmentAttemptEvaluated {
				t.Fatalf("criterion identity changed the grading/trust contract: %v", err)
			}
			var frozen models.AssessmentRubric
			if json.Unmarshal([]byte(attempt.RubricJSON), &frozen) != nil || frozen.Criteria[0].ID != criterion.ID {
				t.Fatal("stored rubric lost the original criterion identity")
			}
		})
	}
}
