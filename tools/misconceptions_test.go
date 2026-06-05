// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"

	"tutor-mcp/models"
)

func TestGetMisconceptions_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetMisconceptions, "", "get_misconceptions", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestGetMisconceptions_NoDataReturnsEmpty(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetMisconceptions, "L_owner", "get_misconceptions", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	mc, ok := out["misconceptions"].([]any)
	if !ok {
		t.Fatalf("expected misconceptions array, got %v", out)
	}
	if len(mc) != 0 {
		t.Fatalf("expected empty list, got %v", mc)
	}
}

func TestGetMisconceptions_DomainNotFound(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetMisconceptions, "L_owner", "get_misconceptions", map[string]any{
		"domain_id": "missing",
	})
	if !res.IsError {
		t.Fatalf("expected error result, got %q", resultText(res))
	}
}

func TestGetMisconceptions_FilterByConcept(t *testing.T) {
	store, deps := setupToolsTest(t)

	// Seed an interaction with a misconception.
	if err := store.CreateInteraction(context.Background(), &models.Interaction{
		LearnerID:           "L_owner",
		Concept:             "loops",
		ActivityType:        "RECALL_EXERCISE",
		Success:             false,
		Confidence:          0.3,
		MisconceptionType:   "off_by_one",
		MisconceptionDetail: "uses < instead of <=",
	}); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerGetMisconceptions, "L_owner", "get_misconceptions", map[string]any{
		"concept": "loops",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	mc, _ := out["misconceptions"].([]any)
	if len(mc) == 0 {
		t.Fatalf("expected at least one misconception, got %v", out)
	}
}

func TestGetMisconceptions_DomainAndConceptMismatch(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math") // contains a, b

	res := callTool(t, deps, registerGetMisconceptions, "L_owner", "get_misconceptions", map[string]any{
		"domain_id": d.ID,
		"concept":   "loops", // not in domain
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	mc, _ := out["misconceptions"].([]any)
	if len(mc) != 0 {
		t.Fatalf("expected empty list (concept not in domain), got %v", mc)
	}
}
