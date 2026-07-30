// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const learningNegotiationOverrideTTL = 30 * time.Minute

const (
	LearningNegotiationOverrideConsumeNone                   = "none"
	LearningNegotiationOverrideConsumeConsumed               = "consumed"
	LearningNegotiationOverrideConsumeExpired                = "expired"
	LearningNegotiationOverrideConsumeRejectedHardConstraint = "rejected_hard_constraint"
	LearningNegotiationOverrideConsumeInvalidPayload         = "invalid_payload"
)

type learningNegotiationProposal struct {
	Concept         string
	Format          string
	ActivityType    models.ActivityType
	Scaffold        bool
	MicroDiagnostic bool
	DeferActivity   bool
	Rationale       string
}

type LearningNegotiationOverride struct {
	ID              int64               `json:"id,omitempty"`
	DomainID        string              `json:"domain_id"`
	SessionID       string              `json:"session_id,omitempty"`
	Concept         string              `json:"concept,omitempty"`
	Format          string              `json:"format,omitempty"`
	ActivityType    models.ActivityType `json:"activity_type,omitempty"`
	Scaffold        bool                `json:"scaffold"`
	MicroDiagnostic bool                `json:"micro_diagnostic"`
	DeferActivity   bool                `json:"defer_activity"`
	Activity        models.Activity     `json:"activity"`
	Rationale       string              `json:"rationale,omitempty"`
	ExpiresAt       time.Time           `json:"expires_at"`
}

type LearningNegotiationOverrideConsumeResult struct {
	Status   string                       `json:"status"`
	ID       int64                        `json:"id,omitempty"`
	Reason   string                       `json:"reason,omitempty"`
	Override *LearningNegotiationOverride `json:"override,omitempty"`
}

var allowedLearningNegotiationActivityTypes = []string{
	string(models.ActivityRecall),
	string(models.ActivityNewConcept),
	string(models.ActivityMasteryChallenge),
	string(models.ActivityDebuggingCase),
	string(models.ActivityRest),
	string(models.ActivityPractice),
	string(models.ActivityDebugMisconception),
	string(models.ActivityFeynmanPrompt),
	string(models.ActivityTransferProbe),
}

func validateLearningNegotiationActivityType(field, raw string) error {
	if raw == "" {
		return nil
	}
	return validateEnum(field, raw, allowedLearningNegotiationActivityTypes)
}

func normalizeLearningNegotiationConcept(concept, learnerConcept string) (string, error) {
	if concept != "" && learnerConcept != "" && concept != learnerConcept {
		return "", fmt.Errorf("concept and learner_concept must match when both are provided")
	}
	if concept != "" {
		return concept, nil
	}
	return learnerConcept, nil
}

func hasLearningNegotiationProposal(params LearningNegotiationParams, concept string) bool {
	return concept != "" ||
		params.Format != "" ||
		params.ActivityType != "" ||
		params.Scaffold ||
		params.MicroDiagnostic ||
		params.DeferActivity
}

func newLearningNegotiationProposal(params LearningNegotiationParams, concept string) learningNegotiationProposal {
	return learningNegotiationProposal{
		Concept:         concept,
		Format:          params.Format,
		ActivityType:    models.ActivityType(params.ActivityType),
		Scaffold:        params.Scaffold,
		MicroDiagnostic: params.MicroDiagnostic,
		DeferActivity:   params.DeferActivity,
		Rationale:       params.LearnerRationale,
	}
}

func buildLearningNegotiationOverride(params LearningNegotiationParams, domainID string, systemActivity models.Activity, states []*models.ConceptState, concept string, now time.Time) LearningNegotiationOverride {
	proposal := newLearningNegotiationProposal(params, concept)
	activity := buildNegotiatedActivity(systemActivity, states, proposal)
	expiresAt := now.UTC().Add(learningNegotiationOverrideTTL)
	return LearningNegotiationOverride{
		DomainID:        domainID,
		SessionID:       params.SessionID,
		Concept:         proposal.Concept,
		Format:          proposal.Format,
		ActivityType:    proposal.ActivityType,
		Scaffold:        proposal.Scaffold,
		MicroDiagnostic: proposal.MicroDiagnostic,
		DeferActivity:   proposal.DeferActivity,
		Activity:        activity,
		Rationale:       proposal.Rationale,
		ExpiresAt:       expiresAt,
	}
}

