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

// MasteryStage is a learner-facing evidence ladder. Each stage implies the
// previous one; a probability estimate alone is never labelled demonstrated.
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
// The boolean fields are cumulative, making the hierarchy explicit to clients.
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
	if cs == nil {
		return status
	}
	status.MasteryEstimate = cs.PMastery
	if cs.CardState != "new" {
		status.Stage = MasteryStageDeveloping
		status.RetentionEstimate = algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
	}

	evidence := MasteryEvidenceQuality(BuildEvidenceProfile(learnerID, concept, interactions, now))
	uncertainty := ComputeMasteryUncertainty(cs, interactions, MasteryEvidenceProfile{Now: now})
	verifiedTransfers := trustedTransferRecords(concept, transfers, assessments)
	transfer := BuildTrustedTransferProfileAt(concept, verifiedTransfers, now)
	status.EvidenceQuality = evidence.Quality
	status.Confidence = uncertainty.ConfidenceLabel
	status.TransferReadiness = transfer.ReadinessLabel
	linkedRecall := hasAssessmentLinkedDelayedSuccessfulRecall(
		learnerID, concept, interactions, assessments, minimumDelayedRecallGap,
	)
	if linkedRecall {
		status.RetentionEvidence = RetentionEvidenceAssessmentLinked
	} else if hasDelayedRetrievalObservation(learnerID, concept, interactions, minimumDelayedRecallGap) {
		status.RetentionEvidence = RetentionEvidenceUnverifiedOnly
	}

	status.Estimated = cs.PMastery >= algorithms.MasteryBKT()
	if !status.Estimated {
		return status
	}
	status.Stage = MasteryStageEstimated

	status.Retained = cs.CardState != "new" &&
		status.RetentionEstimate >= algorithms.RetentionRecallRoutingThreshold && linkedRecall
	if !status.Retained {
		return status
	}
	status.Stage = MasteryStageRetained

	status.Demonstrated = hasTrustedPassedDemonstration(concept, assessments)
	if !status.Demonstrated {
		return status
	}
	status.Stage = MasteryStageDemonstrated

	status.Transferred = transfer.ReadinessLabel == TransferReadinessReady || transfer.ReadinessLabel == TransferReadinessRobust
	if status.Transferred {
		status.Stage = MasteryStageTransferred
	}
	return status
}

func delayedRetrievalCandidates(learnerID, concept string, interactions []*models.Interaction, minimumGap time.Duration) []*models.Interaction {
	if minimumGap <= 0 {
		minimumGap = minimumDelayedRecallGap
	}
	var relevant []*models.Interaction
	for _, interaction := range interactions {
		if interaction == nil || interaction.LearnerID != learnerID || interaction.Concept != concept || interaction.CreatedAt.IsZero() {
			continue
		}
		relevant = append(relevant, interaction)
	}
	if len(relevant) < 2 {
		return nil
	}
	sort.Slice(relevant, func(i, j int) bool { return relevant[i].CreatedAt.Before(relevant[j].CreatedAt) })
	oldestEvidence := relevant[0].CreatedAt
	var candidates []*models.Interaction
	for _, interaction := range relevant[1:] {
		if interaction.CreatedAt.Sub(oldestEvidence) < minimumGap {
			continue
		}
		if interaction.Success && isRetrievalActivity(interaction.ActivityType) {
			candidates = append(candidates, interaction)
		}
	}
	return candidates
}

func hasDelayedRetrievalObservation(learnerID, concept string, interactions []*models.Interaction, minimumGap time.Duration) bool {
	return len(delayedRetrievalCandidates(learnerID, concept, interactions, minimumGap)) > 0
}

func hasAssessmentLinkedDelayedSuccessfulRecall(learnerID, concept string, interactions []*models.Interaction, assessments []*models.AssessmentAttempt, minimumGap time.Duration) bool {
	attempts := make(map[string]*models.AssessmentAttempt, len(assessments))
	for _, attempt := range assessments {
		if attempt != nil && attempt.ID != "" {
			attempts[attempt.ID] = attempt
		}
	}
	for _, interaction := range delayedRetrievalCandidates(learnerID, concept, interactions, minimumGap) {
		attempt := attempts[interaction.AssessmentAttemptID]
		if assessmentMatchesEvaluatedInteraction(attempt, interaction, learnerID, concept) && attempt.Passed {
			return true
		}
	}
	return false
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
		attempt.Status != models.AssessmentAttemptEvaluated || !attempt.TrustedEvaluation ||
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
		attempt.Status == models.AssessmentAttemptEvaluated &&
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
