// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package assessment

import (
	"strings"
	"testing"

	"tutor-mcp/models"
)

func floorRubric(passing float64, maxScores ...float64) models.AssessmentRubric {
	rubric := models.AssessmentRubric{PassingScore: passing}
	for i, max := range maxScores {
		rubric.Criteria = append(rubric.Criteria, models.AssessmentRubricCriterion{
			ID: string(rune('a' + i)), Description: "observable", MaxScore: max,
		})
	}
	return rubric
}

func TestValidateDecisionRubricFloor(t *testing.T) {
	cases := []struct {
		name     string
		activity models.ActivityType
		rubric   models.AssessmentRubric
		wantErr  string
	}{
		{"challenge below floor", models.ActivityMasteryChallenge, floorRubric(0.5, 5, 5), "passing_score >= 6"},
		{"challenge single criterion", models.ActivityMasteryChallenge, floorRubric(1, 1), "at least 2 criteria"},
		{"challenge at floor", models.ActivityMasteryChallenge, floorRubric(6, 5, 5), ""},
		{"transfer below floor", models.ActivityTransferProbe, floorRubric(1, 2, 1), "passing_score >= 1.8"},
		{"transfer single criterion allowed", models.ActivityTransferProbe, floorRubric(0.6, 1), ""},
		{"recall below floor", models.ActivityRecall, floorRubric(0.1, 1), "below the required floor"},
		{"practice keeps structural rules only", models.ActivityPractice, floorRubric(0.1, 1), ""},
	}
	for _, tc := range cases {
		if err := ValidateRubric(tc.rubric); err != nil {
			t.Fatalf("%s: fixture is structurally invalid: %v", tc.name, err)
		}
		err := ValidateDecisionRubric(tc.activity, tc.rubric)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error=%v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}
