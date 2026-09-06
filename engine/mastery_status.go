// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"sort"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

const minimumDelayedRecallGap = 24 * time.Hour

// MasteryStage is a compact learner-facing summary of the strongest supported
// evidence axis. It does not imply that the other axes have been observed.
type MasteryStage string

// RetentionEvidenceTrust makes the distinction between routing observations
// and protocol-backed retention evidence explicit in learner-facing state.
// "assessment_linked" does not imply an independent/trusted evaluator; that
// stronger boundary is required separately for Demonstrated.
type RetentionEvidenceTrust string

const (
	MasteryStageNotStarted   MasteryStage = "not_started"
	MasteryStageDeveloping   MasteryStage = "developing"
	MasteryStageEstimated    MasteryStage = "estimated"
	MasteryStageRetained     MasteryStage = "retained"
	MasteryStageDemonstrated MasteryStage = "demonstrated"
	MasteryStageTransferred  MasteryStage = "transferred"
)

const (
	RetentionEvidenceNone             RetentionEvidenceTrust = "none"
	RetentionEvidenceUnverifiedOnly   RetentionEvidenceTrust = "unverified_observations_only"
	RetentionEvidenceAssessmentLinked RetentionEvidenceTrust = "assessment_linked"
)

// MasteryStatus unifies the semantics consumed by check_mastery and the OLM.
// Estimated, Retained and Demonstrated are independent: for example, an expert
// can demonstrate a capability before a delayed recall or a high model estimate
// exists. Transferred additionally requires demonstrated assessment evidence.
type MasteryStatus struct {
	Stage             MasteryStage           `json:"stage"`
	Estimated         bool                   `json:"estimated"`
	Retained          bool                   `json:"retained"`
	Demonstrated      bool                   `json:"demonstrated"`
	Transferred       bool                   `json:"transferred"`
	MasteryEstimate   float64                `json:"mastery_estimate"`
	RetentionEstimate float64                `json:"retention_estimate"`
	RetentionEvidence RetentionEvidenceTrust `json:"retention_evidence"`
	EvidenceQuality   EvidenceQuality        `json:"evidence_quality"`
	Confidence        MasteryConfidenceLabel `json:"confidence"`
	TransferReadiness TransferReadinessLabel `json:"transfer_readiness"`
}

// AssessMasteryStatus derives a conservative, shared mastery state from the
// persisted model and evidence. Retained requires an observed successful
// retrieval after a meaningful delay; Demonstrated requires a trusted passed
// assessment whose task and rubric were frozen before the response; and
// Transferred additionally requires broad transfer records linked to trusted
// passed transfer assessments. Model probability and host-authored observations
// alone can never create those claims.
func AssessMasteryStatus(
	learnerID, concept string,
	cs *models.ConceptState,
	interactions []*models.Interaction,
	transfers []*models.TransferRecord,
	assessments []*models.AssessmentAttempt,
	now time.Time,
) MasteryStatus {
	status := MasteryStatus{
		Stage:             MasteryStageNotStarted,
		EvidenceQuality:   EvidenceQualityWeak,
		Confidence:        MasteryConfidenceLow,
		TransferReadiness: TransferReadinessUnobserved,
		RetentionEvidence: RetentionEvidenceNone,
	}
	if cs != nil {
		status.MasteryEstimate = cs.PMastery
		status.Estimated = cs.PMastery >= algorithms.MasteryBKT()
		if cs.CardState != "new" {
			status.Stage = MasteryStageDeveloping
			status.RetentionEstimate = algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
		}
		uncertainty := ComputeMasteryUncertainty(cs, interactions, MasteryEvidenceProfile{Now: now})
		status.Confidence = uncertainty.ConfidenceLabel
	}

	assessments = assessmentEvidenceAt(learnerID, concept, assessments, now)
	evidence := MasteryEvidenceQuality(BuildEvidenceProfile(learnerID, concept, interactions, now))
	verifiedTransfers := trustedTransferRecords(concept, transfers, assessments)
	transfer := BuildTrustedTransferProfileAt(concept, verifiedTransfers, now)
	status.EvidenceQuality = evidence.Quality
	status.TransferReadiness = transfer.ReadinessLabel
	retentionTimeline := assessmentResponseTimeline(learnerID, concept, interactions, assessments, now)
	linkedRecall := hasAssessmentLinkedDelayedSuccessfulRecall(
		learnerID, concept, retentionTimeline, assessments, minimumDelayedRecallGap, now,
	)
	if linkedRecall {
		status.RetentionEvidence = RetentionEvidenceAssessmentLinked
	} else if hasDelayedRetrievalObservation(learnerID, concept, retentionTimeline, minimumDelayedRecallGap, now) {
		status.RetentionEvidence = RetentionEvidenceUnverifiedOnly
	}

	status.Retained = cs != nil && cs.CardState != "new" &&
		status.RetentionEstimate >= algorithms.RetentionRecallRoutingThreshold && linkedRecall
	status.Demonstrated = hasTrustedPassedDemonstration(concept, assessments)
	status.Transferred = status.Demonstrated &&
		(transfer.ReadinessLabel == TransferReadinessReady || transfer.ReadinessLabel == TransferReadinessRobust)

	switch {
	case status.Transferred:
		status.Stage = MasteryStageTransferred
	case status.Demonstrated:
		status.Stage = MasteryStageDemonstrated
	case status.Retained:
		status.Stage = MasteryStageRetained
	case status.Estimated:
		status.Stage = MasteryStageEstimated
	}
	return status
}

