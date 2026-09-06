// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package engine

import "tutor-mcp/models"

// BKTUpdateModeForActivity is a versioned runtime policy, not a claim that
// testing can never cause learning. Cold/summative probes measure the response;
// practice retains the modeled learning opportunity of traditional BKT. Merely
// linking a rubric or declaring success never changes this purpose.
func BKTUpdateModeForActivity(activity models.ActivityType) models.BKTUpdateMode {
	switch activity {
	case models.ActivityNewConcept, models.ActivityPractice, models.ActivityRecall,
		models.ActivityDebuggingCase, models.ActivityDebugMisconception, models.ActivityFeynmanPrompt:
		return models.BKTLearningOpportunity
	default:
		// Includes DIAGNOSTIC_ASSESSMENT, MASTERY_CHALLENGE and TRANSFER_PROBE.
		// Unknown/non-cognitive actions never imply a learning transition.
		return models.BKTObservationOnly
	}
}
