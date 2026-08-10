// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/engine"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type LearningNegotiationParams struct {
	IdempotentMutationParams
	SessionID        string `json:"session_id" jsonschema:"current session ID"`
	Concept          string `json:"concept,omitempty" jsonschema:"concept proposed by the learner; canonical key for concept overrides"`
	LearnerConcept   string `json:"learner_concept,omitempty" jsonschema:"concept proposed by the learner (optional)"`
	Format           string `json:"format,omitempty" jsonschema:"requested activity format override (optional)"`
	ActivityType     string `json:"activity_type,omitempty" jsonschema:"requested activity type override: DIAGNOSTIC_ASSESSMENT, RECALL_EXERCISE, NEW_CONCEPT, MASTERY_CHALLENGE, DEBUGGING_CASE, REST, PRACTICE, DEBUG_MISCONCEPTION, FEYNMAN_PROMPT, TRANSFER_PROBE"`
	Scaffold         bool   `json:"scaffold,omitempty" jsonschema:"whether the next activity should include extra scaffolding"`
	MicroDiagnostic  bool   `json:"micro_diagnostic,omitempty" jsonschema:"whether the next activity should be a short diagnostic prompt"`
	DeferActivity    bool   `json:"defer_activity,omitempty" jsonschema:"whether the learner wants to defer the next activity briefly"`
	LearnerRationale string `json:"learner_rationale,omitempty" jsonschema:"learner's rationale (optional)"`
	DomainID         string `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
}

type tradeoff struct {
	Factor      string  `json:"factor"`
	SystemPlan  string  `json:"system_plan_impact"`
	LearnerPlan string  `json:"learner_plan_impact"`
	Delta       float64 `json:"delta"`
}

func registerLearningNegotiation(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "learning_negotiation",
		Description: "Expose the session plan with rationale. The learner can propose an alternative - the system accepts or explains the trade-offs.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params LearningNegotiationParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "learning_negotiation", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		// String length caps (issue #82). These fields are echoed back into
		// the JSON response and learner_rationale ends up in the negotiated
		// activity rationale. Without these guards a misbehaving caller could
		// push multi-MB strings into orchestrator output.
		stringFields := []struct {
			name  string
			value string
			max   int
		}{
			{"session_id", params.SessionID, maxShortLabelLen},
			{"concept", params.Concept, maxShortLabelLen},
			{"learner_concept", params.LearnerConcept, maxShortLabelLen},
			{"format", params.Format, maxShortLabelLen},
			{"activity_type", params.ActivityType, maxShortLabelLen},
			{"learner_rationale", params.LearnerRationale, maxNoteLen},
			{"domain_id", params.DomainID, maxShortLabelLen},
		}
		for _, f := range stringFields {
			if err := validateString(f.name, f.value, f.max); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}
		concept, err := normalizeLearningNegotiationConcept(params.Concept, params.LearnerConcept)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateLearningNegotiationActivityType("activity_type", params.ActivityType); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.DeferActivity && params.MicroDiagnostic {
			r, _ := errorResult("defer_activity and micro_diagnostic cannot both be true")
			return r, nil, nil
		}
		if params.DeferActivity && params.ActivityType != "" && params.ActivityType != string(models.ActivityRest) {
			r, _ := errorResult("defer_activity can only be combined with activity_type REST")
			return r, nil, nil
		}

		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("learning_negotiation: domain not found by id", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("learning_negotiation: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult()
			return r, nil, nil
		}

		// Issue #92: when the learner proposes a concept, validate it is part
		// of the active domain before any plan construction. Without this
		// guard the orchestrator silently builds an "accepted" plan around a
		// hallucinated concept (no prereqs to break, no states to consult)
		// and returns counts_as_self_initiated=true. Mirror the record_
		// interaction / transfer_challenge guard pattern. Concept overrides
		// are optional (system_plan-only path), so only validate when non-empty.
		if concept != "" {
			if err := validateConceptInDomain(domain, concept); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}

		states, err := deps.Store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load negotiation state", err)
			return r, nil, nil
		}

		domainConcepts := make(map[string]bool)
		for _, c := range domain.Graph.Concepts {
			domainConcepts[c] = true
		}
		var domainStates []*models.ConceptState
		for _, cs := range states {
			if domainConcepts[cs.Concept] {
				domainStates = append(domainStates, cs)
			}
		}

		mastery := masteryByConcept(domainStates)

		// Same orchestrator path as get_next_activity, so the system_plan
		// shown to the learner during negotiation matches what would be
		// served on the next activity request.
		now := time.Now().UTC()
		systemActivity, orchErr := engine.Orchestrate(ctx, deps.Store, engine.OrchestratorInput{
			LearnerID:   learnerID,
			DomainID:    domain.ID,
			Now:         now,
			PreviewOnly: true,
			Config:      engine.NewDefaultPhaseConfig(),
			Logger:      deps.Logger,
		})
		if orchErr != nil {
			deps.Logger.Error("learning_negotiation: orchestrator failed", "err", orchErr, "learner", learnerID)
			r, _ := errorResult("could not compute system plan")
			return r, nil, nil
		}

		result := map[string]interface{}{
			"system_plan": map[string]interface{}{
				"activity":  systemActivity,
				"rationale": systemActivity.Rationale,
			},
		}

		if hasLearningNegotiationProposal(params, concept) {
			proposal := newLearningNegotiationProposal(params, concept)
			var tradeoffs []tradeoff

			if systemActivity.Type == models.ActivityCloseSession && !proposal.DeferActivity {
				tradeoffs = append(tradeoffs, tradeoff{
					Factor:      "overload",
					SystemPlan:  "close the session now",
					LearnerPlan: "continue with a negotiated activity",
					Delta:       1.0,
				})
			}

			systemConcept := systemActivity.Concept
			if concept != "" && systemConcept != "" && systemConcept != concept {
				systemCS, err := deps.Store.GetConceptStateInDomain(ctx, learnerID, domain.ID, systemConcept)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					r, _ := safeErrorResult(deps.Logger, "failed to load system concept state", err)
					return r, nil, nil
				}
				if systemCS != nil && systemCS.LastReview != nil {
					currentRet := algorithms.CurrentRetrievability(now, systemCS.LastReview, systemCS.Stability)
					futureRet := algorithms.CurrentRetrievability(now.Add(24*time.Hour), systemCS.LastReview, systemCS.Stability)
					tradeoffs = append(tradeoffs, tradeoff{
						Factor:      "retention",
						SystemPlan:  fmt.Sprintf("reviewing %s keeps retention at %.0f%%", systemConcept, currentRet*100),
						LearnerPlan: fmt.Sprintf("postpone %s - retention will drop to %.0f%% tomorrow", systemConcept, futureRet*100),
						Delta:       currentRet - futureRet,
					})
				}
			}

			unmetPrereqs := unmetHardPrerequisites(domain, mastery, concept)
			if len(unmetPrereqs) > 0 {
				tradeoffs = append(tradeoffs, tradeoff{
					Factor:      "prerequisites",
					SystemPlan:  "prerequis respectes",
					LearnerPlan: fmt.Sprintf("%d prerequis non maitrises pour %s", len(unmetPrereqs), concept),
					Delta:       float64(len(unmetPrereqs)) * 0.2,
				})
			}
			if proposal.Format != "" && proposal.Format != systemActivity.Format {
				tradeoffs = append(tradeoffs, tradeoff{
					Factor:      "format",
					SystemPlan:  fmt.Sprintf("format %s", systemActivity.Format),
					LearnerPlan: fmt.Sprintf("format %s", proposal.Format),
					Delta:       0.05,
				})
			}
			if proposal.ActivityType != "" && proposal.ActivityType != systemActivity.Type {
				tradeoffs = append(tradeoffs, tradeoff{
					Factor:      "activity_type",
					SystemPlan:  fmt.Sprintf("type %s", systemActivity.Type),
					LearnerPlan: fmt.Sprintf("type %s", proposal.ActivityType),
					Delta:       0.05,
				})
			}
			if proposal.Scaffold {
				tradeoffs = append(tradeoffs, tradeoff{
					Factor:      "scaffold",
					SystemPlan:  "standard support",
					LearnerPlan: "more explicit scaffolding",
					Delta:       0,
				})
			}
			if proposal.MicroDiagnostic {
				tradeoffs = append(tradeoffs, tradeoff{
					Factor:      "micro_diagnostic",
					SystemPlan:  "full activity",
					LearnerPlan: "short diagnostic prompt first",
					Delta:       0.05,
				})
			}
			if proposal.DeferActivity {
				tradeoffs = append(tradeoffs, tradeoff{
					Factor:      "defer_activity",
					SystemPlan:  "work on the selected activity now",
					LearnerPlan: "briefly defer the activity",
					Delta:       0.10,
				})
			}
			if concept != "" {
				learnerCS, err := deps.Store.GetConceptStateInDomain(ctx, learnerID, domain.ID, concept)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					r, _ := safeErrorResult(deps.Logger, "failed to load proposed concept state", err)
					return r, nil, nil
				}
				if proposal.ActivityType == models.ActivityMasteryChallenge ||
					proposal.ActivityType == models.ActivityFeynmanPrompt ||
					proposal.ActivityType == models.ActivityTransferProbe {
					if learnerCS == nil || learnerCS.PMastery < algorithms.MasteryBKT() {
						tradeoffs = append(tradeoffs, tradeoff{
							Factor:      "mastery_readiness",
							SystemPlan:  "wait until mastery evidence is strong enough",
							LearnerPlan: fmt.Sprintf("attempt %s before mastery is ready", proposal.ActivityType),
							Delta:       0.25,
						})
					}
				}
			}

			accepted := true
			for _, t := range tradeoffs {
				if t.Delta > 0.15 {
					accepted = false
					break
				}
			}

			acceptedPlan := systemActivity
			var override *LearningNegotiationOverride
			if accepted {
				ov := buildLearningNegotiationOverride(params, domain.ID, systemActivity, domainStates, concept, now)
				id, err := PersistLearningNegotiationOverride(ctx, deps.Store, learnerID, &ov, now)
				if err != nil {
					deps.Logger.Error("learning_negotiation: failed to persist override", "err", err, "learner", learnerID, "domain", domain.ID)
					r, _ := errorResult("could not persist negotiated override")
					return r, nil, nil
				}
				ov.ID = id
				acceptedPlan = ov.Activity
				override = &ov
			}

			if concept != "" {
				result["learner_proposal"] = concept
			} else {
				result["learner_proposal"] = "structured_override"
			}
			result["proposal"] = map[string]interface{}{
				"concept":          concept,
				"format":           params.Format,
				"activity_type":    params.ActivityType,
				"scaffold":         params.Scaffold,
				"micro_diagnostic": params.MicroDiagnostic,
				"defer_activity":   params.DeferActivity,
			}
			result["tradeoffs"] = tradeoffs
			result["accepted"] = accepted
			result["accepted_plan"] = map[string]interface{}{
				"activity":  acceptedPlan,
				"rationale": acceptedPlan.Rationale,
			}
			if override != nil {
				result["override"] = override
				result["override_persistence"] = map[string]interface{}{
					"status":       "persisted",
					"id":           override.ID,
					"expires_at":   override.ExpiresAt,
					"consume_with": "ConsumeLearningNegotiationOverride",
				}
			}
			result["counts_as_self_initiated"] = accepted
		}

		r, _ := jsonResult(result)
		return r, nil, nil
	})
}
