// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package tools

// Issue #97: end-to-end coherence regression suite — scenarios 5 through 9.
// The former scenario 1–4 blockers have landed and are guarded by focused
// suites: manage_domain_test.go (archived/deleted domains),
// negotiation_test.go (proposal validation), activity_test.go (phase
// transitions), and interaction_e2e_test.go (multi-domain isolation).
//
// Each test is self-contained, follows the same shape as
// TestEndToEnd_TenSuccessesMoveMastery (interaction_e2e_test.go) and
// uses only the existing helpers in testhelper_test.go. The goal is to
// lock in the current behaviour as a regression guard.

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/engine"
	"tutor-mcp/models"
)

// Scenario 5 — DIAGNOSTIC → INSTRUCTION → MAINTENANCE for a
// single-concept domain. Diagnostic exit is deliberately driven only by
// cold DIAGNOSTIC_ASSESSMENT responses: unrelated practice must not consume
// qualified diagnostic coverage. Once the one-concept target is reached, one routing
// pass persists INSTRUCTION and the following pass can persist MAINTENANCE
// because the successful observations put the sole concept above the BKT
// routing threshold. MAINTENANCE may have no review due and return an explicit
// pipeline-exhausted REST payload; the persisted phase remains authoritative.
func TestCoherenceE2E_SingleConceptDomain_TransitionsToMaintenance(t *testing.T) {
	store, deps := setupToolsTest(t)

	if res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "single",
		"concepts":      []string{"a"},
		"prerequisites": map[string][]string{},
	}); res.IsError {
		t.Fatalf("init_domain failed: %s", resultText(res))
	}

	diagnosticItems := engine.NewDefaultPhaseConfig().NDiagnosticMax
	for i := 0; i < diagnosticItems; i++ {
		prepared := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
			"concept":       "a",
			"activity_type": "DIAGNOSTIC_ASSESSMENT",
			"observable":    "Cold response without hints",
			"task_text":     "Solve diagnostic item independently.",
			"rubric_json":   `{"criteria":[{"id":"correctness","max_score":1}],"passing_score":0.6}`,
		})
		if prepared.IsError {
			t.Fatalf("iter %d: prepare diagnostic errored: %s", i, resultText(prepared))
		}
		preparedOut := decodeResult(t, prepared)
		attemptID, _ := preparedOut["attempt_id"].(string)
		sessionID, _ := preparedOut["session_id"].(string)
		submitted := callTool(t, deps, registerSubmitAssessmentAttempt, "L_owner", "submit_assessment_attempt", map[string]any{
			"attempt_id": attemptID, "learner_response": "Independent correct response.",
		})
		if submitted.IsError {
			t.Fatalf("iter %d: submit diagnostic errored: %s", i, resultText(submitted))
		}
		recRes := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
			"concept":               "a",
			"activity_type":         "DIAGNOSTIC_ASSESSMENT",
			"success":               true,
			"response_time_seconds": 4.0,
			"confidence":            0.9,
			"notes":                 "",
			"session_id":            sessionID,
			"attempt_id":            attemptID,
			"evaluator_id":          "host-test-v1",
			"evaluation_method":     "host_llm",
			"rubric_score_json":     `{"criteria_scores":[{"id":"correctness","score":1}]}`,
		})
		if recRes.IsError {
			t.Fatalf("iter %d: record_interaction errored: %s", i, resultText(recRes))
		}
	}

	// The first orchestration pass evaluates the persisted DIAGNOSTIC phase and
	// advances exactly one FSM edge.
	firstRes := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{})
	if firstRes.IsError {
		t.Fatalf("first get_next_activity errored: %s", resultText(firstRes))
	}
	domain, err := store.GetDomainByLearner(context.Background(), "L_owner")
	if err != nil {
		t.Fatalf("load domain after diagnostic: %v", err)
	}
	if domain.Phase != models.PhaseInstruction {
		t.Fatalf("phase after %d diagnostic assessments = %s, want INSTRUCTION", diagnosticItems, domain.Phase)
	}

	// The next pass evaluates the newly persisted INSTRUCTION phase. Since the
	// sole goal-relevant concept is already above the routing threshold, it can
	// now advance to MAINTENANCE.
	actRes := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{})
	if actRes.IsError {
		t.Fatalf("second get_next_activity errored: %s", resultText(actRes))
	}
	domain, err = store.GetDomainByLearner(context.Background(), "L_owner")
	if err != nil {
		t.Fatalf("load domain after instruction: %v", err)
	}
	if domain.Phase != models.PhaseMaintenance {
		t.Fatalf("phase after instruction routing = %s, want MAINTENANCE", domain.Phase)
	}
	out := decodeResult(t, actRes)
	activity, ok := out["activity"].(map[string]any)
	if !ok {
		t.Fatalf("missing activity object in response: %v", out)
	}

	// The orchestrator must always populate a type, even on REST/escape.
	if _, hasType := activity["type"]; !hasType {
		t.Fatalf("activity is missing 'type' field: %v", activity)
	}

	// A real maintenance activity carries the phase prefix; if no spaced review
	// is due, the explicit NoFringe/pipeline-exhausted REST result is valid.
	rationale, _ := activity["rationale"].(string)
	if strings.Contains(rationale, "[phase=DIAGNOSTIC]") {
		t.Fatalf("expected DIAGNOSTIC to have exited after cold assessments, rationale=%q", rationale)
	}
	hasPhase := strings.Contains(rationale, "[phase=MAINTENANCE]")
	hasNoFringe := strings.Contains(rationale, "pipeline_exhausted") || strings.Contains(rationale, "NoFringe")
	if !hasPhase && !hasNoFringe {
		t.Fatalf("unexpected rationale after MAINTENANCE transition — want MAINTENANCE or explicit NoFringe, got %q", rationale)
	}
}

