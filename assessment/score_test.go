// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package assessment

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"tutor-mcp/models"
)

const testRubric = `{"criteria":[{"id":"result","description":"Correct generated result.","max_score":2},{"id":"reasoning","description":"Justifies the method.","max_score":1}],"passing_score":2.5,"answer_key":"Generated reference."}`
const testScores = `{"criteria_scores":[{"id":"reasoning","score":0.5,"evidence":"Incomplete justification."},{"id":"result","score":2,"evidence":"Correct result."}],"total":2.5,"max_total":3,"summary":"Review of the committed response.","confidence":0.8}`

func TestEvaluateCanonicalRoundTrip(t *testing.T) {
	rubric, err := ParseRubric(testRubric)
	if err != nil {
		t.Fatal(err)
	}
	result, err := EvaluateJSON(rubric, testScores)
	if err != nil || result.Total != 2.5 || !result.Passed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	items := result.Score["criteria_scores"].([]map[string]any)
	if items[0]["id"] != "result" || items[1]["id"] != "reasoning" {
		t.Fatal("scores did not use the frozen criterion order")
	}
	raw, err := json.Marshal(result.Score)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := EvaluateJSON(rubric, string(raw))
	if err != nil || !reflect.DeepEqual(reloaded, result) {
		t.Fatalf("canonical result did not round-trip: %+v err=%v", reloaded, err)
	}
}

func TestEvaluateRejectsAmbiguousOrContradictoryScoring(t *testing.T) {
	rubric, err := ParseRubric(testRubric)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"duplicate JSON key":              strings.Replace(testScores, `"score":2`, `"score":0,"score":2`, 1),
		"escaped duplicate key":           strings.Replace(testScores, `"score":2`, `"score":0,"scor\u0065":2`, 1),
		"duplicate aggregate":             strings.Replace(testScores, `"total":2.5`, `"total":0,"total":2.5`, 1),
		"numeric string":                  strings.Replace(testScores, `"score":2`, `"score":"2"`, 1),
		"null score":                      strings.Replace(testScores, `"score":2`, `"score":null`, 1),
		"boolean score":                   strings.Replace(testScores, `"score":2`, `"score":true`, 1),
		"infinite score":                  strings.Replace(testScores, `"score":2`, `"score":1e999`, 1),
		"negative score":                  strings.Replace(testScores, `"score":2`, `"score":-1`, 1),
		"above maximum":                   strings.Replace(testScores, `"score":2`, `"score":3`, 1),
		"unknown criterion":               strings.Replace(testScores, `"id":"result"`, `"id":"other"`, 1),
		"normalized criterion":            strings.Replace(testScores, `"id":"result"`, `"id":" Result "`, 1),
		"duplicate criterion":             strings.Replace(testScores, `"id":"result"`, `"id":"reasoning"`, 1),
		"missing criterion":               `{"criteria_scores":[{"id":"result","score":2,"evidence":"Correct."}]}`,
		"missing evidence":                strings.Replace(testScores, `,"evidence":"Correct result."`, "", 1),
		"empty evidence":                  strings.Replace(testScores, `"Correct result."`, `" "`, 1),
		"evidence alias":                  strings.Replace(testScores, `"evidence":"Correct result."`, `"feedback":"Correct result."`, 1),
		"total contradiction":             strings.Replace(testScores, `"total":2.5`, `"total":3`, 1),
		"maximum contradiction":           strings.Replace(testScores, `"max_total":3`, `"max_total":4`, 1),
		"criterion maximum contradiction": strings.Replace(testScores, `"score":2`, `"score":2,"max_score":3`, 1),
		"claimed trust":                   strings.Replace(testScores, `"total":2.5`, `"trusted_evaluation":true,"total":2.5`, 1),
		"unknown grading rule":            strings.Replace(testScores, `"score":2`, `"score":2,"weight":3`, 1),
		"score aliases":                   strings.Replace(testScores, `"criteria_scores"`, `"scores"`, 1),
		"multiple JSON values":            testScores + `{}`,
		"scalar":                          `1`,
		"null":                            `null`,
		"array":                           `[]`,
		"oversized":                       strings.Repeat(" ", MaxJSONBytes) + testScores,
		"invalid UTF8":                    strings.Replace(testScores, "Correct result.", string([]byte{0xff}), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if result, err := EvaluateJSON(rubric, raw); err == nil {
				t.Fatalf("invalid score accepted: %+v", result)
			}
		})
	}
}

