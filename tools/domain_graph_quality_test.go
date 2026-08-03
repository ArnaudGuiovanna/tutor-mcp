// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestInitDomain_ReturnsGraphQualityReport(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "go",
		"concepts":      []string{"Basics", "Go routines", "goroutines", "channels"},
		"prerequisites": map[string][]string{},
	})
	if res.IsError {
		t.Fatalf("expected warning report, not error: %s", resultText(res))
	}
	out := decodeResult(t, res)
	report := out["graph_quality_report"].(map[string]any)
	if report["quality"] != "warning" {
		t.Fatalf("quality = %v, want warning", report["quality"])
	}
	guidance := out["graph_quality_guidance"].(map[string]any)
	if guidance["required"] != false || guidance["prompt"] == "" {
		t.Fatalf("unexpected graph guidance: %+v", guidance)
	}
}

func TestValidateDomainGraph_ReturnsReportForActiveDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	_, err := store.CreateDomain(context.Background(), "L_owner", "flat", "", models.KnowledgeSpace{
		Concepts:      []string{"Basics", "channels", "goroutines", "interfaces"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerValidateDomainGraph, "L_owner", "validate_domain_graph", map[string]any{})
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(res))
	}
	out := decodeResult(t, res)
	report := out["graph_quality_report"].(map[string]any)
	if report["quality"] != "warning" {
		t.Fatalf("quality = %v, want warning", report["quality"])
	}
	if _, ok := out["graph_quality_guidance"].(map[string]any); !ok {
		t.Fatalf("expected graph_quality_guidance, got %+v", out)
	}
}

func TestAddConcepts_RejectsCycleAfterMergingExistingGraph(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerAddConcepts, "L_owner", "add_concepts", map[string]any{
		"domain_id":        d.ID,
		"expected_version": 1,
		"concepts":         []string{"c"},
		"prerequisites": map[string][]string{
			"c": {"b"},
			"a": {"c"},
		},
	})
	if !res.IsError {
		t.Fatalf("expected merged cycle rejection, got %q", resultText(res))
	}
	if !strings.Contains(strings.ToLower(resultText(res)), "cycle") {
		t.Fatalf("expected cycle error, got %q", resultText(res))
	}
	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, concept := range got.Graph.Concepts {
		if concept == "c" {
			t.Fatalf("concept c should not be persisted after rejected cycle: %+v", got.Graph)
		}
	}
}
