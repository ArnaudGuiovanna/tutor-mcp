// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestRecallEligible_OnlyAcquiredCards(t *testing.T) {
	last := time.Now().UTC().Add(-72 * time.Hour)
	for name, tc := range map[string]struct {
		cs   *models.ConceptState
		want bool
	}{
		"nil":                      {nil, false},
		"new card":                 {&models.ConceptState{CardState: "new"}, false},
		"learning, never reviewed": {&models.ConceptState{CardState: "learning", LastReview: &last}, false},
		"review":                   {&models.ConceptState{CardState: "review", LastReview: &last}, true},
		"relearning":               {&models.ConceptState{CardState: "relearning", LastReview: &last}, true},
		"review without date":      {&models.ConceptState{CardState: "review"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RecallEligible(tc.cs); got != tc.want {
				t.Fatalf("RecallEligible(%+v)=%v, want %v", tc.cs, got, tc.want)
			}
		})
	}
}