// assessmentEvidenceAt excludes other learners and evidence that has not yet
// completed at the decision clock. Trust remains assigned by the persistence
// boundary, including its human-review-only policy for high-stakes domains.
func assessmentEvidenceAt(learnerID, concept string, assessments []*models.AssessmentAttempt, now time.Time) []*models.AssessmentAttempt {
	var relevant []*models.AssessmentAttempt
	for _, attempt := range assessments {
		if attempt == nil || attempt.LearnerID != learnerID || attempt.ConceptID != concept ||
			attempt.Status != models.AssessmentAttemptEvaluated || attempt.CurriculumInvalidatedVersion != 0 ||
			attempt.SubmittedAt == nil || attempt.EvaluatedAt == nil ||
			attempt.SubmittedAt.IsZero() || attempt.EvaluatedAt.IsZero() ||
			attempt.SubmittedAt.After(*attempt.EvaluatedAt) ||
			(!now.IsZero() && attempt.EvaluatedAt.After(now)) {
			continue
		}
		relevant = append(relevant, attempt)
	}
	return relevant
}

// assessmentResponseTimeline dates a linked retrieval to the committed learner
// response, not to grading. Otherwise grading an immediate response a day later
// could manufacture a delayed recall. Copies preserve callers' audit records;
// duplicates of one attempt share its response time and cannot imply a delay.
func assessmentResponseTimeline(learnerID, concept string, interactions []*models.Interaction, assessments []*models.AssessmentAttempt, now time.Time) []*models.Interaction {
	attempts := make(map[string]*models.AssessmentAttempt, len(assessments))
	for _, attempt := range assessments {
		attempts[attempt.ID] = attempt
	}
	timeline := make([]*models.Interaction, 0, len(interactions))
	for _, interaction := range interactions {
		if interaction == nil || (!now.IsZero() && interaction.CreatedAt.After(now)) {
			continue
		}
		if attempt := attempts[interaction.AssessmentAttemptID]; assessmentMatchesEvaluatedInteraction(attempt, interaction, learnerID, concept) {
			observation := *interaction
			observation.CreatedAt = *attempt.SubmittedAt
			timeline = append(timeline, &observation)
			if interaction.CreatedAt.After(observation.CreatedAt) {
				// Keep the later grading/feedback event as an exposure for
				// subsequent tasks, but never as another successful retrieval.
				feedback := *interaction
				feedback.Success = false
				feedback.AssessmentAttemptID = ""
				timeline = append(timeline, &feedback)
			}
		} else {
			timeline = append(timeline, interaction)
		}
	}
	return timeline
}

