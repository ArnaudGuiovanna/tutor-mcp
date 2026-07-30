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
	Concept   string  `json:"concept"`
	Mastery   float64 `json:"mastery" jsonschema:"BKT mastery probability as a 0..1 float"`
	Retention float64 `json:"retention" jsonschema:"memory retention probability as a 0..1 float"`
	Status    string  `json:"status"`
	CardState string  `json:"card_state"`
}

type domainDashboard struct {
	DomainID        string                   `json:"domain_id"`
	Name            string                   `json:"name"`
	Archived        bool                     `json:"archived"`
	TotalConcepts   int                      `json:"total_concepts"`
	MasteredCount   int                      `json:"mastered_count"`
	ProgressPct     float64                  `json:"progress_percent" jsonschema:"domain progress as a 0..100 percentage"`
	Concepts        []conceptProgress        `json:"concepts"`
	RetentionAlerts []map[string]interface{} `json:"retention_alerts"`
	NextAction      string                   `json:"next_action"`
}

func registerGetDashboardState(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
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
		totalMastered := 0
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
			domainAlerts := engine.ComputeAlerts(domainStates, domainInteractions, sessionStart)
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
			masteredCount := 0
			for _, concept := range domain.Graph.Concepts {
				status := algorithms.ConceptStatus(graph, mastery, concept)
				cs := stateMap[concept]

				cp := conceptProgress{
					Concept:   concept,
					Mastery:   mastery[concept],
					Retention: 1.0,
					Status:    status,
					CardState: "new",
				}

				if cs != nil {
					cp.CardState = cs.CardState
					cp.Retention = algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
				}

				if status == "done" {
					masteredCount++
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
			if len(domain.Graph.Concepts) > 0 {
				progressPct = float64(masteredCount) / float64(len(domain.Graph.Concepts)) * 100
			}

			domainDashboards = append(domainDashboards, domainDashboard{
				DomainID:        domain.ID,
				Name:            domain.Name,
				Archived:        domain.Archived,
				TotalConcepts:   len(domain.Graph.Concepts),
				MasteredCount:   masteredCount,
				ProgressPct:     progressPct,
				Concepts:        concepts,
				RetentionAlerts: retentionAlerts,
				NextAction:      nextAction,
			})

			totalMastered += masteredCount
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
		if totalConcepts > 0 {
			globalProgress = float64(totalMastered) / float64(totalConcepts) * 100
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
		var calibBias float64
		if len(domains) == 1 {
			calibBias, err = deps.Store.GetCalibrationBiasInDomain(ctx, learnerID, domains[0].ID, 20)
		} else {
			calibBias, err = deps.Store.GetCalibrationBias(ctx, learnerID, 20)
		}
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load dashboard calibration", err)
			return r, nil, nil
		}

		autonomy := engine.ComputeAutonomyMetrics(engine.AutonomyInput{
			Interactions:    allInteractions,
			ConceptStates:   states,
			CalibrationBias: calibBias,
			SessionGap:      2 * time.Hour,
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
			"domains":                 domainDashboards,
			"total_concepts":          totalConcepts,
			"total_mastered":          totalMastered,
			"global_progress_percent": globalProgress,
			"alerts":                  alerts,
			"signal":                  signal,
			"autonomy_score":          autonomy.Score,
			"calibration_bias":        calibBias,
			"affect_last_n":           affectLastN,
			"dependency_trend":        dependencyTrend,
		})
		return r, nil, nil
	})
}
