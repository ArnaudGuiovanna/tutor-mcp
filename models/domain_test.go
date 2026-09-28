// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package models

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// ─── ParseGoalRelevance ────────────────────────────────────────────────────

func TestParseGoalRelevance_EmptyJSONReturnsNil(t *testing.T) {
	d := &Domain{ID: "d1", GoalRelevanceJSON: ""}
	if got := d.ParseGoalRelevance(); got != nil {
		t.Errorf("empty JSON: want nil, got %+v", got)
	}
}

func TestParseGoalRelevance_MalformedJSONReturnsNilAndWarns(t *testing.T) {
	// The "WARN-on-corruption" path: malformed JSON must not panic, must
	// return nil, and must log a WARN. We don't assert the WARN itself
	// (would couple the test to slog.Default), only that the function
	// degrades silently.
	d := &Domain{ID: "d1", GoalRelevanceJSON: "{not_valid_json"}
	if got := d.ParseGoalRelevance(); got != nil {
		t.Errorf("malformed JSON: want nil, got %+v", got)
	}
}

func TestParseGoalRelevance_ValidStructureRoundTrips(t *testing.T) {
	// A full structured payload with relevance map + for_graph_version.
	json := `{"for_graph_version":2,"relevance":{"A":0.9,"B":0.4},"set_at":"2026-01-01T00:00:00Z"}`
	d := &Domain{ID: "d1", GoalRelevanceJSON: json}
	gr := d.ParseGoalRelevance()
	if gr == nil {
		t.Fatal("expected non-nil GoalRelevance")
	}
	if gr.ForGraphVersion != 2 {
		t.Errorf("ForGraphVersion: want 2, got %d", gr.ForGraphVersion)
	}
	if gr.Relevance["A"] != 0.9 || gr.Relevance["B"] != 0.4 {
		t.Errorf("Relevance map: %+v", gr.Relevance)
	}
}

func TestParseGoalRelevance_EmptyJSONObject(t *testing.T) {
	// Edge case: the JSON is structurally valid but has no fields. Should
	// still parse to a zero-value GoalRelevance (not nil).
	d := &Domain{ID: "d1", GoalRelevanceJSON: "{}"}
	gr := d.ParseGoalRelevance()
	if gr == nil {
		t.Fatal("expected non-nil GoalRelevance for {}")
	}
	if gr.ForGraphVersion != 0 {
		t.Errorf("ForGraphVersion: want 0, got %d", gr.ForGraphVersion)
	}
	if len(gr.Relevance) != 0 {
		t.Errorf("Relevance: want empty, got %+v", gr.Relevance)
	}
}

// ─── IsGoalRelevanceStale ──────────────────────────────────────────────────

func TestIsGoalRelevanceStale_NilVectorAndZeroGraphVersion(t *testing.T) {
	// No JSON, GraphVersion=0 → not stale (legacy domain, never had a
	// relevance vector and graph_version isn't tracked yet).
	d := &Domain{ID: "d1", GoalRelevanceJSON: "", GraphVersion: 0}
	if d.IsGoalRelevanceStale() {
		t.Error("nil + GraphVersion=0: want NOT stale")
	}
}

func TestIsGoalRelevanceStale_NilVectorWithGraphVersion(t *testing.T) {
	// No JSON but GraphVersion>0 → stale (the graph exists; the vector
	// was never set against it).
	d := &Domain{ID: "d1", GoalRelevanceJSON: "", GraphVersion: 1}
	if !d.IsGoalRelevanceStale() {
		t.Error("nil + GraphVersion=1: want stale")
	}
}

func TestIsGoalRelevanceStale_VersionMatch(t *testing.T) {
	d := &Domain{
		ID:                "d1",
		GoalRelevanceJSON: `{"for_graph_version":2,"relevance":{"A":0.5}}`,
		GraphVersion:      2,
	}
	if d.IsGoalRelevanceStale() {
		t.Error("for_graph_version == GraphVersion: want NOT stale")
	}
}

func TestIsGoalRelevanceStale_VersionMismatch(t *testing.T) {
	d := &Domain{
		ID:                "d1",
		GoalRelevanceJSON: `{"for_graph_version":1,"relevance":{"A":0.5}}`,
		GraphVersion:      2,
	}
	if !d.IsGoalRelevanceStale() {
		t.Error("for_graph_version < GraphVersion: want stale")
	}
}

func TestIsGoalRelevanceStale_MalformedJSONFallsBackToNilBranch(t *testing.T) {
	// Malformed JSON ⇒ ParseGoalRelevance returns nil ⇒ stale-when-
	// GraphVersion>0 branch is taken.
	d := &Domain{ID: "d1", GoalRelevanceJSON: "{garbage", GraphVersion: 1}
	if !d.IsGoalRelevanceStale() {
		t.Error("malformed JSON + GraphVersion=1: want stale")
	}
}

// ─── UncoveredConcepts ─────────────────────────────────────────────────────

func TestUncoveredConcepts_FullCoverage(t *testing.T) {
	d := &Domain{
		ID:                "d1",
		Graph:             KnowledgeSpace{Concepts: []string{"A", "B"}},
		GoalRelevanceJSON: `{"for_graph_version":1,"relevance":{"A":0.9,"B":0.4}}`,
	}
	if got := d.UncoveredConcepts(); len(got) != 0 {
		t.Errorf("full coverage: want empty, got %v", got)
	}
}

