// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestTransferChallenge_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerTransferChallenge, "", "transfer_challenge", map[string]any{"concept": "x"})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestTransferChallenge_MissingConcept(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerTransferChallenge, "L_owner", "transfer_challenge", map[string]any{"concept": ""})
	if !res.IsError || !strings.Contains(resultText(res), "concept is required") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestTransferChallenge_NotFound(t *testing.T) {
	store, deps := setupToolsTest(t)
	// The concept is part of the domain graph but has no concept_state row —
	// the domain/membership guard passes and the GetConceptState lookup is
	// what fails. (issue #8 guard mirrors record_transfer_result.)
	seedDomain(t, store, "L_owner", "ghost")
	res := callTool(t, deps, registerTransferChallenge, "L_owner", "transfer_challenge", map[string]any{"concept": "ghost"})
	if !res.IsError {
		t.Fatalf("expected error for missing concept state")
	}
}

func TestTransferChallenge_RejectsConceptOutsideDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	// A mastered concept_state that is not part of the resolved domain must
	// be rejected (issue #8): without the guard the tool served a transfer
	// challenge for a stale/hallucinated concept name.
	cs := models.NewConceptState("L_owner", "orphan")
	cs.PMastery = 0.95
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)
	seedDomain(t, store, "L_owner", "calc") // domain does NOT contain "orphan"

	res := callTool(t, deps, registerTransferChallenge, "L_owner", "transfer_challenge", map[string]any{"concept": "orphan"})
	if !res.IsError || !strings.Contains(resultText(res), "not part of domain") {
		t.Fatalf("expected concept-not-in-domain error, got %q", resultText(res))
	}
}

func TestTransferChallenge_NotMastered(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "calc")
	cs := models.NewConceptState("L_owner", "calc")
	cs.PMastery = 0.3
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	res := callTool(t, deps, registerTransferChallenge, "L_owner", "transfer_challenge", map[string]any{"concept": "calc"})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["eligible"] != false {
		t.Fatalf("expected eligible=false, got %v", out)
	}
}

func TestTransferChallenge_DefaultContextType(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "calc")
	cs := models.NewConceptState("L_owner", "calc")
	cs.PMastery = 0.95
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	res := callTool(t, deps, registerTransferChallenge, "L_owner", "transfer_challenge", map[string]any{"concept": "calc"})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["concept"] != "calc" {
		t.Fatalf("expected concept=calc, got %v", out["concept"])
	}
	if _, ok := out["concept_id"]; ok {
		t.Fatalf("did not expect legacy concept_id alias in result: %v", out)
	}
	if out["context_type"] != "near" {
		t.Fatalf("expected default context_type=near, got %v", out["context_type"])
	}
	if out["transfer_dimension"] != "near" {
		t.Fatalf("expected transfer_dimension=near, got %v", out["transfer_dimension"])
	}
	if _, ok := out["transfer_profile"].(map[string]any); !ok {
		t.Fatalf("expected transfer_profile, got %v", out["transfer_profile"])
	}
}

func TestTransferChallenge_CustomContextType(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "calc")
	cs := models.NewConceptState("L_owner", "calc")
	cs.PMastery = 0.95
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	res := callTool(t, deps, registerTransferChallenge, "L_owner", "transfer_challenge", map[string]any{
		"concept":      "calc",
		"context_type": "interview",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["context_type"] != "interview" {
		t.Fatalf("expected interview, got %v", out["context_type"])
	}
}
