// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"
	"time"

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
