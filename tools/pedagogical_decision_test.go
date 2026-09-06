// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestPedagogicalDecisionBindingLifecycle(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "decision-test")
	for _, concept := range domain.Graph.Concepts {
		if err := store.InsertConceptStateIfNotExists(context.Background(), models.NewConceptStateInDomain("L_owner", domain.ID, concept)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpdateDomainPhase(context.Background(), domain.ID, models.PhaseDiagnostic, 0.469, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	activity := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{"domain_id": domain.ID})
	if activity.IsError {
		t.Fatal(resultText(activity))
	}
	out := decodeResult(t, activity)
	id, _ := out["decision_id"].(string)
	if id == "" {
		t.Fatalf("no decision: %v", out)
	}
	decision, err := store.GetPedagogicalDecision(context.Background(), models.LegacyPrincipal("L_owner").TenantScope(), id)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Contract.Competency == nil || decision.Contract.PolicyVersion == "" || decision.CurriculumVersion != domain.GraphVersion {
		t.Fatalf("incomplete contract: %+v", decision)
	}
	if decision.Contract.RecommendedActivityType != models.ActivityDiagnosticAssessment || decision.Contract.BKTUpdateMode != models.BKTObservationOnly {
		t.Fatalf("cold diagnostic must freeze an observation-only contract: %+v", decision.Contract)
	}
	if contract := out["pedagogical_contract"].(map[string]any); contract["bkt_update_mode"] != string(decision.Contract.BKTUpdateMode) {
		t.Fatalf("emitted and frozen modes differ: %v", contract)
	}
	if _, err := store.GetPedagogicalDecision(context.Background(), models.LegacyPrincipal("L_attacker").TenantScope(), id); err == nil {
		t.Fatal("cross-learner decision exposed")
	}
	foreign := models.LegacyPrincipal("L_owner").TenantScope()
	foreign.TenantID = "tenant_not_owner"
	if _, err := store.GetPedagogicalDecision(context.Background(), foreign, id); err == nil {
		t.Fatal("cross-tenant decision exposed")
	}
	if _, err := store.RawDB().Exec(`UPDATE pedagogical_decisions SET snapshot_json = '{}' WHERE id = ?`, id); err == nil {
		t.Fatal("decision mutable")
	}
	args := map[string]any{
		"domain_id": domain.ID, "session_id": decision.SessionID, "decision_id": id,
		"concept": decision.Contract.TargetConcept, "activity_type": string(decision.Contract.RecommendedActivityType),
		"observable": "Explain the result", "task_text": "Generated task frozen before the learner responds",
		"rubric_json": `{"criteria":[{"id":"correctness","description":"Correct result and explanation","max_score":1,"anchors":[{"score":1,"description":"Both correct"}]}],"passing_score":1,"answer_key":"Generated reference explanation"}`,
	}
	wrong := map[string]any{}
	for k, v := range args {
		wrong[k] = v
	}
	wrong["activity_type"] = "TRANSFER_PROBE"
	if wrong["activity_type"] == args["activity_type"] {
		wrong["activity_type"] = "PRACTICE"
	}
	if res := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", wrong); !res.IsError {
		t.Fatal("activity mismatch accepted")
	}
	prepared := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", args)
	if prepared.IsError {
		t.Fatal(resultText(prepared))
	}
	preparedOut := decodeResult(t, prepared)
	if preparedOut["binding_status"] != "decision_bound" {
		t.Fatal(preparedOut)
	}
	attemptID := preparedOut["attempt_id"].(string)
	attempt, err := store.GetAssessmentAttempt(context.Background(), "L_owner", attemptID)
	if err != nil {
		t.Fatal(err)
	}
	var competency models.CurriculumConcept
	if err := json.Unmarshal([]byte(attempt.CurriculumConceptJSON), &competency); err != nil || competency.ID != decision.Contract.Competency.ID {
		t.Fatalf("snapshot lost: %+v %v", attempt, err)
	}
	if attempt.DecisionID != id || attempt.CurriculumVersion != decision.CurriculumVersion {
		t.Fatal("binding lost")
	}
	if _, err := store.RawDB().Exec(`UPDATE assessment_attempts SET decision_id = NULL WHERE id = ?`, attemptID); err == nil {
		t.Fatal("binding mutable")
	}
	if res := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", args); !res.IsError {
		t.Fatal("decision reused for a second attempt")
	}
	if res := callTool(t, deps, registerSubmitAssessmentAttempt, "L_owner", "submit_assessment_attempt", map[string]any{"attempt_id": attemptID, "learner_response": "My answer"}); res.IsError {
		t.Fatal(resultText(res))
	}
	eval := assessmentEvaluationArgs(domain.ID, decision.SessionID, attemptID, true)
	eval["concept"], eval["activity_type"] = args["concept"], args["activity_type"]
	eval["rubric_score_json"] = `{"criteria_scores":[{"id":"correctness","score":1,"evidence":"The committed answer explains the result"}]}`
	graded := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", eval)
	if graded.IsError {
		t.Fatal(resultText(graded))
	}
	obs := decodeResult(t, graded)["observation"].(map[string]any)
	if obs["bkt_update_mode"] != string(decision.Contract.BKTUpdateMode) || obs["bkt_transition_applied"] != false {
		t.Fatalf("diagnostic evaluation violated the frozen BKT policy: %v", obs)
	}
	attempt, err = store.GetAssessmentAttempt(context.Background(), "L_owner", attemptID)
	if err != nil || attempt.TrustedEvaluation || !attempt.Passed {
		t.Fatalf("bound must remain untrusted host evaluation: %+v %v", attempt, err)
	}
	before, err := store.GetConceptStateInDomain(context.Background(), "L_owner", domain.ID, decision.Contract.TargetConcept)
	if err != nil {
		t.Fatal(err)
	}
	if res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", eval); !res.IsError {
		t.Fatal("diagnostic response evaluated twice")
	}
	after, err := store.GetConceptStateInDomain(context.Background(), "L_owner", domain.ID, decision.Contract.TargetConcept)
	if err != nil || after.PMastery != before.PMastery || after.Reps != before.Reps {
		t.Fatalf("duplicate evaluation changed learner state: %+v -> %+v, err=%v", before, after, err)
	}
}

func TestPedagogicalDecisionRejectsStaleCurriculum(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "stale-decision")
	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{"domain_id": domain.ID})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	id := decodeResult(t, res)["decision_id"].(string)
	decision, err := store.GetPedagogicalDecision(context.Background(), models.LegacyPrincipal("L_owner").TenantScope(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a concurrent committed version bump; preparation must fail closed.
	if _, err := store.RawDB().Exec(`UPDATE domains SET graph_version = graph_version + 1 WHERE id = ?`, domain.ID); err != nil {
		t.Fatal(err)
	}
	res = callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
		"domain_id": domain.ID, "session_id": decision.SessionID, "decision_id": id,
		"concept": decision.Contract.TargetConcept, "activity_type": string(decision.Contract.RecommendedActivityType),
		"observable": "Explain", "task_text": "Generated task", "rubric_json": `{"criteria":[{"id":"c","description":"Correct","max_score":1}],"passing_score":1}`,
	})
	if !res.IsError {
		t.Fatal("stale decision accepted")
	}
}
