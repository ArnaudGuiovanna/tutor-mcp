// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCheckMastery_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerCheckMastery, "", "check_mastery", map[string]any{"concept": "x"})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestCheckMastery_MissingConcept(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{"concept": ""})
	if !res.IsError || !strings.Contains(resultText(res), "concept is required") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestCheckMastery_NotFound(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "ghost")
	res := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{"concept": "ghost"})
	if !res.IsError || !strings.Contains(resultText(res), "concept state not found") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestCheckMastery_NotReady(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := seedDomain(t, store, "L_owner", "calc")
	cs := models.NewConceptStateInDomain("L_owner", domain.ID, "calc")
	cs.PMastery = 0.4
	if err := store.InsertConceptStateIfNotExists(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{"concept": "calc"})
	if res.IsError {
		t.Fatalf("did not expect error for low mastery, got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["mastery_ready"] != false {
		t.Fatalf("expected mastery_ready=false, got %v", out["mastery_ready"])
	}
	if out["mastery"] != 0.4 {
		t.Fatalf("expected mastery=0.4, got %v", out["mastery"])
	}
	if out["mastery_estimate"] != 0.4 {
		t.Fatalf("expected mastery_estimate=0.4, got %v", out["mastery_estimate"])
	}
	if _, ok := out["current_mastery"]; ok {
		t.Fatalf("did not expect legacy current_mastery alias in result: %v", out)
	}
}

func TestCheckMastery_Ready(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := seedDomain(t, store, "L_owner", "calc")
	cs := models.NewConceptStateInDomain("L_owner", domain.ID, "calc")
	cs.PMastery = 0.95
	cs.CardState = "review"
	cs.Stability = 30
	cs.Reps = 5
	lastReview := time.Now().UTC().Add(-time.Hour)
	cs.LastReview = &lastReview
	if err := store.InsertConceptStateIfNotExists(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	// InsertConceptStateIfNotExists does not update if exists. Use Upsert to set mastery.
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	retentionAttemptID := seedEvaluatedAssessmentFixture(
		t, store, "L_owner", domain.ID, "calc",
		models.ActivityMasteryChallenge, true, now, "",
	)
	for _, activityType := range []models.ActivityType{
		models.ActivityRecall,
		models.ActivityPractice,
		models.ActivityMasteryChallenge,
	} {
		attemptID := ""
		if activityType == models.ActivityMasteryChallenge {
			attemptID = retentionAttemptID
		}
		if err := store.CreateInteraction(context.Background(), &models.Interaction{
			LearnerID: "L_owner", AssessmentAttemptID: attemptID,
			Concept: "calc", ActivityType: string(activityType), Success: true,
			CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Retention is an evidence claim, not a synonym for a high FSRS estimate:
	// make the first observation old enough that the successful mastery
	// retrieval happened after a real delay.
	if _, err := store.RawDB().Exec(`UPDATE interactions SET created_at = ?
		WHERE learner_id = ? AND concept = ? AND activity_type = ?`,
		time.Now().UTC().Add(-48*time.Hour), "L_owner", "calc", string(models.ActivityRecall)); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{"concept": "calc"})
	if res.IsError {
		t.Fatalf("expected success: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["mastery_ready"] != true {
		t.Fatalf("expected mastery_ready=true, got %v", out["mastery_ready"])
	}
	if out["mastery"] != 0.95 {
		t.Fatalf("expected mastery=0.95, got %v", out["mastery"])
	}
	if out["mastery_estimate"] != 0.95 {
		t.Fatalf("expected mastery_estimate=0.95, got %v", out["mastery_estimate"])
	}
	if _, ok := out["current_mastery"]; ok {
		t.Fatalf("did not expect legacy current_mastery alias in result: %v", out)
	}
	challenge, ok := out["challenge"].(map[string]any)
	if !ok {
		t.Fatalf("expected a domain-neutral challenge, got %v", out["challenge"])
	}
	prompt, _ := challenge["prompt_for_llm"].(string)
	if strings.Contains(strings.ToLower(prompt), "code") || strings.Contains(strings.ToLower(prompt), "software") {
		t.Fatalf("mastery prompt must be domain-neutral, got %q", prompt)
	}
}

func TestMasteryReadyAlertAndCheckMasteryShareTrustedTransferFailure(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := seedDomain(t, store, "L_owner", "calc")
	now := time.Now().UTC()
	lastReview := now.Add(-time.Hour)
	cs := models.NewConceptStateInDomain("L_owner", domain.ID, "calc")
	cs.PMastery = 0.95
	cs.CardState = "review"
	cs.Stability = 30
	cs.Reps = 8
	cs.LastReview = &lastReview
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	retentionAttempt := seedEvaluatedAssessmentFixture(t, store, "L_owner", domain.ID, "calc", models.ActivityRecall, true, now.Add(-time.Hour), "")
	for _, interaction := range []*models.Interaction{
		{LearnerID: "L_owner", DomainID: domain.ID, Concept: "calc", ActivityType: string(models.ActivityPractice), Success: true, CreatedAt: now.Add(-72 * time.Hour)},
		{LearnerID: "L_owner", DomainID: domain.ID, Concept: "calc", ActivityType: string(models.ActivityFeynmanPrompt), Success: true, CreatedAt: now.Add(-2 * time.Hour)},
		{LearnerID: "L_owner", DomainID: domain.ID, AssessmentAttemptID: retentionAttempt, Concept: "calc", ActivityType: string(models.ActivityRecall), Success: true, CreatedAt: now.Add(-time.Hour)},
	} {
		if err := store.CreateInteraction(context.Background(), interaction); err != nil {
			t.Fatal(err)
		}
	}

	for index, dimension := range []string{"near", "far", "debugging"} {
		at := now.Add(-time.Duration(4-index) * time.Hour)
		attemptID := seedEvaluatedAssessmentFixture(t, store, "L_owner", domain.ID, "calc", models.ActivityTransferProbe, true, at, models.EvaluationMethodExternal)
		if err := store.CreateTransferRecord(context.Background(), &models.TransferRecord{
			LearnerID: "L_owner", DomainID: domain.ID, ConceptID: "calc",
			AssessmentAttemptID: attemptID, ContextType: dimension, Score: 0.9,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RawDB().Exec(`UPDATE transfer_records SET created_at = ? WHERE assessment_attempt_id = ?`, at, attemptID); err != nil {
			t.Fatal(err)
		}
	}

	before := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{"concept": "calc", "domain_id": domain.ID})
	if before.IsError || decodeResult(t, before)["mastery_ready"] != true {
		t.Fatalf("trusted passing transfer baseline should be ready: %s", resultText(before))
	}
	alertsBefore := callTool(t, deps, registerGetPendingAlerts, "L_owner", "get_pending_alerts", map[string]any{"domain_id": domain.ID})
	if alertsBefore.IsError || !resultHasAlertType(t, alertsBefore, string(models.AlertMasteryReady), "calc") {
		t.Fatalf("MASTERY_READY missing before trusted failure: %s", resultText(alertsBefore))
	}

	failureAt := now.Add(-30 * time.Minute)
	failureAttempt := seedEvaluatedAssessmentFixture(t, store, "L_owner", domain.ID, "calc", models.ActivityTransferProbe, false, failureAt, models.EvaluationMethodExternal)
	if err := store.CreateTransferRecord(context.Background(), &models.TransferRecord{
		LearnerID: "L_owner", DomainID: domain.ID, ConceptID: "calc",
		AssessmentAttemptID: failureAttempt, ContextType: "near", Score: 0.95,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RawDB().Exec(`UPDATE transfer_records SET created_at = ? WHERE assessment_attempt_id = ?`, failureAt, failureAttempt); err != nil {
		t.Fatal(err)
	}

	after := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{"concept": "calc", "domain_id": domain.ID})
	afterOut := decodeResult(t, after)
	if after.IsError || afterOut["mastery_ready"] != false {
		t.Fatalf("recent trusted failure did not block check_mastery: %s", resultText(after))
	}
	profile, _ := afterOut["transfer_profile"].(map[string]any)
	if profile["readiness_label"] != "blocked" {
		t.Fatalf("trusted failure profile=%v, want blocked", profile)
	}
	alertsAfter := callTool(t, deps, registerGetPendingAlerts, "L_owner", "get_pending_alerts", map[string]any{"domain_id": domain.ID})
	if alertsAfter.IsError {
		t.Fatalf("get_pending_alerts after failure: %s", resultText(alertsAfter))
	}
	if resultHasAlertType(t, alertsAfter, string(models.AlertMasteryReady), "calc") {
		t.Fatalf("MASTERY_READY disagreed with blocked check_mastery: %s", resultText(alertsAfter))
	}
}

func resultHasAlertType(t *testing.T, result *mcp.CallToolResult, alertType, concept string) bool {
	t.Helper()
	out := decodeResult(t, result)
	alerts, _ := out["alerts"].([]any)
	for _, raw := range alerts {
		alert, _ := raw.(map[string]any)
		if alert["type"] == alertType && alert["concept"] == concept {
			return true
		}
	}
	return false
}

func TestCheckMastery_AcceptsLegacyConceptID(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "legacy_calc")
	cs := models.NewConceptState("L_owner", "legacy_calc")
	cs.PMastery = 0.4
	if err := store.InsertConceptStateIfNotExists(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{"concept_id": "legacy_calc"})
	if res.IsError {
		t.Fatalf("did not expect error for legacy concept_id, got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["mastery"] != 0.4 {
		t.Fatalf("expected mastery=0.4, got %v", out["mastery"])
	}
	if _, ok := out["current_mastery"]; ok {
		t.Fatalf("did not expect legacy current_mastery alias in result: %v", out)
	}
}

func TestCheckMastery_HighBKTWeakEvidenceNotReady(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "calc")
	cs := models.NewConceptState("L_owner", "calc")
	cs.PMastery = 0.95
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{"concept": "calc"})
	if res.IsError {
		t.Fatalf("expected success: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["mastery_ready"] != false {
		t.Fatalf("expected mastery_ready=false with weak evidence, got %v", out["mastery_ready"])
	}
	if out["bkt_mastery_ready"] != true {
		t.Fatalf("expected bkt_mastery_ready=true, got %v", out["bkt_mastery_ready"])
	}
	if _, ok := out["evidence_quality"].(map[string]any); !ok {
		t.Fatalf("expected evidence_quality in result, got %v", out)
	}
}
