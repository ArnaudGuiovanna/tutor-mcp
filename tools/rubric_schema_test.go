// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"math"
	"strings"
	"testing"
)

func TestNormalizeRubricJSONStableObject(t *testing.T) {
	got, warnings, err := normalizeRubricJSON(`{
		"criteria": [
			{"id": "correctness", "description": "Right answer", "max_score": 2},
			{"id": "reasoning", "max_score": 1}
		],
		"passing_score": 2.5
	}`)
	if err != nil {
		t.Fatalf("normalizeRubricJSON returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}

	criteria := rubricCriteriaForTest(t, got)
	if len(criteria) != 2 {
		t.Fatalf("criteria length = %d, want 2", len(criteria))
	}
	if criteria[0]["id"] != "correctness" {
		t.Fatalf("criteria[0].id = %v", criteria[0]["id"])
	}
	if criteria[0]["description"] != "Right answer" {
		t.Fatalf("criteria[0].description = %v", criteria[0]["description"])
	}
	assertRubricFloat(t, criteria[0]["max_score"], 2)
	assertRubricFloat(t, got["passing_score"], 2.5)
}

func TestNormalizeRubricScoreJSONStableWithRubric(t *testing.T) {
	rubric, _, err := normalizeRubricJSON(`{
		"criteria": [
			{"id": "correctness", "max_score": 1},
			{"id": "reasoning", "max_score": 2}
		]
	}`)
	if err != nil {
		t.Fatalf("normalizeRubricJSON returned error: %v", err)
	}

	got, warnings, err := normalizeRubricScoreJSON(`{
		"criteria_scores": [
			{"id": "correctness", "score": 1, "evidence": "exact"},
			{"id": "reasoning", "score": 1.5, "error_type": "LOGIC_ERROR"}
		],
		"summary": "solid",
		"confidence": 0.75
	}`, rubric)
	if err != nil {
		t.Fatalf("normalizeRubricScoreJSON returned error: %v", err)
	}
	requireRubricWarning(t, warnings, "max_total missing; inferred from rubric")

	scores := rubricScoresForTest(t, got)
	if len(scores) != 2 {
		t.Fatalf("criteria_scores length = %d, want 2", len(scores))
	}
	if scores[0]["id"] != "correctness" || scores[0]["evidence"] != "exact" {
		t.Fatalf("unexpected first score: %#v", scores[0])
	}
	if scores[1]["id"] != "reasoning" || scores[1]["error_type"] != "LOGIC_ERROR" {
		t.Fatalf("unexpected second score: %#v", scores[1])
	}
	assertRubricFloat(t, got["total"], 2.5)
	assertRubricFloat(t, got["max_total"], 3)
	assertRubricFloat(t, got["confidence"], 0.75)
	if got["summary"] != "solid" {
		t.Fatalf("summary = %v", got["summary"])
	}
}

func TestNormalizeRubricScoreJSONCrossSchemaWarnings(t *testing.T) {
	rubric, _, err := normalizeRubricJSON(`{
		"criteria": [
			{"id": "correctness", "max_score": 1},
			{"id": "reasoning", "max_score": 1}
		]
	}`)
	if err != nil {
		t.Fatalf("normalizeRubricJSON returned error: %v", err)
	}

	got, warnings, err := normalizeRubricScoreJSON(`{
		"criteria_scores": [
			{"id": "correctness", "score": 1.2},
			{"id": "extra", "score": 0.5}
		]
	}`, rubric)
	if err != nil {
		t.Fatalf("normalizeRubricScoreJSON returned error: %v", err)
	}
	requireRubricWarning(t, warnings, "score exceeds rubric max_score")
	requireRubricWarning(t, warnings, "extra")
	requireRubricWarning(t, warnings, "missing rubric criterion")
	assertRubricFloat(t, got["total"], 1.7)
	assertRubricFloat(t, got["max_total"], 2)
}

func TestNormalizeRubricScoreJSONWithoutRubricInfersMaxTotal(t *testing.T) {
	got, warnings, err := normalizeRubricScoreJSON(`{"criteria_scores":[{"id":"a","score":0.8},{"id":"b","score":3},{"id":"c","score":1,"max_score":4}]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRubricWarning(t, warnings, "inferred from criteria_scores")
	assertRubricFloat(t, got["total"], 4.8)
	assertRubricFloat(t, got["max_total"], 8)
}

// The advertised schemas are closed, so the standalone normalizers decode only
// the canonical shape; their errors describe that shape, not legacy ones.
func TestNormalizeRubricSchemaRejectsShapesTheSchemaRefuses(t *testing.T) {
	for raw, want := range map[string]string{
		`[{"id":"x","max_score":1}]`:                                  "rubric must be an object with a criteria array",
		`{"criteria":{"x":{"max_score":1}}}`:                          "rubric.criteria must be a non-empty array",
		`{"criteria":["clear"]}`:                                      "rubric.criteria[0] must be an object",
		`{"criteria":[{"id":"x","max_score":"2"}]}`:                   "rubric.criteria[0].max_score must be a positive number",
		`{"criteria":[{"name":"x","max_score":1}]}`:                   "rubric.criteria[0].id must be a non-empty string",
		`{"criteria":[{"id":"x","max_score":1}],"passing_score":"1"}`: "rubric.passing_score must be a positive number",
		`{"criteria":[{"id":"x","max_score":1}],"max_total":3}`:       "rubric.max_total contradicts the criteria",
	} {
		if _, _, err := normalizeRubricJSON(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("rubric %s: err=%v, want %q", raw, err, want)
		}
	}
	for raw, want := range map[string]string{
		`0.8`:                         "rubric_score must be an object with a criteria_scores array",
		`[{"id":"x","score":1}]`:      "rubric_score must be an object with a criteria_scores array",
		`{"total":1}`:                 "rubric_score.criteria_scores must be a non-empty array",
		`{"criteria_scores":{"x":1}}`: "rubric_score.criteria_scores must be a non-empty array",
		`{"criteria_scores":[{"id":"x","score":"1"}]}`:                  "rubric_score.criteria_scores[0].score must be a non-negative number",
		`{"criteria_scores":[{"id":"x","score":1}],"confidence":"0.8"}`: "rubric_score.confidence must be a number in [0, 1]",
	} {
		if _, _, err := normalizeRubricScoreJSON(raw, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("rubric_score %s: err=%v, want %q", raw, err, want)
		}
	}
}

func TestNormalizeRubricSchemaRejectsInvalidInputs(t *testing.T) {
	if _, _, err := normalizeRubricJSON(`42`); err == nil {
		t.Fatal("expected scalar rubric to be rejected")
	}
	if _, _, err := normalizeRubricJSON(`{"criteria":[{"id":"x","max_score":0}]}`); err == nil {
		t.Fatal("expected zero max_score to be rejected")
	}
	if _, _, err := normalizeRubricScoreJSON(`{"criteria_scores":[{"id":"x","evidence":"missing score"}]}`, nil); err == nil {
		t.Fatal("expected missing score to be rejected")
	}
	if _, _, err := normalizeRubricScoreJSON(`{"criteria_scores":[{"id":"x","score":1}],"confidence":2}`, nil); err == nil {
		t.Fatal("expected out-of-range confidence to be rejected")
	}
}

func rubricCriteriaForTest(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	criteria, ok := got["criteria"].([]map[string]any)
	if !ok {
		t.Fatalf("criteria type = %T, want []map[string]any", got["criteria"])
	}
	return criteria
}

func rubricScoresForTest(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	scores, ok := got["criteria_scores"].([]map[string]any)
	if !ok {
		t.Fatalf("criteria_scores type = %T, want []map[string]any", got["criteria_scores"])
	}
	return scores
}

func assertRubricFloat(t *testing.T, got any, want float64) {
	t.Helper()
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("value type = %T, want float64", got)
	}
	if math.Abs(f-want) > 1e-9 {
		t.Fatalf("value = %v, want %v", f, want)
	}
}

func requireRubricWarning(t *testing.T, warnings []string, needle string) {
	t.Helper()
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			return
		}
	}
	t.Fatalf("expected warning containing %q, got %v", needle, warnings)
}
