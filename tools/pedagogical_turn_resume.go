// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"crypto/sha256"
	"fmt"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// The outer MCP writer transaction owns selection, decision and idempotence.
// A validation or packet-budget failure therefore rolls all three back.
func selectPedagogicalTurnTraining(ctx context.Context, deps *Deps, principal models.Principal, in PreparePedagogicalTurnParams, current *models.ConversationContext) (*models.ConversationContext, models.ResumeTrainingChoices, string, error) {
	resolver, ok := deps.Store.(storeport.ResumeTrainingStore)
	if !ok {
		return nil, models.ResumeTrainingChoices{}, "", fmt.Errorf("resume training resolver unavailable")
	}
	choices, err := resolver.ResolveResumeTrainings(ctx, principal, in.TrainingID, in.TrainingName)
	if err != nil {
		return nil, choices, "", err
	}
	if in.TrainingID == "" && in.TrainingName == "" {
		return current, choices, "missing", nil
	}
	if len(choices.Items) == 0 {
		return current, choices, "not_found", nil
	}
	if len(choices.Items) != 1 || choices.HasMore {
		return current, choices, "ambiguous", nil
	}
	chosen := choices.Items[0]
	if chosen.ID == current.TrainingID {
		return current, models.ResumeTrainingChoices{}, "", nil
	}
	// The key namespace is stable and scoped independently of a user's other
	// training mutations, including identical turn keys from other memberships.
	key := fmt.Sprintf("turn-select:%x", sha256.Sum256([]byte(principal.TenantID+"\x00"+principal.MembershipID+"\x00"+in.ConversationID+"\x00"+in.IdempotencyKey)))
	training := deps.Store.(storeport.TrainingStore)
	_, err = training.TrainingAction(ctx, principal, "select_conversation_training", models.TrainingRequest{ID: chosen.ID, ConversationID: in.ConversationID, ExpectedSelection: &current.SelectionVersion, Key: key})
	if err != nil {
		return nil, choices, "", err
	}
	// Read live selection rather than accepting a cached internal receipt as a
	// capability. The outer durable turn receipt handles complete exact retries.
	raw, err := training.TrainingAction(ctx, principal, "get_conversation_context", models.TrainingRequest{ConversationID: in.ConversationID})
	if err != nil {
		return nil, choices, "", err
	}
	selected, ok := raw.(*models.ConversationContext)
	if !ok || selected == nil || selected.TrainingID != chosen.ID || selected.DomainID != chosen.DomainID || selected.Revision != chosen.CurriculumRevision || selected.SelectionVersion != current.SelectionVersion+1 {
		return nil, choices, "", storeport.ErrCurriculumVersionConflict
	}
	return selected, models.ResumeTrainingChoices{}, "", nil
}

// Presentation describes the first display, not evidence or completion. Even
// NEW_CONCEPT can later have a separately frozen understanding assessment.
func pedagogicalTurnPresentation(activity models.ActivityType) (string, string) {
	switch activity {
	case models.ActivityNewConcept:
		return "instruction", "introduce_the_selected_concept_without_awarding_progress"
	case models.ActivityRecall, models.ActivityDiagnosticAssessment, models.ActivityMasteryChallenge, models.ActivityDebuggingCase, models.ActivityPractice, models.ActivityDebugMisconception, models.ActivityFeynmanPrompt, models.ActivityTransferProbe:
		return "assessment", "freeze_task_and_rubric_before_display_collect_actual_learner_response"
	case models.ActivityRest:
		return "guidance", "respect_the_engine_pause_without_assessment_or_progress"
	case models.ActivityCloseSession:
		return "guidance", "offer_session_close_guidance_without_claiming_a_close_was_recorded"
	case models.ActivitySetupDomain:
		return "guidance", "request_setup_without_creating_training_or_progress"
	default:
		return "guidance", "unrecognized_activity_requires_clarification_without_assessment_or_progress"
	}
}
