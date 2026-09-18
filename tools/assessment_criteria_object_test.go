// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import "testing"

// normalizeRubricScoreCriteria accepts criteria_scores as an object keyed by
// criterion id, so a client that passes that shape clears rubric validation.
// deriveAssessmentOutcome has to accept the same shape, otherwise the score is
// rejected after the rubric already approved it.
func TestDeriveAssessmentOutcomeAcceptsCriteriaScoresObject(t *testing.T) {
	rubric := map[string]any{
		"criteria": []any{
			map[string]any{"id": "recall", "max_score": 1.0},
			map[string]any{"id": "transfer", "max_score": 1.0},
		},
		"passing_score": 1.0,
	}

	array := map[string]any{"criteria_scores": []any{
		map[string]any{"id": "recall", "score": 1.0},
		map[string]any{"id": "transfer", "score": 1.0},
	}}
	wantTotal, wantPassed, err := deriveAssessmentOutcome(rubric, array)
	if err != nil {
		t.Fatalf("array form rejected: %v", err)
	}

	for name, object := range map[string]map[string]any{
		"objets par identifiant": {"criteria_scores": map[string]any{
			"recall":   map[string]any{"score": 1.0},
			"transfer": map[string]any{"score": 1.0},
		}},
		"nombres par identifiant": {"criteria_scores": map[string]any{
			"recall":   1.0,
			"transfer": 1.0,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			total, passed, err := deriveAssessmentOutcome(rubric, object)
			if err != nil {
				t.Fatalf("object form rejected: %v", err)
			}
			if total != wantTotal || passed != wantPassed {
				t.Fatalf("object form disagrees with array form: got (%v, %v), want (%v, %v)",
					total, passed, wantTotal, wantPassed)
			}
		})
	}
}
