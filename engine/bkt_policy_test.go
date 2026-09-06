// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"testing"

	"tutor-mcp/models"
)

func TestBKTUpdateModeForActivity(t *testing.T) {
	for _, tc := range []struct {
		activity models.ActivityType
		want     models.BKTUpdateMode
	}{
		{models.ActivityDiagnosticAssessment, models.BKTObservationOnly},
		{models.ActivityMasteryChallenge, models.BKTObservationOnly},
		{models.ActivityTransferProbe, models.BKTObservationOnly},
		{models.ActivityNewConcept, models.BKTLearningOpportunity},
		{models.ActivityPractice, models.BKTLearningOpportunity},
		{models.ActivityRecall, models.BKTLearningOpportunity},
		{models.ActivityDebuggingCase, models.BKTLearningOpportunity},
		{models.ActivityDebugMisconception, models.BKTLearningOpportunity},
		{models.ActivityFeynmanPrompt, models.BKTLearningOpportunity},
		{models.ActivityRest, models.BKTObservationOnly},
		{models.ActivityCloseSession, models.BKTObservationOnly},
		{models.ActivitySetupDomain, models.BKTObservationOnly},
		{"unknown", models.BKTObservationOnly},
	} {
		t.Run(string(tc.activity), func(t *testing.T) {
			if got := BKTUpdateModeForActivity(tc.activity); got != tc.want {
				t.Fatalf("mode = %s, want %s", got, tc.want)
			}
		})
	}
}
