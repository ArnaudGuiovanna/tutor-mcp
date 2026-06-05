// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"strings"
	"testing"
)

func TestValidateConcepts(t *testing.T) {
	cases := []struct {
		name     string
		concepts []string
		prereqs  map[string][]string
		wantErr  string
	}{
		{
			name:     "ok small graph",
			concepts: []string{"a", "b", "c"},
			prereqs:  map[string][]string{"b": {"a"}},
		},
		{
			name:     "empty concept name rejected",
			concepts: []string{"a", "", "c"},
			wantErr:  "empty concept name",
		},
		{
			name:     "concept name too long",
			concepts: []string{strings.Repeat("x", maxConceptNameLen+1)},
			wantErr:  "too long",
		},
		{
			name:     "too many concepts",
			concepts: makeStrings(maxConceptsPerCall + 1),
			wantErr:  "too many concepts",
		},
		{
			name:     "too many prereqs per node",
			concepts: []string{"target"},
			prereqs:  map[string][]string{"target": makeStrings(maxPrereqEntriesPerNode + 1)},
			wantErr:  "too many prerequisites",
		},
		{
			name:     "prereq value too long",
			concepts: []string{"a"},
			prereqs:  map[string][]string{"a": {strings.Repeat("y", maxConceptNameLen+1)}},
			wantErr:  "prerequisite value too long",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConcepts(tc.concepts, tc.prereqs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidateConcepts_CycleDetection covers issue #62: the prerequisite
// graph must be a DAG. Without this guard, KST's ComputeFrontier and the
// concept_selector logic would loop forever on a cyclic graph (a node's
// prereq chain never reaches a terminal `mastery >= threshold`).
//
// Convention: in `prerequisites`, an entry `c -> [p1, p2]` encodes edges
// p1 -> c and p2 -> c (prereqs unlock c). A cycle in that prereq-DAG means
// some concept transitively depends on itself.
func TestValidateConcepts_CycleDetection(t *testing.T) {
	cases := []struct {
		name     string
		concepts []string
		prereqs  map[string][]string
		wantErr  string // empty ⇒ expect success
	}{
		{
			name:     "valid DAG (regression guard)",
			concepts: []string{"a", "b", "c", "d"},
			prereqs: map[string][]string{
				"b": {"a"},
				"c": {"b"},
				"d": {"c", "a"},
			},
			wantErr: "",
		},
		{
			name:     "self-loop A->A",
			concepts: []string{"a"},
			prereqs:  map[string][]string{"a": {"a"}},
			wantErr:  "cycle",
		},
		{
			name:     "2-cycle A->B->A",
			concepts: []string{"a", "b"},
			prereqs: map[string][]string{
				"a": {"b"},
				"b": {"a"},
			},
			wantErr: "cycle",
		},
		{
			name:     "3-cycle A->B->C->A",
			concepts: []string{"a", "b", "c"},
			prereqs: map[string][]string{
				"b": {"a"},
				"c": {"b"},
				"a": {"c"},
			},
			wantErr: "cycle",
		},
		{
			name:     "disconnected graph with one cycle",
			concepts: []string{"x", "y", "a", "b", "c"},
			prereqs: map[string][]string{
				"y": {"x"}, // clean DAG component
				"b": {"a"}, // cycle component
				"c": {"b"},
				"a": {"c"},
			},
			wantErr: "cycle",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConcepts(tc.concepts, tc.prereqs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error on valid DAG: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestInitDomain_RejectsCyclicGraph covers the integration path: an
// init_domain MCP call with a cyclic prereq graph must be rejected before
// any DB write. We assert (a) the call returns an MCP error, (b) the error
// text mentions "cycle" so the LLM can recover, and (c) no domain row was
// persisted.
func TestInitDomain_RejectsCyclicGraph(t *testing.T) {
	store, deps := setupToolsTest(t)

	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":     "math",
		"concepts": []string{"a", "b", "c"},
		"prerequisites": map[string][]string{
			"b": {"a"},
			"c": {"b"},
			"a": {"c"}, // closes the cycle
		},
	})
	if !res.IsError {
		t.Fatalf("expected cycle error, got success: %q", resultText(res))
	}
	if !strings.Contains(strings.ToLower(resultText(res)), "cycle") {
		t.Fatalf("expected error mentioning cycle, got %q", resultText(res))
	}
	if d, _ := store.GetDomainByLearner("L_owner"); d != nil {
		t.Fatalf("domain should not have been persisted on cycle reject, got %+v", d)
	}
}

func TestInitDomain_RejectsSelfLoop(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "math",
		"concepts":      []string{"a"},
		"prerequisites": map[string][]string{"a": {"a"}},
	})
	if !res.IsError {
		t.Fatalf("expected cycle error for self-loop, got success: %q", resultText(res))
	}
	if !strings.Contains(strings.ToLower(resultText(res)), "cycle") {
		t.Fatalf("expected error mentioning cycle, got %q", resultText(res))
	}
}

func TestValidateValueFramings(t *testing.T) {
	if err := validateValueFramings(nil); err != nil {
		t.Fatalf("nil framings should be ok, got %v", err)
	}
	ok := &ValueFramingsInput{Financial: "short"}
	if err := validateValueFramings(ok); err != nil {
		t.Fatalf("short framing rejected: %v", err)
	}
	tooLong := &ValueFramingsInput{Financial: strings.Repeat("a", maxValueFramingLen+1)}
	if err := validateValueFramings(tooLong); err == nil {
		t.Fatal("expected error for oversized framing")
	}
}

func makeStrings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "c"
	}
	return out
}
