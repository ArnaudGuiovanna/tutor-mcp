// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"sort"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/engine"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetDashboardStateParams struct {
	DomainID        string `json:"domain_id,omitempty" jsonschema:"domain ID (optional). If absent, aggregates all active domains."`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"if true, includes archived domains in the response."`
}

type conceptProgress struct {
	Concept       string               `json:"concept"`
	Mastery       float64              `json:"mastery" jsonschema:"BKT mastery probability as a 0..1 estimate"`
	Retention     float64              `json:"retention" jsonschema:"current FSRS retrievability estimate as a 0..1 float"`
	Status        engine.MasteryStage  `json:"status" jsonschema:"authoritative evidence stage"`
	MasteryStatus engine.MasteryStatus `json:"mastery_status"`
	RoutingStatus string               `json:"routing_status" jsonschema:"KST routing state; never a demonstrated-learning claim"`
	CardState     string               `json:"card_state"`
}

type domainDashboard struct {
	DomainID             string                   `json:"domain_id"`
	Name                 string                   `json:"name"`
	Archived             bool                     `json:"archived"`
	TotalConcepts        int                      `json:"total_concepts"`
	EstimatedCount       int                      `json:"estimated_count"`
	RetainedCount        int                      `json:"retained_count"`
	DemonstratedCount    int                      `json:"demonstrated_count"`
	TransferredCount     int                      `json:"transferred_count"`
	MasteredCount        int                      `json:"mastered_count" jsonschema:"deprecated alias for demonstrated_count"`
	ProgressPct          float64                  `json:"progress_percent" jsonschema:"percentage of active concepts with evidence-backed demonstrated status"`
	EstimatedProgressPct float64                  `json:"estimated_progress_percent" jsonschema:"percentage whose model estimate crossed the routing threshold"`
	Concepts             []conceptProgress        `json:"concepts"`
	RetentionAlerts      []map[string]interface{} `json:"retention_alerts"`
	NextAction           string                   `json:"next_action"`
}

func registerGetDashboardState(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "get_dashboard_state",
		Description: "Return the learning state as structured JSON (per-concept progress, retention alerts, trajectory signal, autonomy metrics, next action). The LLM can formulate a text response from this JSON for the learner.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetDashboardStateParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_dashboard_state", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		sessionStart, err := deps.Store.GetSessionStart(ctx, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load session state", err)
			return r, nil, nil
		}

		var domains []*models.Domain
		if params.DomainID != "" {
			d, derr := deps.Store.GetDomainByID(ctx, params.DomainID)
			if derr != nil {
				r, _ := safeErrorResult(deps.Logger, "domain not found", derr)
				return r, nil, nil
			}
			if d.LearnerID != learnerID {
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			domains = []*models.Domain{d}
		} else {
			allDomains, derr := deps.Store.GetDomainsByLearner(ctx, learnerID, params.IncludeArchived)
			if derr != nil {
				deps.Logger.Error("get_dashboard_state: failed to get domains", "err", derr, "learner", learnerID)
				r, _ := errorResult("no active domain configured")
				return r, nil, nil
			}
			if len(allDomains) == 0 {
				// Issue #33/#90: emit the canonical needs_domain_setup payload
				// so the LLM can branch consistently across chat-side tools.
				r, _ := noActiveDomainResult()
				return r, nil, nil
			}
			domains = allDomains
		}

		var domainDashboards []domainDashboard
		totalEstimated := 0
		totalRetained := 0
		totalDemonstrated := 0
		totalTransferred := 0
		totalConcepts := 0
		now := time.Now().UTC()
		var alerts []models.Alert
		var states []*models.ConceptState
		var interactions []*models.Interaction

		for _, domain := range domains {
			domainStates, err := deps.Store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load domain concept states", err)
				return r, nil, nil
			}
			stateMap := make(map[string]*models.ConceptState, len(domainStates))
			for _, cs := range domainStates {
				stateMap[cs.Concept] = cs
			}

			domainInteractions, err := deps.Store.GetRecentInteractionsByDomain(
				ctx, learnerID, domain.ID, engine.DefaultRecentInteractionsWindow,
			)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load domain interactions", err)
				return r, nil, nil
			}
			states = append(states, domainStates...)
			interactions = append(interactions, domainInteractions...)
			evidenceSnapshot, err := engine.LoadEvidenceSnapshot(
				ctx, deps.Store, learnerID, domain.ID, domain.Graph.Concepts,
			)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load dashboard alert evidence", err)
				return r, nil, nil
			}
			alertEvidence := evidenceSnapshot.ForMasteryAlerts(domainStates)
			domainAlerts := engine.ComputeAlertsWithEvidenceAt(domainStates, domainInteractions, alertEvidence, sessionStart, now)
			for i := range domainAlerts {
				domainAlerts[i].DomainID = domain.ID
			}
			alerts = append(alerts, domainAlerts...)

			mastery := make(map[string]float64)
			for _, c := range domain.Graph.Concepts {
				if cs, ok := stateMap[c]; ok {
					mastery[c] = cs.PMastery
				}
			}

			graph := algorithms.KSTGraph{
				Concepts:      domain.Graph.Concepts,
				Prerequisites: domain.Graph.Prerequisites,
			}

			var concepts []conceptProgress
			estimatedCount := 0
			retainedCount := 0
			demonstratedCount := 0
			transferredCount := 0
			interactionsByConcept := make(map[string][]*models.Interaction, len(domain.Graph.Concepts))
			for _, interaction := range domainInteractions {
				interactionsByConcept[interaction.Concept] = append(interactionsByConcept[interaction.Concept], interaction)
			}
			for _, concept := range domain.Graph.Concepts {
				routingStatus := algorithms.ConceptStatus(graph, mastery, concept)
				cs := stateMap[concept]
				conceptInteractions := interactionsByConcept[concept]
				conceptEvidence := evidenceSnapshot.ForConcept(concept)
				masteryStatus := engine.AssessMasteryStatus(
					learnerID, concept, cs, conceptInteractions,
					conceptEvidence.Transfers, conceptEvidence.Assessments, now,
				)

				cp := conceptProgress{
					Concept:       concept,
					Mastery:       mastery[concept],
					Retention:     masteryStatus.RetentionEstimate,
					Status:        masteryStatus.Stage,
					MasteryStatus: masteryStatus,
					RoutingStatus: routingStatus,
					CardState:     "new",
				}

				if cs != nil {
					cp.CardState = cs.CardState
					cp.Retention = algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
				}

				if masteryStatus.Estimated {
					estimatedCount++
				}
				if masteryStatus.Retained {
					retainedCount++
				}
				if masteryStatus.Demonstrated {
					demonstratedCount++
				}
				if masteryStatus.Transferred {
					transferredCount++
				}
				concepts = append(concepts, cp)
			}

			var retentionAlerts []map[string]interface{}
			for _, cp := range concepts {
				if cp.Retention < 0.50 && cp.CardState != "new" {
					color := "orange"
					if cp.Retention < 0.30 {
						color = "red"
					}
					retentionAlerts = append(retentionAlerts, map[string]interface{}{
						"concept":   cp.Concept,
						"retention": cp.Retention,
						"color":     color,
					})
				}
			}
			if retentionAlerts == nil {
				retentionAlerts = []map[string]interface{}{}
			}

			frontier := algorithms.ComputeFrontier(graph, mastery)
			nextAction := "continue reviewing"
			if len(frontier) > 0 {
				nextAction = fmt.Sprintf("new concept: %s", frontier[0])
			}

			progressPct := 0.0
			estimatedProgressPct := 0.0
			if len(domain.Graph.Concepts) > 0 {
				progressPct = float64(demonstratedCount) / float64(len(domain.Graph.Concepts)) * 100
				estimatedProgressPct = float64(estimatedCount) / float64(len(domain.Graph.Concepts)) * 100
			}

			domainDashboards = append(domainDashboards, domainDashboard{
				DomainID:             domain.ID,
				Name:                 domain.Name,
				Archived:             domain.Archived,
				TotalConcepts:        len(domain.Graph.Concepts),
				EstimatedCount:       estimatedCount,
				RetainedCount:        retainedCount,
				DemonstratedCount:    demonstratedCount,
				TransferredCount:     transferredCount,
				MasteredCount:        demonstratedCount,
				ProgressPct:          progressPct,
				EstimatedProgressPct: estimatedProgressPct,
				Concepts:             concepts,
				RetentionAlerts:      retentionAlerts,
				NextAction:           nextAction,
			})

			totalEstimated += estimatedCount
			totalRetained += retainedCount
			totalDemonstrated += demonstratedCount
			totalTransferred += transferredCount
			totalConcepts += len(domain.Graph.Concepts)
		}

		if alerts == nil {
			alerts = []models.Alert{}
		}
		sort.SliceStable(interactions, func(i, j int) bool {
			return interactions[i].CreatedAt.After(interactions[j].CreatedAt)
		})

		signal := "stable"
		if len(interactions) >= 3 {
			recentSuccesses := 0
			window := interactions
			if len(window) > 5 {
				window = window[:5]
			}
			for _, i := range window {
				if i.Success {
					recentSuccesses++
				}
			}
			rate := float64(recentSuccesses) / float64(len(window))
			if rate >= 0.8 {
				signal = "positive"
			} else if rate < 0.4 {
				signal = "declining"
			}
		}

		globalProgress := 0.0
		globalEstimatedProgress := 0.0
		if totalConcepts > 0 {
			globalProgress = float64(totalDemonstrated) / float64(totalConcepts) * 100
			globalEstimatedProgress = float64(totalEstimated) / float64(totalConcepts) * 100
		}

		since := time.Now().UTC().Add(-30 * 24 * time.Hour)
		var allInteractions []*models.Interaction
		for _, domain := range domains {
			domainInteractions, err := deps.Store.GetInteractionsSinceInDomain(ctx, learnerID, domain.ID, since)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load dashboard interaction history", err)
				return r, nil, nil
			}
			allInteractions = append(allInteractions, domainInteractions...)
		}
		var calibHistory []float64
		if len(domains) == 1 {
			calibHistory, err = deps.Store.GetCalibrationBiasHistoryInDomain(ctx, learnerID, domains[0].ID, 20)
		} else {
			calibHistory, err = deps.Store.GetCalibrationBiasHistory(ctx, learnerID, 20)
		}
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load dashboard calibration", err)
			return r, nil, nil
		}
		// Keep signed bias as its own descriptive output. Prediction accuracy
		// below is computed from individual errors, not the absolute mean.
		calibBias := 0.0
		for _, delta := range calibHistory {
			calibBias += delta
		}
		if len(calibHistory) > 0 {
			calibBias /= float64(len(calibHistory))
		}

		autonomy := engine.ComputeAutonomyMetrics(engine.AutonomyInput{
			Interactions:      allInteractions,
			ConceptStates:     states,
			CalibrationDeltas: calibHistory,
			SessionGap:        2 * time.Hour,
		})

		affects, err := deps.Store.GetRecentAffectStates(ctx, learnerID, 10)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load dashboard affect history", err)
			return r, nil, nil
		}
		var autonomyScores []float64
		var affectLastN []interface{}
		for _, a := range affects {
			autonomyScores = append(autonomyScores, a.AutonomyScore)
			affectLastN = append(affectLastN, a)
		}
		if affectLastN == nil {
			affectLastN = []interface{}{}
		}
		autonomy.Trend = engine.ComputeAutonomyTrendExported(autonomyScores)
		dependencyTrend := autonomy.Trend

		r, _ := jsonResult(map[string]interface{}{
			"domains":                           domainDashboards,
			"total_concepts":                    totalConcepts,
			"total_estimated":                   totalEstimated,
			"total_retained":                    totalRetained,
			"total_demonstrated":                totalDemonstrated,
			"total_transferred":                 totalTransferred,
			"total_mastered":                    totalDemonstrated,
			"global_progress_percent":           globalProgress,
			"global_estimated_progress_percent": globalEstimatedProgress,
			"alerts":                            alerts,
			"signal":                            signal,
			"autonomy_score":                    autonomy.Score,
			"autonomy_score_status":             autonomy.ScoreStatus,
			"autonomy_components":               autonomy,
			"calibration_bias":                  calibBias,
			"affect_last_n":                     affectLastN,
			"dependency_trend":                  dependencyTrend,
		})
		return r, nil, nil
	})
}
