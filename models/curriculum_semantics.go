// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

// SameCurriculumDefinition compares the declared measurement contract, not
// semantic equivalence of generated prose. Cosmetic labels and ordering are
// excluded; any changed description, level, outcome or criterion is conservative
// grounds for a fresh estimate. No LLM equivalence claim can override this rule.
func SameCurriculumDefinition(a, b CurriculumConcept) bool {
	if a.Description != b.Description || a.Level != b.Level || len(a.Outcomes) != len(b.Outcomes) || len(a.Criteria) != len(b.Criteria) {
		return false
	}
	outcomes := make(map[string]CurriculumOutcome, len(a.Outcomes))
	for _, item := range a.Outcomes {
		outcomes[item.ID] = item
	}
	for _, item := range b.Outcomes {
		if previous, ok := outcomes[item.ID]; !ok || previous != item {
			return false
		}
	}
	criteria := make(map[string]CurriculumCriterion, len(a.Criteria))
	for _, item := range a.Criteria {
		criteria[item.ID] = item
	}
	for _, item := range b.Criteria {
		if previous, ok := criteria[item.ID]; !ok || previous != item {
			return false
		}
	}
	return true
}
