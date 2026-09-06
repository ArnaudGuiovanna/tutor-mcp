// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"time"

	"tutor-mcp/engine"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetAutonomyMetricsParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional). If provided, autonomy metrics computed over interactions and states are restricted to that domain. The trend remains learner-wide (cross-session signal)."`
}

func registerGetAutonomyMetrics(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "get_autonomy_metrics",
		Description: "Descriptive learning rates, prediction accuracy and observation counts. The legacy summary score is not a validated autonomy measure; missing evidence is explicit.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetAutonomyMetricsParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_autonomy_metrics", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		since := time.Now().UTC().Add(-30 * 24 * time.Hour)
		interactions, err := deps.Store.GetInteractionsSince(ctx, learnerID, since)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load interactions", err)
			return r, nil, nil
		}
		states, err := deps.Store.GetConceptStatesByLearner(ctx, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load concept states", err)
			return r, nil, nil
		}
		calibHistory, err := deps.Store.GetCalibrationBiasHistory(ctx, learnerID, 20)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load calibration", err)
			return r, nil, nil
		}

		// Domain filter (#95): if domain_id is supplied, restrict the
		// concept-keyed inputs (interactions, states) to that domain's
		// concept set. resolveDomain enforces learner ownership and
		// rejects archived/foreign IDs. Trend stays learner-wide
		// because it's computed from affect rows (session-keyed, not
		// concept-keyed) and represents a cross-session learner signal.
		if params.DomainID != "" {
			domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
			if err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
			interactions, err = deps.Store.GetInteractionsSinceInDomain(ctx, learnerID, domain.ID, since)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load domain interactions", err)
				return r, nil, nil
			}
			states, err = deps.Store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load domain states", err)
				return r, nil, nil
			}
			calibHistory, err = deps.Store.GetCalibrationBiasHistoryInDomain(ctx, learnerID, domain.ID, 20)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load domain calibration", err)
				return r, nil, nil
			}
		}

		metrics := engine.ComputeAutonomyMetrics(engine.AutonomyInput{
			Interactions:      interactions,
			ConceptStates:     states,
			CalibrationDeltas: calibHistory,
			SessionGap:        2 * time.Hour,
		})

		affects, err := deps.Store.GetRecentAffectStates(ctx, learnerID, 10)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load affect history", err)
			return r, nil, nil
		}
		var scores []float64
		for _, a := range affects {
			scores = append(scores, a.AutonomyScore)
		}
		metrics.Trend = engine.ComputeAutonomyTrendExported(scores)

		r, _ := jsonResult(metrics)
		return r, nil, nil
	})
}
