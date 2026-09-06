package engine

import (
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestAssessMasteryStatusDelayedRecallUsesLastExposure(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		exposureGap   time.Duration
		hints         int
		recentType    models.ActivityType
		recentAt      time.Time
		recentOwner   string
		recentConcept string
		wantRetained  bool
	}{
		{name: "cold recall after seven days", exposureGap: 7 * 24 * time.Hour, wantRetained: true},
		{name: "exactly twenty four hours", exposureGap: 24 * time.Hour, wantRetained: true},
		{name: "less than twenty four hours", exposureGap: 24*time.Hour - time.Nanosecond},
		{name: "five hints after seven days", exposureGap: 7 * 24 * time.Hour, hints: 5},
		{name: "reteaching one minute before recall", exposureGap: 7 * 24 * time.Hour, recentType: models.ActivityNewConcept, recentAt: now.Add(-time.Minute)},
		{name: "audit reproduction with reteaching and five hints", exposureGap: 7 * 24 * time.Hour, hints: 5, recentType: models.ActivityNewConcept, recentAt: now.Add(-time.Minute)},
		{name: "recent failed retrieval", exposureGap: 7 * 24 * time.Hour, recentType: models.ActivityRecall, recentAt: now.Add(-time.Minute)},
		{name: "recent misconception correction", exposureGap: 7 * 24 * time.Hour, recentType: models.ActivityDebugMisconception, recentAt: now.Add(-time.Minute)},
		{name: "simultaneous instruction has unknown order", exposureGap: 7 * 24 * time.Hour, recentType: models.ActivityNewConcept, recentAt: now},
		{name: "rest is not a cognitive exposure", exposureGap: 7 * 24 * time.Hour, recentType: models.ActivityRest, recentAt: now.Add(-time.Minute), wantRetained: true},
		{name: "other learner does not reset gap", exposureGap: 7 * 24 * time.Hour, recentType: models.ActivityPractice, recentAt: now.Add(-time.Minute), recentOwner: "L2", wantRetained: true},
		{name: "other concept does not reset gap", exposureGap: 7 * 24 * time.Hour, recentType: models.ActivityPractice, recentAt: now.Add(-time.Minute), recentConcept: "history", wantRetained: true},
		{name: "future instruction does not affect past decision", exposureGap: 7 * 24 * time.Hour, recentType: models.ActivityNewConcept, recentAt: now.Add(time.Hour), wantRetained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, recall, attempt := retentionFixture(now)
			recall.HintsRequested = tc.hints
			interactions := []*models.Interaction{
				masteryStatusInteraction("L1", "fractions", models.ActivityPractice, true, now.Add(-tc.exposureGap)),
				recall,
			}
			if tc.recentType != "" {
				owner, concept := tc.recentOwner, tc.recentConcept
				if owner == "" {
					owner = "L1"
				}
				if concept == "" {
					concept = "fractions"
				}
				interactions = append(interactions, masteryStatusInteraction(owner, concept, tc.recentType, false, tc.recentAt))
			}
			// Both chronological and reverse input order must produce the same
			// decision, including simultaneous events. Inputs remain audit data.
			originalTime := recall.CreatedAt
			for order := 0; order < 2; order++ {
				status := AssessMasteryStatus("L1", "fractions", cs, interactions, nil, []*models.AssessmentAttempt{attempt}, now)
				if status.Retained != tc.wantRetained {
					t.Fatalf("order %d: retained=%v want %v: %+v", order, status.Retained, tc.wantRetained, status)
				}
				if !tc.wantRetained && status.RetentionEvidence != RetentionEvidenceNone {
					t.Fatalf("assisted/immediate retrieval incorrectly labelled as delayed evidence: %+v", status)
				}
				for i, j := 0, len(interactions)-1; i < j; i, j = i+1, j-1 {
					interactions[i], interactions[j] = interactions[j], interactions[i]
				}
			}
			if !recall.CreatedAt.Equal(originalTime) {
				t.Fatal("retention analysis mutated the persisted interaction time")
			}
		})
	}
}

