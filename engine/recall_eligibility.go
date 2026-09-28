// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// RecallEligible reports whether a concept can be the target of a recall
// need or a FORGETTING alert. Only a memory card that reached the FSRS
// review state at least once has something to recall: a card left in the
// learning state by a failed first exposure (a cold diagnostic, a first
// practice that failed) describes a concept the learner never acquired.
// Routing such a concept through spaced recall, ahead of the prerequisite
// gate, asked learners to "remember" material they were never taught.
func RecallEligible(cs *models.ConceptState) bool {
	if cs == nil || cs.LastReview == nil || cs.LastReview.IsZero() {
		return false
	}
	switch algorithms.CardState(cs.CardState) {
	case algorithms.Review, algorithms.Relearning:
		return true
	default:
		return false
	}
}