func buildNegotiatedActivity(systemActivity models.Activity, states []*models.ConceptState, proposal learningNegotiationProposal) models.Activity {
	if proposal.DeferActivity {
		return models.Activity{
			Type:             models.ActivityRest,
			Concept:          proposal.Concept,
			DifficultyTarget: 0.3,
			Format:           "learner_deferred",
			EstimatedMinutes: 5,
			Rationale:        negotiationRationale("learner negotiated a short deferral", proposal.Rationale),
			PromptForLLM:     "Acknowledge the learner's choice to defer this activity, keep the pause brief, and ask when they want to resume.",
		}
	}

	activity := systemActivity
	if proposal.Concept != "" {
		activity.Concept = proposal.Concept
		activity.DifficultyTarget = negotiatedDifficulty(states, proposal.Concept, activity.DifficultyTarget)
	}
	if proposal.ActivityType != "" {
		activity.Type = proposal.ActivityType
	}
	if activity.Type == "" {
		activity.Type = models.ActivityRecall
	}
	if proposal.Format != "" {
		activity.Format = proposal.Format
	}
	if proposal.MicroDiagnostic {
		if activity.Format == "" {
			activity.Format = "micro_diagnostic"
		}
		if activity.EstimatedMinutes <= 0 || activity.EstimatedMinutes > 5 {
			activity.EstimatedMinutes = 5
		}
	}
	if proposal.Scaffold {
		if activity.DifficultyTarget == 0 || activity.DifficultyTarget > 0.55 {
			activity.DifficultyTarget = 0.55
		}
	}
	if activity.Format == "" {
		activity.Format = "mixed"
	}
	if activity.EstimatedMinutes <= 0 {
		activity.EstimatedMinutes = 10
	}
	if activity.DifficultyTarget <= 0 {
		activity.DifficultyTarget = 0.55
	}
	activity.Rationale = negotiationRationale("learner negotiated a one-shot activity override", proposal.Rationale)
	activity.PromptForLLM = negotiatedPrompt(activity, proposal)
	return activity
}

func negotiationRationale(prefix, rationale string) string {
	if strings.TrimSpace(rationale) == "" {
		return prefix
	}
	return fmt.Sprintf("%s: %s", prefix, rationale)
}

func negotiatedPrompt(activity models.Activity, proposal learningNegotiationProposal) string {
	parts := []string{
		fmt.Sprintf("Generate a %s activity", activity.Type),
	}
	if activity.Concept != "" {
		parts[0] += fmt.Sprintf(" on %s", activity.Concept)
	}
	parts = append(parts,
		fmt.Sprintf("Format: %s.", activity.Format),
		"Use the final structured activity.difficulty_target field as the difficulty target.",
	)
	if proposal.Scaffold {
		parts = append(parts, "Use explicit scaffolding and fade help only after the learner succeeds.")
	}
	if proposal.MicroDiagnostic {
		parts = append(parts, "Keep it to one micro-diagnostic prompt that reveals the learner's current misconception or missing step.")
	}
	if proposal.Rationale != "" {
		parts = append(parts, "Learner rationale: "+proposal.Rationale)
	}
	return strings.Join(parts, " ")
}

func negotiatedDifficulty(states []*models.ConceptState, concept string, fallback float64) float64 {
	if cs := conceptStateByName(states, concept); cs != nil {
		d := cs.Difficulty
		if d > 1 {
			d = d / 10
		}
		return clampNegotiatedDifficulty(d)
	}
	if fallback > 0 {
		return clampNegotiatedDifficulty(fallback)
	}
	return 0.55
}

func clampNegotiatedDifficulty(v float64) float64 {
	if v < 0.3 {
		return 0.3
	}
	if v > 0.85 {
		return 0.85
	}
	return v
}

func conceptStateByName(states []*models.ConceptState, concept string) *models.ConceptState {
	for _, cs := range states {
		if cs != nil && cs.Concept == concept {
			return cs
		}
	}
	return nil
}

func masteryByConcept(states []*models.ConceptState) map[string]float64 {
	mastery := make(map[string]float64)
	for _, cs := range states {
		if cs != nil {
			mastery[cs.Concept] = cs.PMastery
		}
	}
	return mastery
}

func unmetHardPrerequisites(domain *models.Domain, mastery map[string]float64, concept string) []string {
	if domain == nil || concept == "" {
		return nil
	}
	var unmet []string
	for _, p := range domain.Graph.Prerequisites[concept] {
		if mastery[p] < algorithms.MasteryMid() {
			unmet = append(unmet, p)
		}
	}
	return unmet
}

func PersistLearningNegotiationOverride(ctx context.Context, store storeport.Store, learnerID string, override *LearningNegotiationOverride, now time.Time) (int64, error) {
	if store == nil {
		return 0, fmt.Errorf("store is nil")
	}
	if learnerID == "" {
		return 0, fmt.Errorf("learner_id is required")
	}
	if override == nil {
		return 0, fmt.Errorf("override is nil")
	}
	if override.DomainID == "" {
		return 0, fmt.Errorf("override domain_id is required")
	}
	if override.ExpiresAt.IsZero() {
		override.ExpiresAt = now.UTC().Add(learningNegotiationOverrideTTL)
	}
	payload, err := json.Marshal(override)
	if err != nil {
		return 0, fmt.Errorf("marshal learning negotiation override: %w", err)
	}
	return store.InsertLearningNegotiationOverridePayload(ctx, learnerID, override.DomainID, string(payload), override.ExpiresAt, now)
}

