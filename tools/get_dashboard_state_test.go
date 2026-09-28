// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/engine"
	"tutor-mcp/models"
)

// TestGetDashboardState_NoActiveDomain_UsesCanonicalShape asserts that when a
// learner with no domain calls get_dashboard_state, the response uses the
// canonical needs_domain_setup payload (Issue #33) instead of the previous
// previous French error string instead of the canonical structured payload. This keeps
// the chat-side tool surface uniform so the LLM can branch on a single signal
// regardless of which tool it called.
func TestGetDashboardState_NoActiveDomain_UsesCanonicalShape(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetDashboardState, "L_owner", "get_dashboard_state", map[string]any{})
	if res.IsError {
		t.Fatalf("expected canonical (non-error) payload, got error: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if got, _ := out["needs_domain_setup"].(bool); !got {
		t.Fatalf("expected needs_domain_setup=true, got %v", out)
	}
	if reason, _ := out["reason"].(string); reason == "" {
		t.Fatalf("expected non-empty reason field, got %v", out)
	}
	if next, _ := out["next_action_for_llm"].(string); next == "" {
		t.Fatalf("expected non-empty next_action_for_llm field, got %v", out)
	}
}

// TestGetDashboardState_ColorEnumIsEnglish drives the dashboard into the
// retention-alert branch where the color enum was previously the French
// "rouge" while its sibling already used the English "orange". The fix
// aligns the enum to a consistent English vocabulary ("red"/"orange").
//
// To trigger the red branch we seed a ConceptState in "review" with a low
// stability and a LastReview far in the past, so Retrievability falls below
// 0.30 (review concept "a", stability=1.0, last_review=60 days ago →
// retention ≈ 0.26 < 0.30 → color=red).
func TestGetDashboardState_ColorEnumIsEnglish(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math") // concepts: a, b

	last := time.Now().UTC().Add(-60 * 24 * time.Hour)
	seed := &models.ConceptState{
		LearnerID:  "L_owner",
		Concept:    "a",
		Stability:  1.0,
		Difficulty: 5.0,
		Reps:       3,
		CardState:  "review",
		LastReview: &last,
		PMastery:   0.4,
	}
	if err := store.UpsertConceptState(context.Background(), seed); err != nil {
		t.Fatalf("seed concept state: %v", err)
	}

	res := callTool(t, deps, registerGetDashboardState, "L_owner", "get_dashboard_state", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if _, ok := out["global_progress_percent"].(float64); !ok {
		t.Fatalf("expected global_progress_percent key, got %v", out)
	}
	if _, ok := out["global_progress"]; ok {
		t.Fatalf("did not expect legacy global_progress alias in result: %v", out)
	}
	domains, ok := out["domains"].([]any)
	if !ok || len(domains) == 0 {
		t.Fatalf("expected non-empty domains array, got %v", out)
	}
	dom, _ := domains[0].(map[string]any)
	alerts, _ := dom["retention_alerts"].([]any)
	if len(alerts) == 0 {
		t.Fatalf("expected at least one retention_alert (seeded retention < 0.30), got %v", dom)
	}

	// Find the alert for concept "a" and assert its color is the English
	// "red" (was previously the French "rouge").
	var found bool
	for _, raw := range alerts {
		alert, _ := raw.(map[string]any)
		if alert["concept"] != "a" {
			continue
		}
		found = true
		color, _ := alert["color"].(string)
		if color == "rouge" {
			t.Fatalf("expected color=red, got the legacy French value %q", color)
		}
		if color != "red" {
			t.Fatalf("expected color=red for retention < 0.30, got %q (alert=%v)", color, alert)
		}
	}
	if !found {
		t.Fatalf("no retention_alert for concept 'a' in alerts=%v", alerts)
	}
}

func TestGetDashboardState_DoesNotLabelHighEstimateAsMasteredProgress(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "evidence ladder")
	lastReview := time.Now().UTC().Add(-time.Hour)
	state := models.NewConceptStateInDomain("L_owner", domain.ID, "a")
	state.PMastery = 0.95
	state.CardState = "review"
	state.Stability = 30
	state.LastReview = &lastReview
	if err := store.UpsertConceptState(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerGetDashboardState, "L_owner", "get_dashboard_state", map[string]any{
		"domain_id": domain.ID,
	})
	if res.IsError {
		t.Fatalf("dashboard failed: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["total_estimated"] != float64(1) || out["total_demonstrated"] != float64(0) {
		t.Fatalf("unexpected global evidence counts: %v", out)
	}
	if out["total_mastered"] != float64(0) || out["global_progress_percent"] != float64(0) {
		t.Fatalf("high BKT estimate was mislabeled as mastered progress: %v", out)
	}
	if out["global_estimated_progress_percent"] != float64(50) {
		t.Fatalf("estimated progress=%v, want 50", out["global_estimated_progress_percent"])
	}
	domains, ok := out["domains"].([]any)
	if !ok || len(domains) != 1 {
		t.Fatalf("domains=%T %#v", out["domains"], out["domains"])
	}
	dashboard := domains[0].(map[string]any)
	if dashboard["mastered_count"] != float64(0) || dashboard["demonstrated_count"] != float64(0) || dashboard["estimated_count"] != float64(1) {
		t.Fatalf("unexpected domain evidence counts: %v", dashboard)
	}
	concepts := dashboard["concepts"].([]any)
	for _, raw := range concepts {
		concept := raw.(map[string]any)
		if concept["concept"] == "a" && concept["status"] != string(engine.MasteryStageEstimated) {
			t.Fatalf("concept a status=%v, want estimated", concept["status"])
		}
	}
}

// seedDecisionBoundHostEvaluation runs the full prepare -> submit ->
// record_interaction protocol against a frozen pedagogical decision so the
// host evaluation is decision-bound, exactly as production records it.
func seedDecisionBoundHostEvaluation(t *testing.T, store *db.Store, deps *Deps, domain *models.Domain, decisionID string) {
	t.Helper()
	ctx := context.Background()
	curriculum, err := store.EnsureCurriculumBaseline(ctx, "L_owner", domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session, err := store.OpenLearningSession(ctx, "L_owner", domain.ID, "", now)
	if err != nil {
		t.Fatal(err)
	}
	decision := &models.PedagogicalDecision{
		ID: decisionID, LearnerID: "L_owner", DomainID: domain.ID, SessionID: session.ID,
		CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion, CreatedAt: now,
		Contract: models.PedagogicalContract{DecisionID: decisionID, CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion, TargetConcept: "a", RecommendedActivityType: models.ActivityMasteryChallenge, Competency: curriculumCompetency(curriculum, "a")},
	}
	if err := store.CreatePedagogicalDecision(ctx, models.LegacyPrincipal("L_owner").TenantScope(), decision); err != nil {
		t.Fatal(err)
	}
	prepared := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
		"domain_id": domain.ID, "session_id": session.ID, "decision_id": decision.ID,
		"concept": "a", "activity_type": "MASTERY_CHALLENGE", "observable": "Apply the competency.",
		"task_text":   "Generated task.",
		"rubric_json": `{"criteria":[{"id":"x","description":"Defined criterion.","max_score":1}],"passing_score":0.6}`,
	})
	if prepared.IsError {
		t.Fatalf("prepare: %s", resultText(prepared))
	}
	attemptID := decodeResult(t, prepared)["attempt_id"].(string)
	submitted := callTool(t, deps, registerSubmitAssessmentAttempt, "L_owner", "submit_assessment_attempt", map[string]any{"attempt_id": attemptID, "learner_response": "Committed response."})
	if submitted.IsError {
		t.Fatalf("submit: %s", resultText(submitted))
	}
	args := assessmentEvaluationArgs(domain.ID, session.ID, attemptID, true)
	// Only assessment activity types can demonstrate; routine practice cannot.
	args["activity_type"] = "MASTERY_CHALLENGE"
	args["rubric_score_json"] = `{"criteria_scores":[{"id":"x","score":1,"evidence":"Observed."}]}`
	accepted := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", args)
	if accepted.IsError {
		t.Fatalf("evaluate: %s", resultText(accepted))
	}
}

