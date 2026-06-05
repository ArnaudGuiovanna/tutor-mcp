// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
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
	_ = store.InsertConceptStateIfNotExists(cs)
	_ = store.UpsertConceptState(cs)
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
	_ = store.InsertConceptStateIfNotExists(cs)
	_ = store.UpsertConceptState(cs)

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
	_ = store.InsertConceptStateIfNotExists(cs)
	_ = store.UpsertConceptState(cs)

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
	_ = store.InsertConceptStateIfNotExists(cs)
	_ = store.UpsertConceptState(cs)

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

func TestTransferChallenge_AcceptsLegacyConceptID(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "legacy_calc")
	cs := models.NewConceptState("L_owner", "legacy_calc")
	cs.PMastery = 0.95
	_ = store.InsertConceptStateIfNotExists(cs)
	_ = store.UpsertConceptState(cs)

	res := callTool(t, deps, registerTransferChallenge, "L_owner", "transfer_challenge", map[string]any{"concept_id": "legacy_calc"})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["concept"] != "legacy_calc" {
		t.Fatalf("expected canonical concept key, got %v", out)
	}
	if _, ok := out["concept_id"]; ok {
		t.Fatalf("did not expect legacy concept_id alias in result: %v", out)
	}
}

func TestRecordTransferResult_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordTransferResult, "", "record_transfer_result", map[string]any{
		"concept":      "x",
		"context_type": "real_world",
		"score":        0.5,
	})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestRecordTransferResult_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)
	// makeOwnerDomain creates a domain with concepts ["a","b"]; the
	// transfer concept must be in the active domain (issue #96).
	makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
		"concept":      "a",
		"context_type": "real_world",
		"score":        0.7,
		"session_id":   "s1",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["recorded"] != true {
		t.Fatalf("expected recorded=true, got %v", out)
	}
	if out["transfer_score"].(float64) != 0.7 {
		t.Fatalf("expected score=0.7, got %v", out["transfer_score"])
	}
	if out["blocked"] != false {
		t.Fatalf("expected blocked=false for 0.7, got %v", out["blocked"])
	}
	if out["transfer_dimension"] != "far" {
		t.Fatalf("expected real_world to normalize to transfer_dimension=far, got %v", out["transfer_dimension"])
	}
	profile, ok := out["transfer_profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected transfer_profile, got %v", out["transfer_profile"])
	}
	if profile["readiness_label"] == "" {
		t.Fatalf("expected readiness_label in transfer_profile, got %v", profile)
	}

	// DB state — record persisted.
	scores, err := store.GetTransferScores("L_owner", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 1 || scores[0].Score != 0.7 {
		t.Fatalf("expected single transfer record with score 0.7, got %+v", scores)
	}
}

func TestRecordTransferResult_RejectsScoreOutsideUnitInterval(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
		"concept_id":   "a",
		"context_type": "real_world",
		"score":        1.5, // legal interval is [0,1]
	})
	if !res.IsError {
		t.Fatalf("expected error for score=1.5, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "score") {
		t.Fatalf("expected error to mention 'score', got %q", resultText(res))
	}

	// Negative also rejected.
	resNeg := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
		"concept_id":   "a",
		"context_type": "real_world",
		"score":        -0.2,
	})
	if !resNeg.IsError {
		t.Fatalf("expected error for score=-0.2, got %q", resultText(resNeg))
	}

	// Nothing persisted on rejection.
	scores, _ := store.GetTransferScores("L_owner", "a")
	if len(scores) != 0 {
		t.Fatalf("expected no transfer rows, got %d", len(scores))
	}
}

func TestRecordTransferResult_BlockedFlag(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
		"concept_id":   "a",
		"context_type": "real_world",
		"score":        0.3,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["blocked"] != true {
		t.Fatalf("expected blocked=true for low score, got %v", out["blocked"])
	}
}

