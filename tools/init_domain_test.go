// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
)

func TestInitDomain_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "", "init_domain", map[string]any{
		"name":          "math",
		"concepts":      []string{"a"},
		"prerequisites": map[string][]string{},
	})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestInitDomain_MissingName(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "",
		"concepts":      []string{"a"},
		"prerequisites": map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "name is required") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestInitDomain_NameTooLong(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          strings.Repeat("x", maxDomainNameLen+1),
		"concepts":      []string{"a"},
		"prerequisites": map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "name too long") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestInitDomain_NoConcepts(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "math",
		"concepts":      []string{},
		"prerequisites": map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "at least one concept") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestInitDomain_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)

	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "math",
		"concepts":      []string{"derivative", "integral"},
		"prerequisites": map[string][]string{"integral": {"derivative"}},
		"personal_goal": "do real analysis",
		"value_framings": map[string]any{
			"financial":    "make more money",
			"intellectual": "think clearly",
		},
	})
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["domain_id"] == nil {
		t.Fatalf("expected domain_id in response, got %v", out)
	}

	// Domain saved
	d, err := store.GetDomainByLearner(context.Background(), "L_owner")
	if err != nil {
		t.Fatalf("domain not saved: %v", err)
	}
	if d.Name != "math" {
		t.Fatalf("name not saved, got %q", d.Name)
	}
	if d.PersonalGoal != "do real analysis" {
		t.Fatalf("goal not saved, got %q", d.PersonalGoal)
	}
	if !strings.Contains(d.ValueFramingsJSON, "make more money") {
		t.Fatalf("value framings not persisted: %q", d.ValueFramingsJSON)
	}

	// ConceptStates initialised
	for _, c := range []string{"derivative", "integral"} {
		cs, err := store.GetConceptState(context.Background(), "L_owner", c)
		if err != nil || cs == nil {
			t.Fatalf("concept state for %q missing: %v", c, err)
		}
	}
}

func TestInitDomain_PersonalGoalTooLong(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "math",
		"concepts":      []string{"a"},
		"prerequisites": map[string][]string{},
		"personal_goal": strings.Repeat("x", maxPersonalGoalLen+1),
	})
	if !res.IsError || !strings.Contains(resultText(res), "personal_goal too long") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestInitDomain_InvalidConcepts(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "math",
		"concepts":      []string{"a", ""},
		"prerequisites": map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "empty concept name") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestAddConcepts_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        d.ID,
		"expected_version": 1,
		"concepts":         []string{"c", "d"},
		"prerequisites":    map[string][]string{"c": {"a"}, "d": {"c"}},
	})
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["added"].(float64) != 2 {
		t.Fatalf("expected added=2, got %v", out["added"])
	}

	got, _ := store.GetDomainByID(context.Background(), d.ID)
	expected := []string{"a", "b", "c", "d"}
	if len(got.Graph.Concepts) != len(expected) {
		t.Fatalf("expected %d concepts, got %d (%v)", len(expected), len(got.Graph.Concepts), got.Graph.Concepts)
	}
	if got.Graph.Prerequisites["c"][0] != "a" || got.Graph.Prerequisites["d"][0] != "c" {
		t.Fatalf("prerequisites not merged: %+v", got.Graph.Prerequisites)
	}
}

func TestAddConcepts_DuplicateRejected(t *testing.T) {
	// Issue #27: duplicates of existing concepts are now hard-rejected
	// (previously silently no-op'd). This guards against inflated
	// TotalGoalRelevant counts in the FSM observables.
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        d.ID,
		"expected_version": 1,
		"concepts":         []string{"a"},
		"prerequisites":    map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "duplicate concept name") {
		t.Fatalf("expected duplicate error, got %q", resultText(res))
	}
}

func TestAddConcepts_NoConcepts(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        "x",
		"expected_version": 1,
		"concepts":         []string{},
		"prerequisites":    map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "at least one concept") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestAddConcepts_DomainNotFound(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        "missing",
		"expected_version": 1,
		"concepts":         []string{"x"},
		"prerequisites":    map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "domain not found") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestInitDomain_RejectsDuplicateConcepts(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name": "x", "concepts": []string{"a", "b", "a"},
		"prerequisites": map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "duplicate concept name") {
		t.Fatalf("expected duplicate error, got %q", resultText(res))
	}
}

func TestAddConcepts_RejectsDuplicateInBatch(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math") // contains a, b

	// Duplicate WITHIN the new batch.
	res := callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        d.ID,
		"expected_version": 1,
		"concepts":         []string{"c", "c"},
		"prerequisites":    map[string][]string{},
	})
	if !res.IsError || !strings.Contains(resultText(res), "duplicate concept name") {
		t.Fatalf("expected duplicate-in-batch error, got %q", resultText(res))
	}
}

func TestInitDomain_RejectsUnknownPrereqValue(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name": "x", "concepts": []string{"a"},
		"prerequisites": map[string][]string{"a": {"ghost"}}})
	if !res.IsError {
		t.Fatalf("expected validation error for prereq pointing at unknown concept, got %q", resultText(res))
	}
}

func TestInitDomain_RejectsUnknownPrereqKey(t *testing.T) {
	_, deps := setupToolsTest(t)
	// prereq KEY references a concept not in concepts[]
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name": "x", "concepts": []string{"a"},
		"prerequisites": map[string][]string{"ghost": {"a"}}})
	if !res.IsError {
		t.Fatalf("expected validation error for prereq key not in concepts[]")
	}
}

func TestAddConcepts_RejectsUnknownPrereqValue(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math") // contains a, b

	// Adding c with a prereq that references "ghost" should fail —
	// "ghost" is not in the merged universe (existing[a,b] + new[c]).
	res := callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        d.ID,
		"expected_version": 1,
		"concepts":         []string{"c"},
		"prerequisites":    map[string][]string{"c": {"ghost"}},
	})
	if !res.IsError {
		t.Fatalf("expected validation error for unknown prereq in add_concepts, got %q", resultText(res))
	}

	// But referencing existing concept "a" must still succeed.
	res = callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        d.ID,
		"expected_version": 1,
		"concepts":         []string{"c"},
		"prerequisites":    map[string][]string{"c": {"a"}},
	})
	if res.IsError {
		t.Fatalf("expected success when prereq points at existing concept, got %q", resultText(res))
	}
}
