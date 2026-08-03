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

type GetMetacognitiveMirrorParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional). If provided, the mirror is restricted to interactions and concept states of that domain. If absent, the computation remains learner-wide."`
}

func registerGetMetacognitiveMirror(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_metacognitive_mirror",
		Description: "Return a factual mirror message if a dependency pattern is consolidated over the last 7 days, otherwise mirror=null. On-demand metacognitive reflection tool. " +
			"When to call: ONLY outside the activity cycle - for example, on an explicit request for a metacognitive review, or when the learner asks about their own learning patterns. " +
			"When NOT to call: if get_next_activity was already called in the same turn, the mirror is already present in its metacognitive_mirror key - a second call here duplicates work (same computation, same webhook queue deduplicated per day). " +
			"Precondition: none; if no pattern is detected, mirror=null is returned without error. " +
			"Returns: {mirror: <object or null>}.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetMetacognitiveMirrorParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_metacognitive_mirror", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		since := time.Now().UTC().Add(-7 * 24 * time.Hour)
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
		calibBias, err := deps.Store.GetCalibrationBias(ctx, learnerID, 20)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load calibration", err)
			return r, nil, nil
		}
		calibHistory, err := deps.Store.GetCalibrationBiasHistory(ctx, learnerID, 20)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load calibration evidence", err)
			return r, nil, nil
		}
		affects, err := deps.Store.GetRecentAffectStates(ctx, learnerID, 10)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load affect history", err)
			return r, nil, nil
		}

		// Domain filter (#95): if domain_id is supplied, restrict the
		// concept-keyed inputs (interactions, states) to that domain's
		// concept set. resolveDomain enforces learner ownership and
		// rejects archived/foreign IDs. AutonomyScores stay learner-wide
		// because they are session-keyed (from affect rows, not concept-
		// keyed) — autonomy is a learner trait, not a domain trait.
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
			calibBias, err = deps.Store.GetCalibrationBiasInDomain(ctx, learnerID, domain.ID, 20)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load domain calibration", err)
				return r, nil, nil
			}
			calibHistory, err = deps.Store.GetCalibrationBiasHistoryInDomain(ctx, learnerID, domain.ID, 20)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to load domain calibration evidence", err)
				return r, nil, nil
			}
		}

		var autonomyScores []float64
		for _, a := range affects {
			autonomyScores = append(autonomyScores, a.AutonomyScore)
		}

		sessionCount := len(engine.GroupIntoSessionsExported(interactions, 2*time.Hour))

		mirror := engine.DetectMirrorPattern(engine.MirrorInput{
			Interactions:       interactions,
			ConceptStates:      states,
			AutonomyScores:     autonomyScores,
			CalibrationBias:    calibBias,
			CalibrationSamples: len(calibHistory),
			SessionCount:       sessionCount,
		})

		if mirror == nil {
			r, _ := jsonResult(map[string]interface{}{
				"mirror": nil,
			})
			return r, nil, nil
		}

		// Persist & enqueue for proactive push (#59). Best-effort: a queue
		// failure must not block the in-session pull response — Claude can
		// still surface the mirror text even if the webhook lane is offline.
		if _, _, err := engine.EnqueueMirrorWebhook(ctx, deps.Store, learnerID, mirror, time.Now().UTC()); err != nil {
			deps.Logger.Warn("get_metacognitive_mirror: enqueue failed", "err", err, "learner", learnerID)
		}

		r, _ := jsonResult(map[string]interface{}{
			"mirror": mirror,
		})
		return r, nil, nil
	})
}