func TestParseRubricRejectsAmbiguousJSON(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate passing rule":      strings.Replace(testRubric, `"passing_score":2.5`, `"passing_score":1,"passing_score":2.5`, 1),
		"duplicate criterion maximum": strings.Replace(testRubric, `"max_score":2`, `"max_score":1,"max_score":2`, 1),
		"duplicate anchor":            `{"criteria":[{"id":"x","description":"Defined.","max_score":1,"anchors":[{"score":0,"score":1,"description":"Defined."}]}],"passing_score":1}`,
		"deep nesting":                strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34),
		"null":                        `null`,
		"truncated object":            `{"criteria":[`,
		"multiple values":             testRubric + ` true`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRubric(raw); err == nil {
				t.Fatal("ambiguous rubric accepted")
			}
		})
	}
}

func TestEvaluateDecimalThresholdAndOrder(t *testing.T) {
	for _, tc := range []struct {
		name           string
		maxima, scores []float64
		passing, total float64
		passed         bool
	}{
		{"zero below tiny threshold", []float64{1}, []float64{0}, 1e-12, 0, false},
		{"below threshold", []float64{1}, []float64{0.9999999999}, 1, 0.9999999999, false},
		{"decimal equality", []float64{0.1, 0.7}, []float64{0.1, 0.7}, 0.8, 0.8, true},
		{"very different magnitudes", []float64{1e16, 1, 1}, []float64{1e16, 1, 1}, 1e16 + 2, 1e16 + 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rubric := models.AssessmentRubric{PassingScore: tc.passing}
			items := make([]map[string]any, 0, len(tc.scores))
			for i, value := range tc.scores {
				id := string(rune('a' + i))
				rubric.Criteria = append(rubric.Criteria, models.AssessmentRubricCriterion{ID: id, Description: "Defined.", MaxScore: tc.maxima[i]})
				items = append(items, map[string]any{"id": id, "score": value, "evidence": "Observed response."})
			}
			for i := 0; i < 10; i++ {
				result, err := Evaluate(rubric, map[string]any{"criteria_scores": items})
				if err != nil || result.Total != tc.total || result.Passed != tc.passed {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				items[0], items[len(items)-1] = items[len(items)-1], items[0]
			}
		})
	}
}

func TestEvaluateRejectsNonFiniteTypedScores(t *testing.T) {
	rubric, err := ParseRubric(testRubric)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := Evaluate(rubric, map[string]any{"criteria_scores": []map[string]any{{"id": "result", "score": value, "evidence": "Observed."}, {"id": "reasoning", "score": 1.0, "evidence": "Observed."}}})
		if err == nil {
			t.Fatalf("non-finite score accepted: %v", value)
		}
	}
}

func FuzzBoundAssessmentJSON(f *testing.F) {
	f.Add(testRubric, testScores)
	f.Add(`null`, `{"criteria_scores":null}`)
	f.Fuzz(func(t *testing.T, rawRubric, rawScores string) {
		rubric, err := ParseRubric(rawRubric)
		if err != nil {
			return
		}
		result, err := EvaluateJSON(rubric, rawScores)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(result.Score)
		if err != nil {
			t.Fatal(err)
		}
		reloaded, err := EvaluateJSON(rubric, string(encoded))
		if err != nil || reloaded.Total != result.Total || reloaded.Passed != result.Passed {
			t.Fatalf("accepted score is not stable: %+v err=%v", reloaded, err)
		}
	})
}