func dashboardDemonstrated(t *testing.T, deps *Deps) (float64, float64) {
	t.Helper()
	res := callTool(t, deps, registerGetDashboardState, "L_owner", "get_dashboard_state", map[string]any{})
	if res.IsError {
		t.Fatalf("dashboard: %s", resultText(res))
	}
	out := decodeResult(t, res)
	return out["total_demonstrated"].(float64), out["global_progress_percent"].(float64)
}

func TestGetDashboardState_DecisionBoundHostEvaluationCountsAsDemonstrated(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	seedDecisionBoundHostEvaluation(t, store, deps, domain, "decision-demonstrated")

	demonstrated, progress := dashboardDemonstrated(t, deps)
	if demonstrated != 1 || progress <= 0 {
		t.Fatalf("decision-bound host evaluation must demonstrate the concept: demonstrated=%v progress=%v", demonstrated, progress)
	}

	// The raw row stays untrusted: trust is derived at read time only.
	var raw int
	if err := store.RawDB().QueryRow(`SELECT trusted_evaluation FROM assessment_attempts WHERE decision_id = ?`, "decision-demonstrated").Scan(&raw); err != nil || raw != 0 {
		t.Fatalf("raw trusted_evaluation must remain 0, got %d err=%v", raw, err)
	}
}

func TestGetDashboardState_HostEvaluationNotDemonstratedWhenPolicyOffOrHighStakes(t *testing.T) {
	t.Run("policy off", func(t *testing.T) {
		store, deps := setupToolsTest(t)
		store.SetHostLLMDemonstrationPolicy(false)
		domain := makeOwnerDomain(t, store, "L_owner", "math")
		seedDecisionBoundHostEvaluation(t, store, deps, domain, "decision-policy-off")
		if demonstrated, progress := dashboardDemonstrated(t, deps); demonstrated != 0 || progress != 0 {
			t.Fatalf("policy off must keep host evaluations untrusted: demonstrated=%v progress=%v", demonstrated, progress)
		}
	})
	t.Run("high stakes", func(t *testing.T) {
		store, deps := setupToolsTest(t)
		domain := makeOwnerDomain(t, store, "L_owner", "math")
		if err := store.MarkDomainHighStakes(context.Background(), domain.ID, "L_owner"); err != nil {
			t.Fatal(err)
		}
		seedDecisionBoundHostEvaluation(t, store, deps, domain, "decision-high-stakes")
		if demonstrated, progress := dashboardDemonstrated(t, deps); demonstrated != 0 || progress != 0 {
			t.Fatalf("high-stakes domains require human review: demonstrated=%v progress=%v", demonstrated, progress)
		}
	})
	t.Run("standalone attempt without decision", func(t *testing.T) {
		store, deps := setupToolsTest(t)
		domain := makeOwnerDomain(t, store, "L_owner", "math")
		seedEvaluatedAssessmentFixture(t, store, "L_owner", domain.ID, "a", models.ActivityMasteryChallenge, true, time.Now().UTC(), "")
		if demonstrated, _ := dashboardDemonstrated(t, deps); demonstrated != 0 {
			t.Fatalf("a host evaluation without a frozen decision must stay untrusted: demonstrated=%v", demonstrated)
		}
	})
}
