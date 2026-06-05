// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"

	"tutor-mcp/engine"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetPendingAlertsParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID to scope activity alerts; if absent, aggregates across all active domains of the learner"`
}

func registerGetPendingAlerts(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_pending_alerts",
		Description: "Retrieve ALL pending alerts for the learner - activity alerts (PLATEAU, FATIGUE, etc.) AND metacognitive alerts (DEPENDENCY_INCREASING, CALIBRATION_DIVERGING, AFFECT_NEGATIVE, TRANSFER_BLOCKED). " +
			"When to call: FIRST each turn, before any other read tool. If a critical alert surfaces (has_critical=true), handle it before proceeding. " +
			"When NOT to call: once per turn is sufficient; metacognitive alerts are already included here - do not call a separate tool to retrieve them. " +
			"Precondition: if the learner has no active domain, returns needs_domain_setup=true and an empty alerts list - call init_domain before continuing. " +
			"Returns: {alerts: [...], has_critical: bool, needs_domain_setup: bool}.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetPendingAlertsParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_pending_alerts", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		states, _ := deps.Store.GetConceptStatesByLearner(ctx, learnerID)
		interactions, _ := deps.Store.GetRecentInteractionsByLearner(ctx, learnerID, engine.DefaultRecentInteractionsWindow)
		sessionStart, _ := deps.Store.GetSessionStart(ctx, learnerID)

		// Resolve which domain(s) constrain this alert computation. The
		// README contract for the Alert Engine is that orphan concept
		// history (states/interactions on concepts no longer in any
		// active domain — e.g. survivors of a deleted domain) must NEVER
		// surface as alerts.
		if params.DomainID != "" {
			// Single-domain branch: explicit domain_id given, scope to
			// that domain's concepts (or refuse if the lookup fails or
			// the domain doesn't belong to this learner).
			domain, domainErr := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
			if domainErr != nil || domain == nil {
				deps.Logger.Error("get_pending_alerts: domain not found", "err", domainErr, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			domainConcepts := make(map[string]bool, len(domain.Graph.Concepts))
			for _, c := range domain.Graph.Concepts {
				domainConcepts[c] = true
			}
			states = filterStatesByConcepts(states, domainConcepts)
			interactions = filterInteractionsByConcepts(interactions, domainConcepts)
		} else {
			// No domain_id given: compute alerts over the union of
			// concepts across all non-archived domains. If the learner
			// has zero active domains, return a clean empty payload with
			// needs_domain_setup so the LLM can self-correct.
			activeDomains, _ := deps.Store.GetDomainsByLearner(ctx, learnerID, false)
			if len(activeDomains) == 0 {
				r, _ := jsonResult(map[string]interface{}{
					"alerts":             []models.Alert{},
					"has_critical":       false,
					"needs_domain_setup": true,
				})
				return r, nil, nil
			}
			activeConcepts := make(map[string]bool)
			for _, d := range activeDomains {
				for _, c := range d.Graph.Concepts {
					activeConcepts[c] = true
				}
			}
			states = filterStatesByConcepts(states, activeConcepts)
			interactions = filterInteractionsByConcepts(interactions, activeConcepts)
		}

		alerts := engine.ComputeAlerts(states, interactions, sessionStart)

		// Metacognitive alerts (DEPENDENCY_INCREASING, CALIBRATION_DIVERGING,
		// AFFECT_NEGATIVE, TRANSFER_BLOCKED) are cross-domain learner-level
		// signals. They are computed alongside the activity-level alerts
		// above and merged before returning. Errors fetching any single
		// input are tolerated — the missing input simply skips its branch
		// inside ComputeMetacognitiveAlerts (defensive: a corrupt affect
		// row shouldn't block the whole alert payload).
		affects, _ := deps.Store.GetRecentAffectStates(ctx, learnerID, 10)
		var autonomyScores []float64
		for _, a := range affects {
			autonomyScores = append(autonomyScores, a.AutonomyScore)
		}
		calibBias, _ := deps.Store.GetCalibrationBias(ctx, learnerID, 20)
		transfers, _ := deps.Store.GetTransferRecordsByLearner(ctx, learnerID)
		metaAlerts := engine.ComputeMetacognitiveAlerts(
			autonomyScores,
			calibBias,
			affects,
			interactions,
			engine.WithTransferData(states, transfers),
		)
		alerts = mergeMetacognitiveAlerts(alerts, metaAlerts)

		hasCritical := false
		for _, a := range alerts {
			if a.Urgency == models.UrgencyCritical {
				hasCritical = true
				break
			}
		}
		if alerts == nil {
			alerts = []models.Alert{}
		}

		r, _ := jsonResult(map[string]interface{}{
			"alerts":             alerts,
			"has_critical":       hasCritical,
			"needs_domain_setup": false,
		})
		return r, nil, nil
	})
}

// mergeMetacognitiveAlerts appends meta alerts to the activity-level alert
// list while deduping on (Type, Concept). The Type is the alert kind
// (DEPENDENCY_INCREASING, CALIBRATION_DIVERGING, AFFECT_NEGATIVE,
// TRANSFER_BLOCKED) and Concept disambiguates per-concept TRANSFER_BLOCKED
// entries (the other three are learner-wide and carry an empty concept).
// This guards against double-emit if ComputeAlerts ever starts producing
// the same kinds, and against duplicates if this function is called twice
// in the same payload assembly.
func mergeMetacognitiveAlerts(base, extra []models.Alert) []models.Alert {
	seen := make(map[string]bool, len(base))
	for _, a := range base {
		seen[string(a.Type)+"|"+a.Concept] = true
	}
	for _, a := range extra {
		key := string(a.Type) + "|" + a.Concept
		if seen[key] {
			continue
		}
		seen[key] = true
		base = append(base, a)
	}
	return base
}