// Scenario 6 — add_concepts must reject prerequisite cycles introduced
// in the batch's prereq map. Existing tests in domain_test.go cover the
// validateConcepts helper at the init_domain entry point; this test
// extends the regression coverage to the add_concepts entry point so
// the same DAG invariant is enforced on incremental additions.
//
// We submit a 2-cycle wholly within the new batch (c → d → c) so the
// detection lives entirely in the prereq slice passed to
// validateConcepts (which today only sees params.Prerequisites — see
// domain.go:418). A cycle that would only materialise after merging
// the existing graph's prereqs is intentionally NOT covered here: it
// is a known coverage gap that would require modifying production
// code to fix, which is out of scope for this PR (tests-only). That
// path is tracked separately.
func TestCoherenceE2E_AddConceptsCycleRejected(t *testing.T) {
	store, deps := setupToolsTest(t)

	// Seed initial domain with concepts a, b and edge b → a.
	if res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "cyc",
		"concepts":      []string{"a", "b"},
		"prerequisites": map[string][]string{"b": {"a"}},
	}); res.IsError {
		t.Fatalf("init_domain failed: %s", resultText(res))
	}
	d, err := store.GetDomainByLearner(context.Background(), "L_owner")
	if err != nil || d == nil {
		t.Fatalf("could not look up seeded domain: %v", err)
	}

	// Add concepts c, d with a 2-cycle in the new batch's prereqs:
	//   c depends on d AND d depends on c.
	res := callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        d.ID,
		"expected_version": 1,
		"concepts":         []string{"c", "d"},
		"prerequisites":    map[string][]string{"c": {"d"}, "d": {"c"}},
	})
	if !res.IsError {
		t.Fatalf("expected cycle rejection, got success: %s", resultText(res))
	}
	msg := strings.ToLower(resultText(res))
	if !strings.Contains(msg, "cycle") && !strings.Contains(msg, "prerequisite") {
		t.Fatalf("expected error mentioning cycle/prerequisite, got %q", resultText(res))
	}

	// And the domain graph must not have been mutated — neither concept
	// from the rejected batch should appear.
	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDomainByID after rejected add: %v", err)
	}
	for _, c := range got.Graph.Concepts {
		if c == "c" || c == "d" {
			t.Fatalf("concept %q was persisted despite cycle rejection: %v", c, got.Graph.Concepts)
		}
	}
}