func TestAssessMasteryStatusDelayedGradingDoesNotCreateRetention(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cs, recall, attempt := retentionFixture(now)
	responseAt := now.Add(-48 * time.Hour)
	attempt.SubmittedAt = &responseAt
	interactions := []*models.Interaction{
		masteryStatusInteraction("L1", "fractions", models.ActivityPractice, true, now.Add(-7*24*time.Hour)),
		masteryStatusInteraction("L1", "fractions", models.ActivityNewConcept, true, responseAt.Add(-time.Minute)),
		recall,
	}
	status := AssessMasteryStatus("L1", "fractions", cs, interactions, nil, []*models.AssessmentAttempt{attempt}, now)
	if status.Retained || status.RetentionEvidence != RetentionEvidenceNone {
		t.Fatalf("grading an immediate response 48 hours later manufactured retention: %+v", status)
	}
	if !recall.CreatedAt.Equal(now) || !attempt.SubmittedAt.Equal(responseAt) {
		t.Fatal("analysis mutated the response/evaluation audit timestamps")
	}
}

func TestAssessMasteryStatusPriorGradingRemainsAnExposure(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cs, recall, attempt := retentionFixture(now)
	priorResponse := now.Add(-48 * time.Hour)
	priorGrade := now.Add(-time.Minute)
	prior := masteryStatusInteraction("L1", "fractions", models.ActivityRecall, true, priorGrade)
	prior.HintsRequested = 1
	prior.AssessmentAttemptID = "prior"
	priorAttempt := masteryStatusAssessment("prior", "fractions", models.ActivityRecall, false, priorGrade)
	priorAttempt.SubmittedAt = &priorResponse
	interactions := []*models.Interaction{
		masteryStatusInteraction("L1", "fractions", models.ActivityPractice, true, now.Add(-7*24*time.Hour)),
		prior,
		recall,
	}
	status := AssessMasteryStatus("L1", "fractions", cs, interactions, nil, []*models.AssessmentAttempt{attempt, priorAttempt}, now)
	if status.Retained {
		t.Fatalf("recent feedback must reset the interval even when its response was old: %+v", status)
	}
}

func TestAssessMasteryStatusFutureOrDuplicateAttemptCannotCreateRetention(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []string{"future interaction", "future evaluation", "response after evaluation", "duplicate attempt"} {
		t.Run(tc, func(t *testing.T) {
			cs, recall, attempt := retentionFixture(now)
			interactions := []*models.Interaction{
				masteryStatusInteraction("L1", "fractions", models.ActivityPractice, true, now.Add(-7*24*time.Hour)),
				recall,
			}
			future := now.Add(time.Hour)
			switch tc {
			case "future interaction":
				recall.CreatedAt = future
				attempt.SubmittedAt, attempt.EvaluatedAt = &future, &future
			case "future evaluation":
				attempt.EvaluatedAt = &future
			case "response after evaluation":
				attempt.SubmittedAt = &future
			case "duplicate attempt":
				duplicate := *recall
				duplicate.CreatedAt = now.Add(-48 * time.Hour)
				interactions = append(interactions, &duplicate)
			}
			status := AssessMasteryStatus("L1", "fractions", cs, interactions, nil, []*models.AssessmentAttempt{attempt}, now)
			if status.Retained {
				t.Fatalf("invalid evidence manufactured retention: %+v", status)
			}
		})
	}
}

func retentionFixture(now time.Time) (*models.ConceptState, *models.Interaction, *models.AssessmentAttempt) {
	cs := &models.ConceptState{
		LearnerID: "L1", Concept: "fractions", CardState: "review",
		PMastery: 0.95, Stability: 10, Reps: 6, LastReview: &now,
	}
	recall := masteryStatusInteraction("L1", "fractions", models.ActivityRecall, true, now)
	recall.AssessmentAttemptID = "retention-recall"
	attempt := masteryStatusAssessment("retention-recall", "fractions", models.ActivityRecall, false, now)
	attempt.SubmittedAt = &now
	return cs, recall, attempt
}