func delayedRetrievalCandidates(learnerID, concept string, interactions []*models.Interaction, minimumGap time.Duration, now time.Time) []*models.Interaction {
	if minimumGap <= 0 {
		minimumGap = minimumDelayedRecallGap
	}
	var relevant []*models.Interaction
	for _, interaction := range interactions {
		if interaction == nil || interaction.LearnerID != learnerID || interaction.Concept != concept ||
			interaction.CreatedAt.IsZero() || (!now.IsZero() && interaction.CreatedAt.After(now)) ||
			!isCognitiveExposureActivity(interaction.ActivityType) {
			continue
		}
		relevant = append(relevant, interaction)
	}
	if len(relevant) < 2 {
		return nil
	}
	sort.Slice(relevant, func(i, j int) bool { return relevant[i].CreatedAt.Before(relevant[j].CreatedAt) })
	var previousExposure time.Time
	var candidates []*models.Interaction
	for i := 0; i < len(relevant); {
		interaction := relevant[i]
		j := i + 1
		for j < len(relevant) && relevant[j].CreatedAt.Equal(interaction.CreatedAt) {
			j++
		}
		// A same-time exposure has no provable order relative to the response.
		// Reject that group conservatively instead of depending on input order.
		if j == i+1 && !previousExposure.IsZero() &&
			interaction.CreatedAt.Sub(previousExposure) >= minimumGap &&
			interaction.Success && interaction.HintsRequested == 0 && isRetrievalActivity(interaction.ActivityType) {
			candidates = append(candidates, interaction)
		}
		// Every cognitive exposure resets the interval, including failed or
		// assisted work and instruction; only a cold retrieval can pass it.
		previousExposure = interaction.CreatedAt
		i = j
	}
	return candidates
}

func hasDelayedRetrievalObservation(learnerID, concept string, interactions []*models.Interaction, minimumGap time.Duration, now time.Time) bool {
	return len(delayedRetrievalCandidates(learnerID, concept, interactions, minimumGap, now)) > 0
}

func hasAssessmentLinkedDelayedSuccessfulRecall(learnerID, concept string, interactions []*models.Interaction, assessments []*models.AssessmentAttempt, minimumGap time.Duration, now time.Time) bool {
	attempts := make(map[string]*models.AssessmentAttempt, len(assessments))
	for _, attempt := range assessments {
		if attempt != nil && attempt.ID != "" {
			attempts[attempt.ID] = attempt
		}
	}
	for _, interaction := range delayedRetrievalCandidates(learnerID, concept, interactions, minimumGap, now) {
		attempt := attempts[interaction.AssessmentAttemptID]
		if assessmentMatchesEvaluatedInteraction(attempt, interaction, learnerID, concept) && attempt.Passed {
			return true
		}
	}
	return false
}

func isCognitiveExposureActivity(activityType string) bool {
	if isRetrievalActivity(activityType) {
		return true
	}
	switch models.ActivityType(activityType) {
	case models.ActivityNewConcept, models.ActivityPractice, models.ActivityDebugMisconception:
		return true
	default:
		return false
	}
}

func isRetrievalActivity(activityType string) bool {
	switch models.ActivityType(activityType) {
	case models.ActivityRecall,
		models.ActivityDiagnosticAssessment,
		models.ActivityMasteryChallenge,
		models.ActivityDebuggingCase,
		models.ActivityFeynmanPrompt,
		models.ActivityTransferProbe:
		return true
	default:
		return false
	}
}

func hasTrustedPassedDemonstration(concept string, assessments []*models.AssessmentAttempt) bool {
	for _, attempt := range assessments {
		if !isTrustedPassedAssessment(attempt, concept) {
			continue
		}
		switch models.ActivityType(attempt.ActivityType) {
		case models.ActivityDiagnosticAssessment, models.ActivityMasteryChallenge, models.ActivityTransferProbe:
			return true
		}
	}
	return false
}

