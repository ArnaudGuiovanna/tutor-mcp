// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"time"

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

		sessionStart, err := deps.Store.GetSessionStart(ctx, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load session state", err)
			return r, nil, nil
		}

		var domains []*models.Domain
		if params.DomainID != "" {
			domain, domainErr := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
			if domainErr != nil || domain == nil {
				deps.Logger.Error("get_pending_alerts: domain not found", "err", domainErr, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			domains = []*models.Domain{domain}
		} else {
			domains, err = deps.Store.GetDomainsByLearner(ctx, learnerID, false)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load active domains", err)
				return r, nil, nil
			}
			if len(domains) == 0 {
				r, _ := jsonResult(map[string]interface{}{
					"alerts":             []models.Alert{},
					"has_critical":       false,
					"needs_domain_setup": true,
				})
				return r, nil, nil
			}
		}

		// Compute each domain independently so identical concept labels never
		// combine retention, plateau, or misconception evidence.
		var alerts []models.Alert
		var states []*models.ConceptState
		var interactions []*models.Interaction
		for _, domain := range domains {
			domainStates, err := deps.Store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load concept states", err)
				return r, nil, nil
			}
			domainInteractions, err := deps.Store.GetRecentInteractionsByDomain(
				ctx, learnerID, domain.ID, engine.DefaultRecentInteractionsWindow,
			)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load interactions", err)
				return r, nil, nil
			}
			alertEvidence, err := engine.LoadMasteryAlertEvidence(ctx, deps.Store, learnerID, domain.ID, domainStates)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load mastery alert evidence", err)
				return r, nil, nil
			}
			domainAlerts := engine.ComputeAlertsWithEvidenceAt(domainStates, domainInteractions, alertEvidence, sessionStart, time.Now().UTC())
			for i := range domainAlerts {
				domainAlerts[i].DomainID = domain.ID
			}
			alerts = append(alerts, domainAlerts...)
			states = append(states, domainStates...)
			interactions = append(interactions, domainInteractions...)
		}

		// Metacognitive alerts (DEPENDENCY_INCREASING, CALIBRATION_DIVERGING,
		// AFFECT_NEGATIVE, TRANSFER_BLOCKED) are cross-domain learner-level
		// signals. The tool promises the complete pending-alert view, so a
		// failed source read must not masquerade as "no alert".
		affects, err := deps.Store.GetRecentAffectStates(ctx, learnerID, 10)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load affect alerts", err)
			return r, nil, nil
		}
		var autonomyScores []float64
		for _, a := range affects {
			autonomyScores = append(autonomyScores, a.AutonomyScore)
		}
		calibBias, err := deps.Store.GetCalibrationBias(ctx, learnerID, 20)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load calibration alerts", err)
			return r, nil, nil
		}
		calibHistory, err := deps.Store.GetCalibrationBiasHistory(ctx, learnerID, 20)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load calibration evidence", err)
			return r, nil, nil
		}
		transfers, err := deps.Store.GetTransferRecordsByLearner(ctx, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load transfer alerts", err)
			return r, nil, nil
		}
		metaAlerts := engine.ComputeMetacognitiveAlerts(
			autonomyScores,
			calibBias,
			affects,
			interactions,
			engine.WithTransferData(states, transfers),
			engine.WithCalibrationEvidence(len(calibHistory)),
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
