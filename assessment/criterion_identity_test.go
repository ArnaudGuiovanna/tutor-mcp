// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package assessment

import (
	"encoding/json"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestRubricPreservesOpaqueCriterionIdentities(t *testing.T) {
	// Curriculum IDs are opaque base64url values, not lower-case slugs. These
	// distinct IDs would collide if case and punctuation were normalized.
	ids := []string{"criterion_Ab-C_D", "criterion_ab_c_d", "criterion_Ab_C-D"}
	for range 16 {
		id, err := models.NewCurriculumStableID("criterion")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		raw, _ := json.Marshal(map[string]any{"criteria": []map[string]any{{"id": id, "description": "Preserves equality", "max_score": 1}}, "passing_score": 1})
		rubric, err := ParseRubric(string(raw))
		if err != nil {
			t.Fatalf("curriculum criterion rejected: %v", err)
		}
		if rubric.Criteria[0].ID != id {
			t.Fatal("criterion identity changed when freezing the rubric")
		}
		score := map[string]any{"criteria_scores": []map[string]any{{"id": id, "score": 1.0, "evidence": "Same operation on both sides"}}}
		result, err := Evaluate(rubric, score)
		if err != nil || !result.Passed || result.Score["criteria_scores"].([]map[string]any)[0]["id"] != id {
			t.Fatalf("exact criterion identity did not survive grading: %v", err)
		}
		for _, other := range ids {
			if other == id {
				continue
			}
			score["criteria_scores"].([]map[string]any)[0]["id"] = other
			if _, err := Evaluate(rubric, score); err == nil {
				t.Fatal("a different criterion was accepted as the frozen one")
			}
		}
	}
}

func TestRubricRejectsAmbiguousCriterionIdentities(t *testing.T) {
	for _, id := range []string{"", " result", "result ", "two words", "a/b", "a\x00b", "a\nb", "a\u200bb"} {
		raw, _ := json.Marshal(map[string]any{"criteria": []map[string]any{{"id": id, "description": "Correct", "max_score": 1}}, "passing_score": 1})
		if _, err := ParseRubric(string(raw)); err == nil || !strings.Contains(err.Error(), ".id") {
			t.Fatalf("ambiguous criterion ID accepted or error lacks field: %q, %v", id, err)
		}
	}
}
