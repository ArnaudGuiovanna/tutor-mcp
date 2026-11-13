// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"

	"tutor-mcp/models"
)

func TestActivityMetacognitiveMirror_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callNextActivity(t, deps, "", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestActivityMetacognitiveMirror_NoPattern(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")
	res := callNextActivity(t, deps, "L_owner", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if mirror, ok := out["metacognitive_mirror"]; !ok || mirror != nil {
		t.Fatalf("expected a null metacognitive_mirror without a consolidated pattern, got %v", out["metacognitive_mirror"])
	}
}

func TestActivityMetacognitiveMirror_UnknownDomainIDRejected(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callNextActivity(t, deps, "L_owner", map[string]any{
		"domain_id": "nonexistent_domain_id",
	})
	if !res.IsError {
		t.Fatalf("expected error for unknown domain_id, got success")
	}
}

func TestActivityMetacognitiveMirror_DomainIDFiltersForeignSignal(t *testing.T) {
	// Regression: the activity's metacognitive mirror must filter the concept-keyed
	// inputs (Interactions, ConceptStates) to the active domain's
	// concept set when domain_id is provided. Seeding a strong
	// no_initiative pattern on foreign concept "x" (D2) and querying
	// with domain_id=D1 (which has only "a") must produce mirror=null
	// because the signal is filtered out. Issue #95.
	store, deps := setupToolsTest(t)

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

	// Seed 6 non-self-initiated interactions on x in D2 — strong
	// no_initiative signal IF visible to the detector.
	for i := 0; i < 6; i++ {
		interaction := &models.Interaction{
			LearnerID: "L_owner", Concept: "x", DomainID: d2.ID,
			ActivityType:  "PRACTICE",
			Success:       true,
			SelfInitiated: false,
			ResponseTime:  4.0,
		}
		if err := store.CreateInteraction(context.Background(), interaction); err != nil {
			t.Fatal(err)
		}
	}

	// Query with domain_id=D1: x's signal must be filtered out → mirror null.
	res := callNextActivity(t, deps, "L_owner", map[string]any{
		"domain_id": d1.ID,
	})
	if res.IsError {
		t.Fatalf("D1 query errored: %s", resultText(res))
	}
	out := decodeResult(t, res)
	if mirror, ok := out["metacognitive_mirror"]; ok && mirror != nil {
		t.Errorf("cross-domain leak: with domain_id=D1, mirror should be nil (x's signal filtered out), got %v", mirror)
	}
}