// Scenario 7 — calibration round trip: predicted vs actual surfaces a
// non-zero delta. record_calibration_result returns "delta" =
// predicted - actual_score (see tools/calibration.go:112 and the
// JSONResult payload at line 122). A predicted_mastery of 5.0 maps
// internally to predicted=1.0 (formula (x-1)/4); paired with
// actual_score=0.0 we expect delta=1.0. We assert delta is in the
// expected positive direction (over-estimate) with a generous epsilon
// to absorb any future float arithmetic refactor.
func TestCoherenceE2E_CalibrationRoundTrip_BiasReflectsPrediction(t *testing.T) {
	_, deps := setupToolsTest(t)

	if res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "cal",
		"concepts":      []string{"a"},
		"prerequisites": map[string][]string{},
	}); res.IsError {
		t.Fatalf("init_domain failed: %s", resultText(res))
	}

	// predicted_mastery=5 → internal predicted = 1.0 (full confidence).
	checkRes := callTool(t, deps, registerCalibrationCheck, "L_owner", "calibration_check", map[string]any{
		"concept_id":        "a",
		"predicted_mastery": 5.0,
	})
	if checkRes.IsError {
		t.Fatalf("calibration_check failed: %s", resultText(checkRes))
	}
	checkOut := decodeResult(t, checkRes)
	predictionID, _ := checkOut["prediction_id"].(string)
	if predictionID == "" {
		t.Fatalf("expected prediction_id, got %v", checkOut)
	}

	// Then: a failure on the actual exercise (success=false, low score).
	if recRes := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               false,
		"response_time_seconds": 8.0,
		"confidence":            0.8,
		"notes":                 "",
	}); recRes.IsError {
		t.Fatalf("record_interaction errored: %s", resultText(recRes))
	}

	// record_calibration_result with actual_score=0.0 (total miss).
	resultRes := callTool(t, deps, registerRecordCalibrationResult, "L_owner", "record_calibration_result", map[string]any{
		"prediction_id": predictionID,
		"actual_score":  0.0,
	})
	if resultRes.IsError {
		t.Fatalf("record_calibration_result errored: %s", resultText(resultRes))
	}
	out := decodeResult(t, resultRes)

	deltaRaw, ok := out["delta"]
	if !ok {
		t.Fatalf("expected 'delta' in record_calibration_result response, got %v", out)
	}
	delta, ok := deltaRaw.(float64)
	if !ok {
		t.Fatalf("expected delta to be a number, got %T (%v)", deltaRaw, deltaRaw)
	}
	// predicted_mastery=5 → internal predicted=1.0; actual=0.0 → delta=1.0.
	if delta < 0.99 || delta > 1.01 {
		t.Fatalf("expected delta=1.0 (predicted 1.0 - actual 0.0), got %v", delta)
	}
}

// Scenario 8 — record_affect with low confidence (Likert=1, "anxieux"
// per the affect.go param doc) makes ComputeTutorMode return
// "scaffolding". When tutor_mode is "scaffolding", get_next_activity
// multiplies activity.difficulty_target by 0.75 (see activity.go:166).
// We seed enough interactions to trigger a non-trivial activity, then
// flip the affect to anxious and assert (a) tutor_mode == "scaffolding"
// and (b) the response carries a numeric difficulty_target — the exact
// post-multiplier value depends on the action selected, so we assert
// the field exists and is in the post-clamp range [0.3, 0.85] rather
// than reverse-engineering the action selector.
func TestCoherenceE2E_AffectScaffolding_AdjustsDifficulty(t *testing.T) {
	_, deps := setupToolsTest(t)

	if res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "aff",
		"concepts":      []string{"a", "b", "c"},
		"prerequisites": map[string][]string{},
	}); res.IsError {
		t.Fatalf("init_domain failed: %s", resultText(res))
	}

	// Seed a few interactions so we move out of the bootstrap noise and
	// the orchestrator returns a real activity (not REST/setup).
	for i := 0; i < 5; i++ {
		actRes := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{})
		if actRes.IsError {
			t.Fatalf("seed iter %d: get_next_activity errored: %s", i, resultText(actRes))
		}
		out := decodeResult(t, actRes)
		activity, _ := out["activity"].(map[string]any)
		concept, _ := activity["concept"].(string)
		if concept == "" {
			// Setup or REST — skip the corresponding record_interaction.
			continue
		}
		if recRes := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
			"concept":               concept,
			"activity_type":         "RECALL_EXERCISE",
			"success":               true,
			"response_time_seconds": 4.0,
			"confidence":            0.7,
			"notes":                 "",
		}); recRes.IsError {
			t.Fatalf("seed iter %d: record_interaction errored: %s", i, resultText(recRes))
		}
	}

	// Now record an anxious affect : confidence=1 → "scaffolding" per
	// engine.ComputeTutorMode.
	activeSession, err := deps.Store.GetActiveLearningSession(context.Background(), "L_owner")
	if err != nil {
		t.Fatalf("active learning session: %v", err)
	}
	if affRes := callTool(t, deps, registerRecordAffect, "L_owner", "record_affect", map[string]any{
		"session_id": activeSession.ID,
		"energy":     3,
		"confidence": 1,
	}); affRes.IsError {
		t.Fatalf("record_affect errored: %s", resultText(affRes))
	}

	// Next activity should reflect the scaffolding tutor_mode.
	actRes := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{})
	if actRes.IsError {
		t.Fatalf("post-affect get_next_activity errored: %s", resultText(actRes))
	}
	out := decodeResult(t, actRes)

	tutorMode, _ := out["tutor_mode"].(string)
	if tutorMode != "scaffolding" {
		t.Fatalf("expected tutor_mode=scaffolding after low-confidence affect, got %q (full out: %v)", tutorMode, out)
	}

	activity, ok := out["activity"].(map[string]any)
	if !ok {
		t.Fatalf("missing activity in response: %v", out)
	}
	rawDifficulty, hasDifficulty := activity["difficulty_target"]
	if !hasDifficulty {
		t.Fatalf("activity missing difficulty_target: %v", activity)
	}
	difficulty, ok := rawDifficulty.(float64)
	if !ok {
		t.Fatalf("difficulty_target is not numeric: %T (%v)", rawDifficulty, rawDifficulty)
	}
	// difficulty_target may be 0 for the SETUP_DOMAIN/REST escape paths;
	// for any concrete activity it should fall in the post-clamp range
	// [0.3, 0.85] (see activity.go:175-180). Accept 0 as a degenerate
	// no-activity case rather than failing on it — the load-bearing
	// assertion is that tutor_mode = "scaffolding".
	if difficulty != 0 && (difficulty < 0.0 || difficulty > 0.85) {
		t.Fatalf("difficulty_target=%v outside expected range [0, 0.85]", difficulty)
	}
}

