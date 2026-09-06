// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

func pedagogicalScope(ctx context.Context, learnerID string) (models.TenantScope, error) {
	principal, ok := auth.GetPrincipal(ctx)
	if !ok || principal.LearnerID != learnerID {
		return models.TenantScope{}, fmt.Errorf("authenticated learner principal required")
	}
	return principal.TenantScope(), nil
}

func curriculumCompetency(snapshot *models.CurriculumSnapshot, key string) *models.CurriculumConcept {
	for i := range snapshot.Concepts {
		c := &snapshot.Concepts[i]
		if c.Key == key && c.Status == models.CurriculumConceptActive {
			return c
		}
	}
	return nil
}

func freezePedagogicalDecision(ctx context.Context, deps *Deps, learnerID, sessionID string, domain *models.Domain,
	contract *models.PedagogicalContract, activity models.Activity, now time.Time, decisionContext any) error {
	if !isCognitiveEvidenceActivity(string(activity.Type)) {
		return nil
	}
	scope, err := pedagogicalScope(ctx, learnerID)
	if err != nil {
		return err
	}
	curriculum, err := deps.Store.EnsureCurriculumBaseline(ctx, learnerID, domain.ID)
	if err != nil {
		return err
	}
	if curriculum.Version != domain.GraphVersion {
		return fmt.Errorf("curriculum changed: request a new activity")
	}
	competency := curriculumCompetency(curriculum, activity.Concept)
	if competency == nil {
		return fmt.Errorf("selected competency is not active")
	}
	id, err := models.NewCurriculumStableID("decision")
	if err != nil {
		return err
	}
	contract.DecisionID, contract.PolicyVersion, contract.CurriculumVersion = id, models.PedagogicalPolicyVersion, curriculum.Version
	contract.Competency = competency
	contract.LLMInstruction += " Freeze the generated task and rubric with prepare_assessment_attempt using this decision_id before presenting the task. Preserve the target competency and activity type; a changed curriculum requires a new decision."
	// Retain the deterministic contract, not a second copy of learner prose,
	// episodic memory or the presentation prompt (which may include error notes).
	storedContract := *contract
	storedContract.EpisodicContext = nil
	storedContract.ReasoningRequest = nil
	storedContract.LLMInstruction = ""
	storedContract.LearnerExplanation = ""
	storedContract.AuditRationale = ""
	if storedContract.FadeGuidance != nil {
		fade := *storedContract.FadeGuidance
		fade.Instruction = ""
		storedContract.FadeGuidance = &fade
	}
	contextJSON, err := json.Marshal(map[string]any{"runtime": decisionContext,
		"activity_type": activity.Type, "difficulty_target": activity.DifficultyTarget,
		"fsrs_version": algorithms.FSRSVersion, "desired_retention": algorithms.FSRSDesiredRetention})
	if err != nil {
		return fmt.Errorf("encode pedagogical decision context: %w", err)
	}
	return deps.Store.CreatePedagogicalDecision(ctx, scope, &models.PedagogicalDecision{
		ID: id, LearnerID: learnerID, DomainID: domain.ID, SessionID: sessionID,
		CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion,
		Contract: storedContract, CreatedAt: now,
		ContextJSON: string(contextJSON),
	})
}

func bindAssessmentCurriculum(ctx context.Context, deps *Deps, attempt *models.AssessmentAttempt, domain *models.Domain, outcomeIDs []string) error {
	curriculum, err := deps.Store.EnsureCurriculumBaseline(ctx, attempt.LearnerID, domain.ID)
	if err != nil {
		return err
	}
	if curriculum.Version != domain.GraphVersion {
		return fmt.Errorf("curriculum changed: reload before preparing assessment")
	}
	competency := curriculumCompetency(curriculum, attempt.ConceptID)
	if competency == nil {
		return fmt.Errorf("assessment competency is not active")
	}
	if attempt.DecisionID != "" {
		scope, err := pedagogicalScope(ctx, attempt.LearnerID)
		if err != nil {
			return err
		}
		decision, err := deps.Store.GetPedagogicalDecision(ctx, scope, attempt.DecisionID)
		if err != nil {
			return fmt.Errorf("pedagogical decision not found")
		}
		if decision.DomainID != attempt.DomainID || decision.SessionID != attempt.SessionID ||
			decision.Contract.TargetConcept != attempt.ConceptID || string(decision.Contract.RecommendedActivityType) != attempt.ActivityType {
			return fmt.Errorf("assessment does not match the decision's domain, session, competency or activity type")
		}
		if decision.CurriculumVersion != curriculum.Version {
			return fmt.Errorf("decision curriculum is stale: request a new activity")
		}
	}
	known, seen := map[string]bool{}, map[string]bool{}
	for _, o := range competency.Outcomes {
		known[o.ID] = true
	}
	for _, id := range outcomeIDs {
		if !known[id] || seen[id] {
			return fmt.Errorf("outcome_ids must contain unique outcomes from the selected competency")
		}
		seen[id] = true
	}
	if attempt.DecisionID != "" && len(known) > 0 && len(outcomeIDs) == 0 {
		return fmt.Errorf("outcome_ids required for a competency with defined outcomes")
	}
	if outcomeIDs == nil {
		outcomeIDs = []string{}
	}
	attempt.CurriculumVersion = curriculum.Version
	attempt.CurriculumConceptJSON = mustSnapshotJSON(competency)
	attempt.OutcomeIDsJSON = mustSnapshotJSON(outcomeIDs)
	return nil
}
