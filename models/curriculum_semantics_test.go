// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "testing"

func TestSameCurriculumDefinition(t *testing.T) {
	base := CurriculumConcept{Label: "Original", Description: "Meaning", Level: CurriculumLevelFoundation,
		Outcomes: []CurriculumOutcome{{ID: "o1", Statement: "Explain"}, {ID: "o2", Statement: "Apply"}},
		Criteria: []CurriculumCriterion{{ID: "c1", Description: "Correct"}, {ID: "c2", Description: "Justified"}},
	}
	for _, tc := range []struct {
		name   string
		mutate func(*CurriculumConcept)
		same   bool
	}{
		{"label", func(c *CurriculumConcept) { c.Label = "Display only" }, true},
		{"order", func(c *CurriculumConcept) {
			c.Outcomes[0], c.Outcomes[1] = c.Outcomes[1], c.Outcomes[0]
			c.Criteria[0], c.Criteria[1] = c.Criteria[1], c.Criteria[0]
		}, true},
		{"description", func(c *CurriculumConcept) { c.Description = "Different meaning" }, false},
		{"level", func(c *CurriculumConcept) { c.Level = CurriculumLevelAdvanced }, false},
		{"outcome", func(c *CurriculumConcept) { c.Outcomes[0].Statement = "Different capability" }, false},
		{"observable", func(c *CurriculumConcept) { c.Outcomes[0].Observable = "Independent answer" }, false},
		{"criterion", func(c *CurriculumConcept) { c.Criteria[0].Description = "Different standard" }, false},
		{"evidence", func(c *CurriculumConcept) { c.Criteria[0].Evidence = "Without help" }, false},
		{"outcome id", func(c *CurriculumConcept) { c.Outcomes[0].ID = "new" }, false},
		{"removal", func(c *CurriculumConcept) { c.Criteria = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyConcept := CloneCurriculumSnapshot(&CurriculumSnapshot{Concepts: []CurriculumConcept{base}}).Concepts[0]
			tc.mutate(&copyConcept)
			if got := SameCurriculumDefinition(base, copyConcept); got != tc.same {
				t.Fatalf("same=%t want %t", got, tc.same)
			}
		})
	}
}
