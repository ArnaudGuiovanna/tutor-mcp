// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/engine"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetLearnerContextParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional; last active domain used if absent)"`
}

func registerGetLearnerContext(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "get_learner_context",
		Description: "Retrieve the full learner context for session start. When priority_concept is set, priority_concept_domain_id identifies the domain clients should pass when following it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetLearnerContextParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_learner_context", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		now := time.Now().UTC()
		overview, err := deps.Store.GetLearnerContextOverview(ctx, learnerID, now)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "learner not found", err)
			return r, nil, nil
		}
		profile := models.LearnerProfile{}
		if overview.Learner.ProfileJSON != "" && overview.Learner.ProfileJSON != "{}" {
			if err := json.Unmarshal([]byte(overview.Learner.ProfileJSON), &profile); err != nil {
				deps.Logger.Error("stored learner profile is invalid", "err", err, "learner", learnerID)
				r, _ := errorResult("stored learner profile is invalid")
				return r, nil, nil
			}
		}

		activeDomains := make([]storeport.LearnerContextDomain, 0, len(overview.Domains))
		archivedDomains := make([]storeport.LearnerContextDomain, 0, len(overview.Domains))
		for _, candidate := range overview.Domains {
			if candidate.Archived {
				archivedDomains = append(archivedDomains, candidate)
			} else {
				activeDomains = append(activeDomains, candidate)
			}
		}
		needsDomainSetup := len(activeDomains) == 0
		var domain *storeport.LearnerContextDomain
		if !needsDomainSetup || params.DomainID != "" {
			domain = resolveLearnerContextDomain(activeDomains, params.DomainID)
			if domain == nil {
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
		}

		// Filter out orphan states/interactions left over from deleted or
		// archived domains — only surface concepts that still belong to an
		// active domain. Without this, priority_concept and opening_message
		// can reference ghost concepts (see bug report from cosmos client).
		conceptDomainIDs := make(map[string]string)
		ambiguousConcepts := make(map[string]bool)
		activeDomainIDs := make(map[string]bool, len(activeDomains))
		for _, d := range activeDomains {
			activeDomainIDs[d.ID] = true
			for _, c := range d.Graph.Concepts {
				if existing, ok := conceptDomainIDs[c]; !ok {
					conceptDomainIDs[c] = d.ID
				} else if existing != d.ID {
					ambiguousConcepts[c] = true
				}
			}
		}
		for concept := range ambiguousConcepts {
			delete(conceptDomainIDs, concept)
		}
		states := make([]storeport.LearnerContextConceptState, 0, len(overview.ConceptStates))
		for _, cs := range overview.ConceptStates {
			if (cs.DomainID != "" && activeDomainIDs[cs.DomainID]) ||
				(cs.DomainID == "" && conceptDomainIDs[cs.Concept] != "") {
				states = append(states, cs)
			}
		}
		interactionsToday := 0
		for _, interaction := range overview.TodayInteractions {
			if (interaction.DomainID != "" && activeDomainIDs[interaction.DomainID]) ||
				(interaction.DomainID == "" && conceptDomainIDs[interaction.Concept] != "") {
				interactionsToday += interaction.Count
			}
		}

		// Compute day number since account creation (day 1 = creation day)
		dayNumber := int(math.Floor(now.Sub(overview.Learner.CreatedAt).Hours()/24)) + 1

		// Last session info
		lastSessionInfo := "premiere session"
		if !overview.Learner.LastActive.IsZero() {
			hoursSince := now.Sub(overview.Learner.LastActive).Hours()
			if hoursSince < 24 {
				lastSessionInfo = fmt.Sprintf("derniere session il y a %.0fh", hoursSince)
			} else {
				lastSessionInfo = fmt.Sprintf("derniere session il y a %d jours", int(hoursSince/24))
			}
		}

		// Today's priority: concept with lowest retention
		var priorityConcept string
		var priorityConceptDomainID string
		var priorityRetention float64 = 1.0
		for _, cs := range states {
			if cs.CardState == "new" {
				continue
			}
			ret := algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
			if ret < priorityRetention {
				priorityRetention = ret
				priorityConcept = cs.Concept
				priorityConceptDomainID = cs.DomainID
				if priorityConceptDomainID == "" {
					priorityConceptDomainID = conceptDomainIDs[cs.Concept]
				}
			}
		}

		// Build opening message
		openingMessage := fmt.Sprintf("Day %d", dayNumber)
		if overview.Learner.Objective != "" {
			openingMessage += fmt.Sprintf(" - Goal: %s", overview.Learner.Objective)
		}
		openingMessage += fmt.Sprintf(" - %s", lastSessionInfo)
		if priorityConcept != "" {
			openingMessage += fmt.Sprintf(" - Priority: %s (retention %.0f%%)", priorityConcept, priorityRetention*100)
		}

		// List active domains for multi-domain awareness
		var domainList []map[string]interface{}
		for _, d := range activeDomains {
			var priorityRank interface{}
			if d.PriorityRank != nil {
				priorityRank = *d.PriorityRank
			}
			domainList = append(domainList, map[string]interface{}{
				"domain_id":     d.ID,
				"name":          d.Name,
				"concept_count": len(d.Graph.Concepts),
				"priority_rank": priorityRank,
				"high_stakes":   d.HighStakes,
			})
		}
		if domainList == nil {
			domainList = []map[string]interface{}{}
		}

		// List archived domains so Claude knows they exist.
		var archivedList []map[string]interface{}
		for _, d := range archivedDomains {
			archivedList = append(archivedList, map[string]interface{}{
				"domain_id": d.ID,
				"name":      d.Name,
			})
		}
		if archivedList == nil {
			archivedList = []map[string]interface{}{}
		}

		// Progress narrative — open learner model surfaced at session start.
		var narrative *models.ProgressNarrative
		if !needsDomainSetup && domain != nil {
			signals, signalsErr := deps.Store.GetLearnerContextNarrativeSignals(ctx, learnerID, domain.ID, domain.Graph.Concepts, now)
			if signalsErr != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to build progress narrative", signalsErr)
				return r, nil, nil
			}
			narrative = buildProgressNarrative(now, overview.Learner.LastActive, domain, overview.ConceptStates, signals)
		}

		payload := map[string]interface{}{
			"learner_id":         learnerID,
			"objective":          overview.Learner.Objective,
			"profile":            profile,
			"day_number":         dayNumber,
			"last_session":       lastSessionInfo,
			"concepts_count":     len(states),
			"interactions_today": interactionsToday,
			"needs_domain_setup": needsDomainSetup,
			"opening_message":    openingMessage,
			"priority_concept":   priorityConcept,
			"priority_retention": priorityRetention,
			"domains":            domainList,
			"archived_domains":   archivedList,
			"progress_narrative": narrative,
		}
		// An interrupted client can resume the canonical durable session
		// instead of inventing a second correlation ID.
		activeSession, sessionErr := deps.Store.GetActiveLearningSession(ctx, learnerID)
		if sessionErr != nil && !errors.Is(sessionErr, storeport.ErrNotFound) {
			r, _ := safeErrorResult(deps.Logger, "failed to load active learning session", sessionErr)
			return r, nil, nil
		}
		if sessionErr == nil {
			payload["active_session"] = activeSession
			payload["session_id"] = activeSession.ID
		}
		if priorityConcept != "" {
			payload["priority_concept_domain_id"] = priorityConceptDomainID
		}

		r, _ := jsonResult(payload)
		return r, nil, nil
	})
}

