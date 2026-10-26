// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestPrepareAssessmentSchemaExplainsCanonicalRubric(t *testing.T) {
	_, deps := setupToolsTest(t)
	schema := advertisedInputSchema(t, registerPrepareAssessmentAttempt, deps, "prepare_assessment_attempt")
	rubric := schemaProperty(t, schema, "rubric_json")
	if rubric["type"] != "string" || rubric["contentMediaType"] != "application/json" {
		t.Fatalf("existing JSON string contract changed or content type is missing: %v", rubric)
	}
	description, _ := rubric["description"].(string)
	for _, field := range []string{"criteria", "description", "max_score", "anchors", "array", "score", "passing_score", "answer_key", "string", "without decision_id"} {
		if !strings.Contains(description, field) {
			t.Errorf("rubric description does not explain %q: %s", field, description)
		}
	}
	content, ok := rubric["contentSchema"].(map[string]any)
	if !ok {
		t.Fatal("tools/list does not expose the canonical JSON content structure")
	}
	assertRequired := func(schema map[string]any, want ...string) {
		t.Helper()
		values, _ := schema["required"].([]any)
		var got []string
		for _, value := range values {
			got = append(got, value.(string))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("required fields=%v want=%v", got, want)
		}
	}
	assertRequired(content, "criteria", "passing_score")
	criteria := schemaProperty(t, content, "criteria")
	if criteria["type"] != "array" {
		t.Fatal("criteria must be advertised as an array")
	}
	criterion := criteria["items"].(map[string]any)
	assertRequired(criterion, "id", "description", "max_score")
	anchors := schemaProperty(t, criterion, "anchors")
	if anchors["type"] != "array" {
		t.Fatal("anchors must be advertised as an array rather than a score-keyed object")
	}
	assertRequired(anchors["items"].(map[string]any), "score", "description")
	if schemaProperty(t, content, "answer_key")["type"] != "string" {
		t.Fatal("answer_key must be advertised as a string")
	}
	examples, _ := rubric["examples"].([]any)
	if len(examples) != 1 {
		t.Fatal("expected one complete copyable rubric_json example")
	}
	example := examples[0].(string)
	parsed, err := normalizeAssessmentRubric(example)
	if err != nil || len(parsed.Criteria) != 1 || len(parsed.Criteria[0].Anchors) != 2 || parsed.AnswerKey == "" {
		t.Fatalf("advertised example is not accepted by strict bound parser: rubric=%+v err=%v", parsed, err)
	}
	if _, _, err := normalizeRubricJSON(example); err != nil {
		t.Fatalf("canonical example is not compatible with standalone assessments: %v", err)
	}
}

func TestBoundAssessmentRejectsGuessedAnchorAndAnswerKeyShapes(t *testing.T) {
	for _, field := range []string{"anchors", "answer_key"} {
		t.Run(field, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal([]byte(prepareAssessmentRubricExample), &document); err != nil {
				t.Fatal(err)
			}
			if field == "anchors" {
				document["criteria"].([]any)[0].(map[string]any)[field] = map[string]any{"0": "Incorrect", "2": "Correct"}
			} else {
				document[field] = map[string]any{"answer": "Expected result"}
			}
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := normalizeAssessmentRubric(string(raw)); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("guessed %s shape was accepted or failed without a useful field: %v", field, err)
			}
		})
	}
}
