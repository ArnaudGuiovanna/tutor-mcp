// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

// Mastery thresholds are exposed via accessors in thresholds.go.
// MasteryKST() returns the prerequisite-unlock threshold (0.70 legacy,
// 0.85 unified under REGULATION_THRESHOLD=on).

type KSTGraph struct {
	Concepts      []string            `json:"concepts"`
	Prerequisites map[string][]string `json:"prerequisites"`
}

func ComputeFrontier(graph KSTGraph, mastery map[string]float64) []string {
	threshold := MasteryKST()
	var frontier []string
	for _, concept := range graph.Concepts {
		if mastery[concept] >= threshold {
			continue
		}
		prereqs := graph.Prerequisites[concept]
		allMet := true
		for _, prereq := range prereqs {
			if mastery[prereq] < threshold {
				allMet = false
				break
			}
		}
		if allMet {
			frontier = append(frontier, concept)
		}
	}
	return frontier
}

func ConceptStatus(graph KSTGraph, mastery map[string]float64, concept string) string {
	threshold := MasteryKST()
	if mastery[concept] >= threshold {
		return "done"
	}
	prereqs := graph.Prerequisites[concept]
	for _, prereq := range prereqs {
		if mastery[prereq] < threshold {
			return "locked"
		}
	}
	return "current"
}