// buildProgressNarrative composes the session-opening OLM narrative from the
// specialized read model. It performs no storage reads.
func buildProgressNarrative(now time.Time, lastActive time.Time, domain *storeport.LearnerContextDomain, states []storeport.LearnerContextConceptState, signals *storeport.LearnerContextNarrativeSignals) *models.ProgressNarrative {
	stateByConcept := make(map[string]storeport.LearnerContextConceptState)
	for _, state := range states {
		if state.DomainID == domain.ID {
			stateByConcept[state.Concept] = state
		}
	}
	historyByConcept := make(map[string]storeport.LearnerContextConceptHistory, len(signals.ConceptHistory))
	for _, history := range signals.ConceptHistory {
		historyByConcept[history.Concept] = history
	}

	var deltas []models.ConceptDelta
	var milestones []string
	for _, concept := range domain.Graph.Concepts {
		state, ok := stateByConcept[concept]
		if !ok {
			continue
		}
		history := historyByConcept[concept]
		masteryWas := 0.1
		if history.TotalBefore > 0 {
			masteryWas = float64(history.SuccessfulBefore) / float64(history.TotalBefore)
		}
		delta := state.PMastery - masteryWas
		if delta > 0.05 {
			deltas = append(deltas, models.ConceptDelta{
				Concept:    concept,
				MasteryNow: state.PMastery,
				MasteryWas: masteryWas,
				Delta:      delta,
			})
		}
		if state.PMastery >= algorithms.MasteryBKT() && history.RecentSuccess {
			milestones = append(milestones, concept)
		}
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].Delta > deltas[j].Delta })
	if len(deltas) > 3 {
		deltas = deltas[:3]
	}
	sort.Strings(milestones)

	trend := "stable"
	if len(signals.RecentAutonomyScores) >= 3 {
		trend = engine.ComputeAutonomyTrendExported(signals.RecentAutonomyScores)
	}

	dormancy := false
	if !lastActive.IsZero() && now.Sub(lastActive) > 24*time.Hour {
		dormancy = true
	}

	// Only return a narrative if there's something to say.
	if len(deltas) == 0 && signals.SessionStreak == 0 && len(milestones) == 0 && !dormancy {
		return nil
	}

	return &models.ProgressNarrative{
		EstimateTrajectory:     deltas,
		SessionStreak:          signals.SessionStreak,
		AutonomyTrend:          trend,
		EstimateMilestonesWeek: milestones,
		DormancyImminent:       dormancy,
		Instruction:            "Describe the model-estimate trajectory in 1-2 sentences, not a list. Never present an estimate milestone as demonstrated mastery. If dormancy_imminent is true, make the return welcoming and non-blaming.",
	}
}

// resolveLearnerContextDomain preserves resolveDomain's selection contract
// without re-reading domain rows already present in the overview.
func resolveLearnerContextDomain(domains []storeport.LearnerContextDomain, requestedID string) *storeport.LearnerContextDomain {
	if requestedID != "" {
		for i := range domains {
			if domains[i].ID == requestedID {
				return &domains[i]
			}
		}
		return nil
	}
	if len(domains) == 0 {
		return nil
	}
	selected := 0
	for i := 1; i < len(domains); i++ {
		candidate, current := domains[i], domains[selected]
		switch {
		case candidate.PriorityRank != nil && current.PriorityRank == nil:
			selected = i
		case candidate.PriorityRank != nil && current.PriorityRank != nil && *candidate.PriorityRank < *current.PriorityRank:
			selected = i
		case (candidate.PriorityRank == nil) == (current.PriorityRank == nil) &&
			(candidate.PriorityRank == nil || *candidate.PriorityRank == *current.PriorityRank) &&
			candidate.CreatedAt.After(current.CreatedAt):
			selected = i
		}
	}
	return &domains[selected]
}
