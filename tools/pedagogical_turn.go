// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const (
	pedagogicalTurnBudgetBytes      = 96 * 1024
	pedagogicalTurnConceptLimit     = 32
	pedagogicalTurnObservationLimit = 8
)

type PreparePedagogicalTurnParams struct {
	IdempotentMutationParams
	ConversationID    string `json:"conversation_id,omitempty" jsonschema:"native conversation identifier; the bound MCP session takes precedence"`
	Mode              string `json:"mode,omitempty" jsonschema:"context (default) reads without opening a learning session; activity freezes the canonical engine decision and requires an idempotency_key"`
	ExpectedSelection *int   `json:"expected_selection,omitempty" jsonschema:"optional confirmed conversation selection_version; reject stale selection"`
	ExpectedRevision  string `json:"expected_revision,omitempty" jsonschema:"optional confirmed curriculum revision; reject stale revision"`
	Intent            string `json:"intent,omitempty" jsonschema:"auto or review; used only in activity mode"`
	AttemptID         string `json:"attempt_id,omitempty" jsonschema:"optional assessment attempt belonging to this learner and selected domain; returned unchanged"`
	TrainingID        string `json:"training_id,omitempty" jsonschema:"activity only: explicitly selected existing training; mutually exclusive with training_name. Expected selection and revision refer to the conversation BEFORE this selection"`
	TrainingName      string `json:"training_name,omitempty" jsonschema:"activity only: exact name of an existing active learning training, case insensitive; ambiguous or missing names return needs_selection without an activity"`
}

func normalizePedagogicalTurnMode(mode string) (string, error) {
	switch mode {
	case "", "context":
		return "context", nil
	case "activity":
		return mode, nil
	default:
		return "", storeport.Invalid("mode must be context or activity")
	}
}

// The authorization, transaction and replay middlewares use the same parser as
// the handler. Invalid JSON/modes must fail closed, never select a write grant.
func pedagogicalTurnModeFromRequest(req *mcp.CallToolRequest) (string, error) {
	if req == nil || req.Params == nil {
		return "", errors.New("missing tool arguments")
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return "", errors.New("invalid tool arguments")
		}
	}
	return normalizePedagogicalTurnMode(in.Mode)
}

func registerPreparePedagogicalTurn(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "prepare_pedagogical_turn",
		Description: "Prepare one versioned, authorized teaching context for this conversation. context reads without opening a session or changing progression. activity optionally selects training_id or resolves an exact training_name before freezing the canonical engine decision, in one transaction with a stable idempotency_key. Ambiguous names return needs_selection and bounded authorized choices; never guess or enroll implicitly. Expected selection/revision refer to the previous conversation selection. The packet includes training metadata, curriculum, observations, session and full pedagogical_contract, replacing cascading context/activity/training reads. presentation distinguishes instruction, assessment and guidance without changing the canonical decision. Freeze an assessment before display, then submit actual answers, evaluate and record evidence; acknowledge instruction/feedback only after display. No narrative consolidation is required before responding. Later writes revalidate ownership and versions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PreparePedagogicalTurnParams) (*mcp.CallToolResult, any, error) {
		out, err := preparePedagogicalTurn(ctx, deps, in)
		if errors.Is(err, storeport.ErrCurriculumVersionConflict) {
			r, _ := errorResult("pedagogical turn unavailable: the conversation selection or curriculum revision changed; prepare a context turn again and pass its current selection_version and revision")
			return r, nil, nil
		}
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "pedagogical turn unavailable", err)
			return r, nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})
}