// ConsumeLearningNegotiationOverride is the integration point for
// tools/activity.go. Call it after the normal activity has been selected and
// before tutor-mode/motivation enrichment. It consumes at most one persisted
// override and only returns it when hard constraints still permit it.
func ConsumeLearningNegotiationOverride(ctx context.Context, store storeport.Store, learnerID string, domain *models.Domain, systemActivity models.Activity, alerts []models.Alert, now time.Time) (models.Activity, LearningNegotiationOverrideConsumeResult, error) {
	if store == nil {
		return systemActivity, LearningNegotiationOverrideConsumeResult{Status: LearningNegotiationOverrideConsumeNone}, fmt.Errorf("store is nil")
	}
	if domain == nil {
		return systemActivity, LearningNegotiationOverrideConsumeResult{Status: LearningNegotiationOverrideConsumeNone}, fmt.Errorf("domain is nil")
	}

	record, err := store.ConsumeLearningNegotiationOverridePayload(ctx, learnerID, domain.ID, now)
	if err != nil {
		return systemActivity, LearningNegotiationOverrideConsumeResult{Status: LearningNegotiationOverrideConsumeNone}, err
	}
	switch record.Status {
	case models.LearningNegotiationOverrideStatusNone:
		return systemActivity, LearningNegotiationOverrideConsumeResult{Status: LearningNegotiationOverrideConsumeNone}, nil
	case models.LearningNegotiationOverrideStatusExpired:
		return systemActivity, LearningNegotiationOverrideConsumeResult{
			Status: LearningNegotiationOverrideConsumeExpired,
			ID:     record.ID,
			Reason: "negotiated override expired before the next activity request",
		}, nil
	}

	var override LearningNegotiationOverride
	if err := json.Unmarshal([]byte(record.Payload), &override); err != nil {
		return systemActivity, LearningNegotiationOverrideConsumeResult{
			Status: LearningNegotiationOverrideConsumeInvalidPayload,
			ID:     record.ID,
			Reason: "persisted negotiated override payload is invalid JSON",
		}, nil
	}
	override.ID = record.ID

	if reason := validateConsumedLearningNegotiationOverride(ctx, store, learnerID, domain, systemActivity, alerts, &override); reason != "" {
		return systemActivity, LearningNegotiationOverrideConsumeResult{
			Status:   LearningNegotiationOverrideConsumeRejectedHardConstraint,
			ID:       record.ID,
			Reason:   reason,
			Override: &override,
		}, nil
	}

	return override.Activity, LearningNegotiationOverrideConsumeResult{
		Status:   LearningNegotiationOverrideConsumeConsumed,
		ID:       record.ID,
		Override: &override,
	}, nil
}

func validateConsumedLearningNegotiationOverride(ctx context.Context, store storeport.Store, learnerID string, domain *models.Domain, systemActivity models.Activity, alerts []models.Alert, override *LearningNegotiationOverride) string {
	if override.DomainID != domain.ID {
		return "override domain does not match the active domain"
	}
	if systemActivity.Type == models.ActivityCloseSession {
		return "system selected CLOSE_SESSION; negotiated override cannot bypass overload"
	}
	for _, alert := range alerts {
		if alert.Type == models.AlertOverload {
			return "active OVERLOAD alert; negotiated override cannot bypass overload"
		}
	}
	if override.DeferActivity && override.Activity.Type != models.ActivityRest {
		return "defer_activity override must resolve to REST"
	}
	if err := validateLearningNegotiationActivityType("override.activity.type", string(override.Activity.Type)); err != nil {
		return err.Error()
	}
	if override.ActivityType != "" {
		if err := validateLearningNegotiationActivityType("override.activity_type", string(override.ActivityType)); err != nil {
			return err.Error()
		}
		if override.Activity.Type != "" && override.Activity.Type != override.ActivityType {
			return "override activity_type does not match override activity"
		}
	}

	concept := override.Activity.Concept
	if concept == "" {
		concept = override.Concept
	}
	if concept == "" {
		return ""
	}
	if err := validateConceptInDomain(domain, concept); err != nil {
		return err.Error()
	}
	if override.DeferActivity {
		return ""
	}

	states, err := store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
	if err != nil {
		return "could not validate concept prerequisites"
	}
	mastery := masteryByConcept(states)
	if unmet := unmetHardPrerequisites(domain, mastery, concept); len(unmet) > 0 {
		return fmt.Sprintf("hard prerequisites not mastered for %q: %s", concept, strings.Join(unmet, ", "))
	}

	switch override.Activity.Type {
	case models.ActivityMasteryChallenge, models.ActivityFeynmanPrompt, models.ActivityTransferProbe:
		cs := conceptStateByName(states, concept)
		if cs == nil || cs.PMastery < algorithms.MasteryBKT() {
			return fmt.Sprintf("activity_type %s requires mastery on %q", override.Activity.Type, concept)
		}
	}
	return ""
}