// Issue #96: record_transfer_result must reject concept_id values that are
// not part of the resolved domain's concept set, mirroring the
// validateConceptInDomain guard in record_interaction. Without this,
// orphan transfer rows pollute TRANSFER_BLOCKED alerts downstream.
func TestRecordTransferResult_RejectsUnknownConcept(t *testing.T) {
	store, deps := setupToolsTest(t)
	// Domain has concepts {"a","b"}.
	makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
		"concept_id":   "ghost",
		"context_type": "real_world",
		"score":        0.2,
	})
	if !res.IsError {
		t.Fatalf("expected error for unknown concept, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "ghost") {
		t.Fatalf("expected error to mention the unknown concept, got %q", resultText(res))
	}

	// No orphan row should have been persisted.
	scores, _ := store.GetTransferScores("L_owner", "ghost")
	if len(scores) != 0 {
		t.Fatalf("orphan transfer row persisted for unknown concept: %+v", scores)
	}
}

// Issue #96: context_type must be one of the documented enum values
// (real_world, interview, teaching, debugging, creative). A free-string
// value such as "banana" must be rejected with a clear enum error.
func TestRecordTransferResult_RejectsUnknownContextType(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
		"concept_id":   "a",
		"context_type": "banana",
		"score":        0.2,
	})
	if !res.IsError {
		t.Fatalf("expected error for unknown context_type, got %q", resultText(res))
	}
	msg := resultText(res)
	if !strings.Contains(msg, "context_type") {
		t.Fatalf("expected error to mention 'context_type', got %q", msg)
	}
	if !strings.Contains(msg, "real_world") || !strings.Contains(msg, "interview") {
		t.Fatalf("expected error to enumerate the allowed values, got %q", msg)
	}
}

// Issue #96: each documented context_type enum value must be
// accepted. Table-driven so a future schema drift shows up as a single
// failing row rather than an opaque enum-mismatch.
func TestRecordTransferResult_AllowsKnownContextTypes(t *testing.T) {
	for _, ct := range []string{"near", "far", "real_world", "interview", "teaching", "debugging", "creative"} {
		ct := ct
		t.Run(ct, func(t *testing.T) {
			store, deps := setupToolsTest(t)
			makeOwnerDomain(t, store, "L_owner", "math")
			res := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
				"concept_id":   "a",
				"context_type": ct,
				"score":        0.6,
			})
			if res.IsError {
				t.Fatalf("context_type=%q rejected unexpectedly: %q", ct, resultText(res))
			}
			out := decodeResult(t, res)
			if out["recorded"] != true {
				t.Fatalf("context_type=%q: expected recorded=true, got %v", ct, out)
			}
			scores, _ := store.GetTransferScores("L_owner", "a")
			if len(scores) != 1 {
				t.Fatalf("context_type=%q: expected 1 transfer row, got %d", ct, len(scores))
			}
		})
	}
}

// Issue #96: an explicit domain_id pointing at someone else's domain must
// be rejected — concept membership runs against the resolved domain, and
// a foreign domain has no overlap with the learner's concept set.
func TestRecordTransferResult_DomainIDRejectsForeignDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")
	foreign, err := store.CreateDomain("L_other", "shared", "", models.KnowledgeSpace{
		Concepts: []string{"a"},
	})
	if err != nil {
		t.Fatalf("create foreign domain: %v", err)
	}

	res := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
		"concept_id":   "a",
		"domain_id":    foreign.ID,
		"context_type": "real_world",
		"score":        0.5,
	})
	if !res.IsError {
		t.Fatalf("expected error on foreign domain_id, got %q", resultText(res))
	}
}

// Issue #96: when no domain has been initialised the tool must emit the
// canonical needs_domain_setup payload (mirrors record_interaction).
func TestRecordTransferResult_NoActiveDomain(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordTransferResult, "L_owner", "record_transfer_result", map[string]any{
		"concept_id":   "a",
		"context_type": "real_world",
		"score":        0.5,
	})
	out := decodeResult(t, res)
	if got, _ := out["needs_domain_setup"].(bool); !got {
		t.Fatalf("expected needs_domain_setup=true, got %v (raw %q)", out, resultText(res))
	}
}