func trustedTransferRecords(concept string, transfers []*models.TransferRecord, assessments []*models.AssessmentAttempt) []*models.TransferRecord {
	trustedAttempts := make(map[string]*models.AssessmentAttempt, len(assessments))
	for _, attempt := range assessments {
		if isTrustedEvaluatedAssessment(attempt, concept) && models.ActivityType(attempt.ActivityType) == models.ActivityTransferProbe {
			trustedAttempts[attempt.ID] = attempt
		}
	}
	verified := make([]*models.TransferRecord, 0, len(transfers))
	for _, record := range transfers {
		if record == nil || record.ConceptID != concept || record.AssessmentAttemptID == "" {
			continue
		}
		attempt := trustedAttempts[record.AssessmentAttemptID]
		if attempt == nil {
			continue
		}
		// The assessment outcome is authoritative. A contradictory auxiliary
		// transfer score must not turn a trusted failed attempt into a pass.
		// Clone so callers' audit records remain immutable.
		verifiedRecord := *record
		if !attempt.Passed {
			verifiedRecord.Score = 0
		}
		if verifiedRecord.CreatedAt.IsZero() && attempt.EvaluatedAt != nil {
			verifiedRecord.CreatedAt = *attempt.EvaluatedAt
		}
		verified = append(verified, &verifiedRecord)
	}
	return verified
}

func isTrustedPassedAssessment(attempt *models.AssessmentAttempt, concept string) bool {
	return isTrustedEvaluatedAssessment(attempt, concept) && attempt.Passed
}

func isTrustedEvaluatedAssessment(attempt *models.AssessmentAttempt, concept string) bool {
	if attempt == nil || attempt.ConceptID != concept || attempt.ID == "" ||
		attempt.Status != models.AssessmentAttemptEvaluated || attempt.CurriculumInvalidatedVersion != 0 || !attempt.TrustedEvaluation ||
		attempt.SubmittedAt == nil || attempt.EvaluatedAt == nil {
		return false
	}
	switch attempt.EvaluationMethod {
	case models.EvaluationMethodExternal, models.EvaluationMethodHumanReview, models.EvaluationMethodDeterministic:
		return true
	default:
		return false
	}
}

func assessmentMatchesEvaluatedInteraction(attempt *models.AssessmentAttempt, interaction *models.Interaction, learnerID, concept string) bool {
	return attempt != nil && interaction != nil && interaction.AssessmentAttemptID != "" &&
		attempt.ID == interaction.AssessmentAttemptID &&
		attempt.LearnerID == learnerID && attempt.ConceptID == concept &&
		attempt.ActivityType == interaction.ActivityType &&
		attempt.Status == models.AssessmentAttemptEvaluated && attempt.CurriculumInvalidatedVersion == 0 &&
		attempt.SubmittedAt != nil && attempt.EvaluatedAt != nil
}

// BuildTrustedTransferProfileFromEvidence is the shared trusted read model for
// check_mastery, alerts and dashboards. Both trusted successes and trusted
// failures survive the filter; legacy/unlinked observations never influence a
// demonstrated transfer or readiness decision.
func BuildTrustedTransferProfileFromEvidence(concept string, transfers []*models.TransferRecord, assessments []*models.AssessmentAttempt, now time.Time) TransferProfile {
	return BuildTrustedTransferProfileAt(concept, trustedTransferRecords(concept, transfers, assessments), now)
}

// ReadyForMasteryChallenge is intentionally distinct from demonstrated
// mastery: it decides whether enough evidence exists to attempt the challenge.
func ReadyForMasteryChallenge(status MasteryStatus, evidence EvidenceQualityAssessment, uncertainty MasteryUncertainty, transfer TransferProfile) bool {
	return status.Estimated && status.Retained &&
		evidence.Quality != EvidenceQualityWeak &&
		uncertainty.ConfidenceLabel != MasteryConfidenceLow &&
		transfer.ReadinessLabel != TransferReadinessBlocked
}