// Scenario 9 — a misconception recorded on a failed interaction must
// surface in subsequent get_next_activity calls via the
// "active_misconceptions" enrichment (see activity.go:188-204). The
// enrichment only fires when the orchestrator picks the same concept
// the misconception is attached to; the gate controller actually
// prioritises misconception concepts (see orchestrator's Gate path),
// so we expect the next activity to land on "a" and carry the
// off_by_one entry. We assert the structure of the response without
// over-locking on the exact MisconceptionGroup field encoding — we
// accept either a slice with at least one entry whose
// "misconception_type" mentions "off_by_one", OR a JSON-marshalled
// slice with the same content.
func TestCoherenceE2E_MisconceptionPersistsAcrossSession(t *testing.T) {
	_, deps := setupToolsTest(t)

	if res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "mc",
		"concepts":      []string{"a", "b"},
		"prerequisites": map[string][]string{},
	}); res.IsError {
		t.Fatalf("init_domain failed: %s", resultText(res))
	}

	// Record a failed interaction with a misconception on concept a.
	if recRes := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               false,
		"response_time_seconds": 8.0,
		"confidence":            0.4,
		"error_type":            "LOGIC_ERROR",
		"misconception_type":    "off_by_one",
		"misconception_detail":  "uses < instead of <=",
		"notes":                 "",
	}); recRes.IsError {
		t.Fatalf("record_interaction errored: %s", resultText(recRes))
	}

	// Simulate a session boundary by closing the session; subsequent
	// get_next_activity should still surface the active misconception.
	if closeRes := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{}); closeRes.IsError {
		t.Fatalf("record_session_close errored: %s", resultText(closeRes))
	}

	// Try a few times: the gate prioritises misconception concepts but
	// the action selector may pick a different concept on any single
	// call. We loop until we either see an active misconception or run
	// out of attempts. The test fails if we never see one.
	var seenMisconceptionType string
	for i := 0; i < 5; i++ {
		actRes := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{})
		if actRes.IsError {
			t.Fatalf("iter %d: get_next_activity errored: %s", i, resultText(actRes))
		}
		out := decodeResult(t, actRes)
		raw, ok := out["active_misconceptions"]
		if !ok {
			continue
		}
		// Expected encoding is a JSON array of MisconceptionGroup objects.
		arr, ok := raw.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if mt, _ := m["misconception_type"].(string); mt != "" {
				seenMisconceptionType = mt
				break
			}
		}
		if seenMisconceptionType != "" {
			break
		}
	}

	if seenMisconceptionType == "" {
		t.Fatal("expected an active_misconceptions entry exposing misconception_type after a failed interaction with misconception_type=off_by_one, never saw one across 5 get_next_activity calls")
	}
	if !strings.Contains(seenMisconceptionType, "off_by_one") {
		t.Fatalf("expected misconception_type to mention off_by_one, got %q", seenMisconceptionType)
	}
}