func preparePedagogicalTurn(ctx context.Context, deps *Deps, in PreparePedagogicalTurnParams) (*models.PedagogicalTurn, error) {
	mode, err := normalizePedagogicalTurnMode(in.Mode)
	if err != nil {
		return nil, err
	}
	principal, ok := auth.GetPrincipal(ctx)
	if !ok || principal.LearnerID == "" {
		return nil, storeport.Invalid("authenticated learner principal required")
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	scopes := strings.Join(principal.Scopes, " ")
	canRead := models.OAuthScopeAllows(scopes, models.OAuthScopeLearnerRead)
	canWrite := models.OAuthScopeAllows(scopes, models.OAuthScopeLearnerWrite)
	if !canRead || (mode == "activity" && !canWrite) {
		return nil, storeport.Invalid("learner permission required for requested mode")
	}
	if mode == "activity" {
		if policy, ok := ctx.Value(toolPolicyKey{}).(*toolPolicy); ok && (policy.denies("get_next_activity") || ((in.TrainingID != "" || in.TrainingName != "") && policy.denies("select_conversation_training"))) {
			return nil, storeport.Invalid("a required activity or training selection tool is disabled in your Tutor permission settings")
		}
		key := in.IdempotencyKey
		if key == "" {
			key = idempotencyKeyFromContext(ctx)
		}
		if !validIdempotencyKey.MatchString(key) {
			return nil, storeport.Invalid("activity mode requires a valid idempotency_key")
		}
		in.IdempotencyKey = key
	}
	if (in.TrainingID != "" || in.TrainingName != "") && mode != "activity" {
		return nil, storeport.Invalid("training selection requires activity mode")
	}
	if in.TrainingID != "" && in.TrainingName != "" {
		return nil, storeport.Invalid("training_id and training_name are mutually exclusive")
	}
	if in.TrainingName != "" && strings.TrimSpace(in.TrainingName) == "" {
		return nil, storeport.Invalid("training_name must not be blank")
	}
	if err := validateString("training_name", in.TrainingName, 200); err != nil {
		return nil, invalidInput(err)
	}
	if bound := storeport.ConversationID(ctx); bound != "" {
		if in.ConversationID != "" && in.ConversationID != bound {
			return nil, storeport.Invalid("conversation does not match this MCP session")
		}
		in.ConversationID = bound
	}
	if in.ConversationID == "" {
		return nil, storeport.Invalid("conversation_id is required")
	}
	for name, value := range map[string]string{"conversation_id": in.ConversationID, "attempt_id": in.AttemptID, "expected_revision": in.ExpectedRevision, "training_id": in.TrainingID} {
		if err := validateString(name, value, maxShortLabelLen); err != nil {
			return nil, invalidInput(err)
		}
	}
	ctx = storeport.WithConversation(ctx, in.ConversationID)
	training, ok := deps.Store.(storeport.TrainingStore)
	if !ok {
		return nil, errors.New("conversation context unavailable")
	}
	selected, err := training.TrainingAction(ctx, principal, "get_conversation_context", models.TrainingRequest{ConversationID: in.ConversationID})
	if err != nil {
		return nil, err
	}
	conversation, ok := selected.(*models.ConversationContext)
	if !ok || conversation == nil {
		return nil, errors.New("invalid conversation context")
	}
	if (in.ExpectedSelection != nil && *in.ExpectedSelection != conversation.SelectionVersion) || (in.ExpectedRevision != "" && in.ExpectedRevision != conversation.Revision) {
		return nil, storeport.ErrCurriculumVersionConflict
	}
	var choices models.ResumeTrainingChoices
	selectionReason := ""
	if mode == "activity" && (in.TrainingID != "" || in.TrainingName != "" || conversation.DomainID == "") {
		conversation, choices, selectionReason, err = selectPedagogicalTurnTraining(ctx, deps, principal, in, conversation)
		if err != nil {
			return nil, err
		}
	}
	learner, err := deps.Store.GetLearnerByID(ctx, principal.LearnerID)
	if err != nil {
		return nil, err
	}
	out := &models.PedagogicalTurn{
		SchemaVersion: models.PedagogicalTurnSchemaVersion, Mode: mode, GeneratedAt: time.Now().UTC(), Conversation: conversation, Training: conversation.Training,
		Learner:      models.PedagogicalTurnLearner{ID: principal.LearnerID, Objective: learner.Objective},
		Permissions:  models.PedagogicalTurnPermissions{Read: canRead, PrepareActivity: canWrite},
		Difficulties: []models.PedagogicalTurnDifficulty{}, RecentObservations: []models.PedagogicalTurnObservation{},
		NeedsSelection:  conversation.DomainID == "" || selectionReason != "",
		SelectionReason: selectionReason, TrainingChoices: choices.Items, TrainingChoicesHasMore: choices.HasMore,
		Provenance: models.PedagogicalTurnProvenance{Source: "tutor-mcp", PolicyVersion: models.PedagogicalPolicyVersion, SelectionVersion: conversation.SelectionVersion, Revision: conversation.Revision, ModelUpdated: false, ContextBudgetBytes: pedagogicalTurnBudgetBytes, Reductions: []models.PedagogicalTurnReduction{}},
	}
	if learner.ProfileJSON != "" {
		if err := json.Unmarshal([]byte(learner.ProfileJSON), &out.Learner.Profile); err != nil {
			return nil, fmt.Errorf("invalid stored learner profile: %w", err)
		}
	}
	availability, err := deps.Store.GetAvailability(ctx, principal.LearnerID)
	if err != nil {
		return nil, err
	}
	out.Learner.Accessibility, err = availability.Accessibility()
	if err != nil {
		return nil, err
	}
	if out.NeedsSelection {
		if in.AttemptID != "" {
			return nil, storeport.Invalid("select a training before preparing an activity or assessment context")
		}
		out.Training = nil
		return boundedPedagogicalTurn(out)
	}
	domain, err := deps.Store.GetLearnerDomainByID(ctx, principal.LearnerID, conversation.DomainID)
	if err != nil {
		return nil, err
	}
	if domain.Archived || domain.DeletedAt != nil {
		return nil, storeport.Invalid("selected domain is not active")
	}
	snapshot, err := deps.Store.GetCurriculumSnapshot(ctx, principal.LearnerID, domain.ID, domain.GraphVersion)
	if err != nil {
		return nil, fmt.Errorf("selected curriculum snapshot unavailable: %w", err)
	}
	// Load and validate all assessment evidence before any activity mutation.
	focus, focusSource := "", ""
	if in.AttemptID != "" {
		attempt, err := deps.Store.GetAssessmentAttempt(ctx, principal.LearnerID, in.AttemptID)
		if err != nil {
			return nil, err
		}
		if attempt.DomainID != domain.ID || attempt.CurriculumInvalidatedVersion != 0 || (attempt.CurriculumVersion != 0 && attempt.CurriculumVersion != snapshot.Version) {
			return nil, storeport.Invalid("assessment does not match the selected curriculum")
		}
		if !json.Valid([]byte(attempt.RubricJSON)) {
			return nil, errors.New("invalid stored assessment rubric")
		}
		out.Attempt = &models.PedagogicalTurnAttempt{ID: attempt.ID, DomainID: attempt.DomainID, Concept: attempt.ConceptID, SessionID: attempt.SessionID, DecisionID: attempt.DecisionID, CurriculumVersion: attempt.CurriculumVersion, Status: attempt.Status, TaskText: attempt.TaskText, TaskHash: attempt.TaskContentHash, ActivityID: attempt.ActivityID, ActivityVersion: attempt.ActivityVersion, ActivityType: attempt.ActivityType, Observable: attempt.Observable, PassingScore: attempt.PassingScore, EventProtocol: attempt.EventProtocol, Competency: json.RawMessage(attempt.CurriculumConceptJSON), OutcomeIDs: json.RawMessage(attempt.OutcomeIDsJSON), Rubric: json.RawMessage(attempt.RubricJSON), ResponseText: attempt.ResponseText, ResponseHash: attempt.ResponseContentHash, SubmittedAt: attempt.SubmittedAt}
		focus, focusSource = attempt.ConceptID, "assessment_attempt"
	}
	observations, err := deps.Store.GetRecentInteractionsByDomain(ctx, principal.LearnerID, domain.ID, pedagogicalTurnObservationLimit+1)
	if err != nil {
		return nil, err
	}
	if len(observations) > pedagogicalTurnObservationLimit {
		out.Provenance.Reductions = append(out.Provenance.Reductions, models.PedagogicalTurnReduction{Field: "recent_observations", Reason: "latest eight observations only; available is a lower bound from a bounded query; older durable evidence is retained", SourceVersion: conversation.Revision, Available: len(observations), Returned: pedagogicalTurnObservationLimit})
		observations = observations[:pedagogicalTurnObservationLimit]
	}
	for _, observation := range observations {
		kind := "unverified_observation"
		if observation.AssessmentAttemptID != "" {
			kind = "assessment_linked_observation_not_a_mastery_claim"
		}
		out.RecentObservations = append(out.RecentObservations, models.PedagogicalTurnObservation{ID: observation.ID, Concept: observation.Concept, ActivityType: observation.ActivityType, ObservedSuccess: observation.Success, HintsRequested: observation.HintsRequested, AssessmentAttemptID: observation.AssessmentAttemptID, EvidenceKind: kind, CreatedAt: observation.CreatedAt})
	}
	if mode == "activity" {
		result, _, err := nextActivityResult(ctx, deps, GetNextActivityParams{IdempotentMutationParams: in.IdempotentMutationParams, DomainID: domain.ID, Intent: in.Intent})
		if err != nil {
			return nil, err
		}
		if result == nil || result.IsError {
			// get_next_activity error texts are already written for the caller
			// (domain not found, invalid intent, ...); keep them actionable.
			if detail := resultErrorText(result); detail != "" {
				return nil, storeport.Invalid("canonical activity preparation failed: %s", detail)
			}
			return nil, errors.New("canonical activity preparation failed")
		}
		activity, ok := result.StructuredContent.(map[string]any)
		if !ok {
			return nil, errors.New("invalid canonical activity context")
		}
		out.Activity = activity
		if chosen, ok := activity["activity"].(models.Activity); ok {
			out.Presentation, out.PresentationReason = pedagogicalTurnPresentation(chosen.Type)
			if chosen.Concept != "" {
				focus, focusSource = chosen.Concept, "canonical_engine"
			}
		} else {
			return nil, errors.New("canonical activity type unavailable")
		}
		if degraded, ok := activity["degraded_components"].([]string); ok {
			out.DegradedComponents = append(out.DegradedComponents, degraded...)
		}
	}
	if focus == "" && len(observations) > 0 {
		focus, focusSource = observations[0].Concept, "recent_observation_not_a_routing_decision"
	}
	if focus == "" && len(snapshot.Graph.Concepts) > 0 {
		focus, focusSource = snapshot.Graph.Concepts[0], "curriculum_order_not_a_routing_decision"
	}
	curriculum, reduction, err := pedagogicalTurnCurriculum(snapshot, domain.HighStakes, focus, focusSource)
	if err != nil {
		return nil, err
	}
	out.Curriculum = curriculum
	if reduction != nil {
		out.Provenance.Reductions = append(out.Provenance.Reductions, *reduction)
	}
	concepts := make([]string, 0, len(curriculum.Concepts))
	for _, concept := range curriculum.Concepts {
		concepts = append(concepts, concept.Key)
	}
	misconceptions, err := deps.Store.GetActiveMisconceptionsBatchInDomain(ctx, principal.LearnerID, domain.ID, concepts)
	if err != nil {
		return nil, err
	}
	for _, concept := range concepts {
		if misconceptions[concept] {
			out.Difficulties = append(out.Difficulties, models.PedagogicalTurnDifficulty{Concept: concept, ActiveMisconception: true})
		}
	}
	session, err := deps.Store.GetActiveLearningSession(ctx, principal.LearnerID)
	if err != nil && !errors.Is(err, storeport.ErrNotFound) {
		return nil, err
	}
	if session != nil && session.DomainID == domain.ID {
		out.Session = session
	}
	if mode == "context" && memory.Enabled() {
		ec, memoryErr := memory.LoadContextForDomainContext(ctx, principal.LearnerID, domain.ID, focus, &memory.OLMView{FocusConcept: focus}, nil)
		if memoryErr != nil {
			out.DegradedComponents = append(out.DegradedComponents, "narrative_context")
		} else {
			out.Memory, err = json.Marshal(ec)
			if err != nil {
				return nil, err
			}
		}
	}
	if memory.Enabled() {
		out.Provenance.Reductions = append(out.Provenance.Reductions, models.PedagogicalTurnReduction{Field: "memory", Reason: "bounded narrative projection: 40 KiB, at most three domain sessions and two archives; per-field limits and explicit truncation markers; complete durable narratives remain available", SourceVersion: "episodic-context/v1"})
	}
	return boundedPedagogicalTurn(out)
}

func boundedPedagogicalTurn(out *models.PedagogicalTurn) (*models.PedagogicalTurn, error) {
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(encoded) > pedagogicalTurnBudgetBytes {
		return nil, storeport.Invalid("pedagogical context exceeds %d bytes; full assessment and pedagogical constraints were preserved, request a narrower context", pedagogicalTurnBudgetBytes)
	}
	return out, nil
}

// Keep complete competencies and the transitive prerequisite closure for every
// included concept. Only unrelated concepts may be omitted, with provenance.
func pedagogicalTurnCurriculum(snapshot *models.CurriculumSnapshot, highStakes bool, focus, source string) (*models.PedagogicalTurnCurriculum, *models.PedagogicalTurnReduction, error) {
	byKey := make(map[string]models.CurriculumConcept, len(snapshot.Concepts))
	for _, concept := range snapshot.Concepts {
		byKey[concept.Key] = concept
	}
	chosen := map[string]bool{}
	var include func(string, map[string]bool) error
	include = func(key string, into map[string]bool) error {
		if into[key] {
			return nil
		}
		if _, ok := byKey[key]; !ok {
			return fmt.Errorf("curriculum metadata missing for %s", key)
		}
		into[key] = true
		for _, pre := range snapshot.Graph.Prerequisites[key] {
			if err := include(pre, into); err != nil {
				return err
			}
		}
		return nil
	}
	if focus != "" {
		if err := include(focus, chosen); err != nil {
			return nil, nil, err
		}
	}
	if len(chosen) > pedagogicalTurnConceptLimit {
		return nil, nil, storeport.Invalid("focused prerequisite closure exceeds the context concept limit; no prerequisite was silently discarded")
	}
	for _, key := range snapshot.Graph.Concepts {
		candidate := make(map[string]bool, len(chosen)+1)
		for k, v := range chosen {
			candidate[k] = v
		}
		if err := include(key, candidate); err != nil {
			return nil, nil, err
		}
		if len(candidate) <= pedagogicalTurnConceptLimit {
			chosen = candidate
		}
	}
	out := &models.PedagogicalTurnCurriculum{DomainID: snapshot.DomainID, Version: snapshot.Version, FocusConcept: focus, FocusSource: source, HighStakes: highStakes, Concepts: []models.CurriculumConcept{}, Prerequisites: map[string][]string{}}
	for _, concept := range snapshot.Concepts {
		if chosen[concept.Key] {
			out.Concepts = append(out.Concepts, concept)
			out.Prerequisites[concept.Key] = append([]string{}, snapshot.Graph.Prerequisites[concept.Key]...)
		}
	}
	var reduction *models.PedagogicalTurnReduction
	if len(out.Concepts) < len(snapshot.Concepts) {
		reduction = &models.PedagogicalTurnReduction{Field: "curriculum.concepts", Reason: "bounded focused curriculum projection; complete included competencies and prerequisite closures retained; full version available via get_curriculum_snapshot", SourceVersion: strconv.Itoa(snapshot.Version), Available: len(snapshot.Concepts), Returned: len(out.Concepts)}
	}
	return out, reduction, nil
}
