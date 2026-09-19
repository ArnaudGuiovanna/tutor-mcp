// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package assessment

import (
	"reflect"
	"testing"
)

// The rubric schema layer accepts criteria_scores as an object keyed by
// criterion id, so a score in that shape clears validation and reaches this
// package unchanged through EvaluateJSON. Refusing it here fails an evaluation
// the caller was already told was acceptable: a recorded tutoring session shows
// exactly that, the attempt dying on "criteria_scores must be an array" after
// the rubric had approved the same payload.
func TestEvaluateAcceptsCriteriaScoresObject(t *testing.T) {
	rubric, err := ParseRubric(testRubric)
	if err != nil {
		t.Fatal(err)
	}
	want, err := EvaluateJSON(rubric, testScores)
	if err != nil {
		t.Fatalf("array form rejected: %v", err)
	}

	for name, raw := range map[string]string{
		"objets par identifiant": `{"criteria_scores":{"reasoning":{"score":0.5,"evidence":"Incomplete justification."},"result":{"score":2,"evidence":"Correct result."}},"total":2.5,"max_total":3,"summary":"Review of the committed response.","confidence":0.8}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := EvaluateJSON(rubric, raw)
			if err != nil {
				t.Fatalf("object form rejected after the rubric accepted it: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("object form scored differently:\n got=%+v\nwant=%+v", got, want)
			}
		})
	}
}
