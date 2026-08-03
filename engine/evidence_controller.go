// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

type EvidenceControllerInput struct {
	Activity           models.Activity
	ConceptState       *models.ConceptState
	EvidenceQuality    EvidenceQualityAssessment
	MasteryUncertainty MasteryUncertainty
	TransferProfile    TransferProfile
}

type EvidenceControllerDecision struct {
	Activity  models.Activity
	Adjusted  bool
	Rationale string
}

func ApplyEvidenceController(input EvidenceControllerInput) EvidenceControllerDecision {
	activity := input.Activity
	if !evidenceControllerEligible(activity) || input.ConceptState == nil {
		return EvidenceControllerDecision{Activity: activity}
	}
	if input.ConceptState.PMastery < algorithms.MasteryBKT() {
		return EvidenceControllerDecision{Activity: activity}
	}

	switch input.TransferProfile.ReadinessLabel {
	case TransferReadinessBlocked:
		return evidenceAdjusted(activity, models.ActivityFeynmanPrompt, "transfer_repair", 15,
			"evidence controller: transfer blocked -> Feynman explanation before further challenge")
	case TransferReadinessUnobserved:
		return evidenceAdjusted(activity, models.ActivityTransferProbe, "transfer_novel_context", 20,
			"evidence controller: high mastery but transfer unobserved -> transfer probe")
	}

	if activity.Type == models.ActivityMasteryChallenge && input.EvidenceQuality.Quality == EvidenceQualityWeak {
		return evidenceAdjusted(activity, models.ActivityPractice, "varied_evidence_probe", 12,
			"evidence controller: weak mastery evidence -> varied proof before mastery challenge")
	}

	return EvidenceControllerDecision{Activity: activity}
}

func evidenceControllerEligible(activity models.Activity) bool {
	switch activity.Type {
	case models.ActivityCloseSession, models.ActivityRest, models.ActivitySetupDomain, models.ActivityDiagnosticAssessment:
		return false
	default:
		return activity.Concept != ""
	}
}

func evidenceAdjusted(activity models.Activity, t models.ActivityType, format string, minutes int, rationale string) EvidenceControllerDecision {
	activity.Type = t
	activity.Format = format
	activity.EstimatedMinutes = minutes
	activity.Rationale = fmt.Sprintf("%s; %s", activity.Rationale, rationale)
	activity.PromptForLLM = BuildActivityPrompt(activity.Type, activity.Concept, activity.Format)
	return EvidenceControllerDecision{Activity: activity, Adjusted: true, Rationale: rationale}
}

func BuildActivityPrompt(t models.ActivityType, concept, format string) string {
	switch t {
	case models.ActivityDiagnosticAssessment:
		return fmt.Sprintf("Generate one cold diagnostic assessment on %s. Format: %s. Freeze the exact task and domain-appropriate scoring rubric with prepare_assessment_attempt before showing it. Ask for an observable response before giving any explanation, hint, worked example, correction, or feedback. Do not teach the concept before the learner commits an answer. Persist that response with submit_assessment_attempt before scoring it, then record the derived evaluation with record_interaction.", concept, format)
	case models.ActivityTransferProbe:
		return fmt.Sprintf("Generate a novel transfer activity on %s. Format: %s. Choose one canonical transfer dimension (near, far, debugging, teaching, or creative). Freeze the task and rubric with prepare_assessment_attempt before showing it; after the learner responds, call submit_assessment_attempt before evaluating, then pass the attempt_id, criterion scores, transfer dimension and 0..1 transfer score to record_interaction with activity_type=TRANSFER_PROBE.", concept, format)
	case models.ActivityMasteryChallenge:
		return fmt.Sprintf("Generate a domain-appropriate integrated performance task on %s. Format: %s. Require autonomous application and evaluate correctness, completeness, boundary conditions or counterexamples appropriate to the domain, and clarity of reasoning or execution. Freeze the exact task and rubric with prepare_assessment_attempt before showing it; persist the learner response with submit_assessment_attempt before evaluating; then record criterion scores through record_interaction.", concept, format)
	default:
		return fmt.Sprintf("Generate a %s activity on %s. Format: %s. Use the final structured activity.difficulty_target field as the difficulty target.", t, concept, format)
	}
}
