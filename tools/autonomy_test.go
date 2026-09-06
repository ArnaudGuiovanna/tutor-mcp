// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"

	"tutor-mcp/models"
)

func TestGetAutonomyMetrics_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetAutonomyMetrics, "", "get_autonomy_metrics", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestGetAutonomyMetrics_HappyPath(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetAutonomyMetrics, "L_owner", "get_autonomy_metrics", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if _, ok := out["score"]; !ok {
		t.Fatalf("expected score in result, got %v", out)
	}
	if _, ok := out["trend"]; !ok {
		t.Fatalf("expected trend in result, got %v", out)
	}
	if out["score_status"] != "unavailable" || out["score"] != float64(0) || out["calibration_accuracy"] != float64(0) || out["hint_independence"] != float64(0) {
		t.Fatalf("empty history must not appear perfectly calibrated or independent: %v", out)
	}
}

func TestGetAutonomyMetrics_UnknownDomainIDRejected(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetAutonomyMetrics, "L_owner", "get_autonomy_metrics", map[string]any{
		"domain_id": "nonexistent_domain_id",
	})
	if !res.IsError {
		t.Fatalf("expected error for unknown domain_id, got success")
	}
}

func TestGetAutonomyMetrics_DomainIDFiltersForeignInteractions(t *testing.T) {
	// Regression: get_autonomy_metrics must filter the concept-keyed
	// inputs (Interactions, ConceptStates) to the active domain's
	// concept set when domain_id is provided. Otherwise a learner with
	// proactive-review activity in domain D2 sees that signal show up
	// in D1's autonomy score — a cross-domain leak. Issue #95.
	store, deps := setupToolsTest(t)

	// Seed two domains. Only D1 will be queried; D2's interactions
	// must be filtered out.
	d1, err := store.CreateDomain(context.Background(), "L_owner", "active", "",
		models.KnowledgeSpace{Concepts: []string{"a"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := store.CreateDomain(context.Background(), "L_owner", "foreign", "",
		models.KnowledgeSpace{Concepts: []string{"x"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}

	// 5 proactive-review interactions on x (in D2 only).
	for i := 0; i < 5; i++ {
		interaction := &models.Interaction{
			LearnerID: "L_owner", Concept: "x", DomainID: d2.ID,
			ActivityType:      "RECALL_EXERCISE",
			Success:           true,
			IsProactiveReview: true,
			SelfInitiated:     true,
			ResponseTime:      4.0,
		}
		if err := store.CreateInteraction(context.Background(), interaction); err != nil {
			t.Fatal(err)
		}
	}

	// Query 1: domain_id=D1 (foreign data must be filtered out).
	res1 := callTool(t, deps, registerGetAutonomyMetrics, "L_owner", "get_autonomy_metrics", map[string]any{
		"domain_id": d1.ID,
	})
	if res1.IsError {
		t.Fatalf("D1 query errored: %s", resultText(res1))
	}
	out1 := decodeResult(t, res1)
	pr1, _ := out1["proactive_review_rate"].(float64)
	if pr1 != 0.0 {
		t.Errorf("cross-domain leak: with domain_id=D1, proactive_review_rate should be 0 (no x in D1), got %v", pr1)
	}

	// Query 2: domain_id=D2 (in-scope, signal must surface).
	res2 := callTool(t, deps, registerGetAutonomyMetrics, "L_owner", "get_autonomy_metrics", map[string]any{
		"domain_id": d2.ID,
	})
	if res2.IsError {
		t.Fatalf("D2 query errored: %s", resultText(res2))
	}
	out2 := decodeResult(t, res2)
	pr2, _ := out2["proactive_review_rate"].(float64)
	if pr2 == 0.0 {
		t.Errorf("with domain_id=D2 (where x lives), proactive_review_rate should be > 0, got %v", pr2)
	}
}

func TestGetAutonomyMetrics_OppositeCalibrationErrorsDoNotCancel(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "calibration")
	for _, prediction := range []struct {
		id                string
		predicted, actual float64
	}{
		{"overconfident", 1, 0},
		{"underconfident", 0, 1},
	} {
		record := &models.CalibrationRecord{PredictionID: prediction.id, LearnerID: "L_owner", DomainID: domain.ID, ConceptID: "a", Predicted: prediction.predicted}
		if err := store.CreateCalibrationPrediction(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteCalibrationRecord(context.Background(), prediction.id, "L_owner", prediction.actual, prediction.predicted-prediction.actual); err != nil {
			t.Fatal(err)
		}
	}
	for _, params := range []map[string]any{{}, {"domain_id": domain.ID}} {
		res := callTool(t, deps, registerGetAutonomyMetrics, "L_owner", "get_autonomy_metrics", params)
		if res.IsError {
			t.Fatalf("autonomy query failed: %s", resultText(res))
		}
		out := decodeResult(t, res)
		if out["calibration_accuracy"] != float64(0) || out["calibration_samples"] != float64(2) {
			t.Fatalf("opposite prediction errors were cancelled or evidence lost: %v", out)
		}
	}
}