func TestUncoveredConcepts_PartialCoverage(t *testing.T) {
	d := &Domain{
		ID:                "d1",
		Graph:             KnowledgeSpace{Concepts: []string{"A", "B", "C"}},
		GoalRelevanceJSON: `{"for_graph_version":1,"relevance":{"A":0.9}}`,
	}
	got := d.UncoveredConcepts()
	sort.Strings(got)
	want := []string{"B", "C"}
	if len(got) != len(want) {
		t.Fatalf("len: want %d, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pos %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestUncoveredConcepts_EmptyVectorAllUncovered(t *testing.T) {
	// No vector at all ⇒ every concept in Graph is uncovered.
	d := &Domain{
		ID:                "d1",
		Graph:             KnowledgeSpace{Concepts: []string{"A", "B"}},
		GoalRelevanceJSON: "",
	}
	got := d.UncoveredConcepts()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Errorf("empty vector: want [A B], got %v", got)
	}
}

func TestUncoveredConcepts_EmptyGraph(t *testing.T) {
	d := &Domain{
		ID:                "d1",
		Graph:             KnowledgeSpace{Concepts: nil},
		GoalRelevanceJSON: `{"for_graph_version":1,"relevance":{"A":0.9}}`,
	}
	if got := d.UncoveredConcepts(); len(got) != 0 {
		t.Errorf("empty graph: want empty, got %v", got)
	}
}

func TestUncoveredConcepts_MalformedJSONTreatsAllAsUncovered(t *testing.T) {
	// Corrupt JSON ⇒ Parse returns nil ⇒ no concept is "covered" ⇒ every
	// concept of the graph is reported as uncovered.
	d := &Domain{
		ID:                "d1",
		Graph:             KnowledgeSpace{Concepts: []string{"A"}},
		GoalRelevanceJSON: "{not_valid",
	}
	got := d.UncoveredConcepts()
	if len(got) != 1 || got[0] != "A" {
		t.Errorf("malformed JSON: want [A], got %v", got)
	}
}

// ─── ActivityType public surface ───────────────────────────────────────────

// TestActivityType_NoInterleaving is a regression guard for sub-issue #64.
//
// `ActivityInterleaving` ("INTERLEAVING") was declared in models/domain.go
// but never emitted by any production code path (router, action selector).
// It was removed as dead code rather than implementing Rohrer-style
// interleaving (out of scope). This test parses the package source and
// asserts that no constant of type ActivityType equals the literal
// "INTERLEAVING" — failing if the dead type is ever re-introduced.
func TestActivityType_NoInterleaving(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "domain.go", nil, 0)
	if err != nil {
		t.Fatalf("parse domain.go: %v", err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if name.Name == "ActivityInterleaving" {
					t.Fatalf("models.ActivityInterleaving must not be " +
						"declared (sub-issue #64: removed as unused " +
						"dead code; no production path emits it)")
				}
			}
			for _, val := range vs.Values {
				bl, ok := val.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				if bl.Value == `"INTERLEAVING"` {
					t.Fatalf(`ActivityType literal "INTERLEAVING" must not ` +
						`be declared (sub-issue #64)`)
				}
			}
		}
	}
}

// ─── EffectiveGoalRelevance ────────────────────────────────────────────────

func TestEffectiveGoalRelevance_NilVectorStaysNil(t *testing.T) {
	d := &Domain{ID: "d1", GraphVersion: 2, Graph: KnowledgeSpace{Concepts: []string{"A", "B"}}}
	if got := d.EffectiveGoalRelevance(); got != nil {
		t.Errorf("no vector: want nil (uniform fallback), got %v", got)
	}
}

func TestEffectiveGoalRelevance_CurrentVectorKeepsOmissionAsExclusion(t *testing.T) {
	d := &Domain{
		ID:                "d1",
		GoalRelevanceJSON: `{"for_graph_version":2,"relevance":{"A":0.5}}`,
		GraphVersion:      2,
		Graph:             KnowledgeSpace{Concepts: []string{"A", "B"}},
	}
	got := d.EffectiveGoalRelevance()
	if _, covered := got["B"]; covered {
		t.Errorf("current vector: B must stay uncovered (restrictive goal), got %v", got)
	}
	if got["A"] != 0.5 {
		t.Errorf("A weight altered: %v", got)
	}
}

func TestEffectiveGoalRelevance_StaleVectorDefaultsUncoveredToMean(t *testing.T) {
	d := &Domain{
		ID:                "d1",
		GoalRelevanceJSON: `{"for_graph_version":1,"relevance":{"A":0.8,"B":0.4}}`,
		GraphVersion:      2,
		Graph:             KnowledgeSpace{Concepts: []string{"A", "B", "C"}},
	}
	got := d.EffectiveGoalRelevance()
	if got["C"] < 0.599 || got["C"] > 0.601 {
		t.Errorf("stale vector: C should default to the mean 0.6, got %v", got["C"])
	}
	if got["A"] != 0.8 || got["B"] != 0.4 {
		t.Errorf("existing weights altered: %v", got)
	}
	// The stored vector is not mutated.
	if gr := d.ParseGoalRelevance(); len(gr.Relevance) != 2 {
		t.Errorf("stored vector mutated: %v", gr.Relevance)
	}
}

func TestDefaultUncoveredRelevance_Bounds(t *testing.T) {
	if got := DefaultUncoveredRelevance(nil); got != 1.0 {
		t.Errorf("empty vector: want 1.0, got %v", got)
	}
	if got := DefaultUncoveredRelevance(map[string]float64{"A": 0, "B": 0.02}); got != 0.1 {
		t.Errorf("near-zero mean: want floor 0.1, got %v", got)
	}
	if got := DefaultUncoveredRelevance(map[string]float64{"A": 1, "B": 1}); got != 1 {
		t.Errorf("full relevance: want 1, got %v", got)
	}
}
