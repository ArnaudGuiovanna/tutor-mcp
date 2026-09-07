package engine

import (
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestLearningEventProtocolUsesExposureAndIndependentGrade(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, gap := range []time.Duration{time.Minute, 24 * time.Hour, 7 * 24 * time.Hour} {
		cs, recall, attempt := retentionFixture(now)
		attempt.EventProtocol = models.LearningEventProtocol
		exposure := now.Add(-gap)
		attempt.PriorExposureAt = &exposure
		recall.Success = false // preserved original host disagreement
		status := AssessMasteryStatus("L1", "fractions", cs, []*models.Interaction{recall}, nil, []*models.AssessmentAttempt{attempt}, now)
		if status.Retained != (gap >= 24*time.Hour) {
			t.Fatalf("gap=%v retained=%v", gap, status.Retained)
		}
		if recall.Success {
			t.Fatal("mutated the original host observation")
		}
	}
}

func TestLearningEventProtocolGradingIsNotImplicitFeedback(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	cs, recall, attempt := retentionFixture(now)
	priorResponse := now.Add(-48 * time.Hour)
	priorGrade := now.Add(-time.Minute)
	prior := masteryStatusInteraction("L1", "fractions", models.ActivityRecall, true, priorGrade)
	prior.AssessmentAttemptID = "prior"
	prior.HintsRequested = 1
	priorAttempt := masteryStatusAssessment("prior", "fractions", models.ActivityRecall, false, priorGrade)
	priorAttempt.SubmittedAt = &priorResponse
	priorAttempt.EventProtocol = models.LearningEventProtocol
	attempt.EventProtocol = models.LearningEventProtocol
	attempt.PriorExposureAt = &priorResponse
	status := AssessMasteryStatus("L1", "fractions", cs, []*models.Interaction{prior, recall}, nil, []*models.AssessmentAttempt{priorAttempt, attempt}, now)
	if !status.Retained {
		t.Fatal("grading without delivered feedback reset the delay")
	}
}
