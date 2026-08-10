// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"

	"tutor-mcp/engine"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetOLMSnapshotParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional; last active domain used if absent)"`
	Scope    string `json:"scope,omitempty" jsonschema:"'session' (default, single-domain snapshot) or 'global' (multi-domain aggregation)"`
}

func registerGetOLMSnapshot(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "get_olm_snapshot",
		Description: "Return a transparent snapshot of model estimates and evidence-backed stages (retained, demonstrated, transferred), plus focus and metacognitive signals. Learner and tutor see the same data.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetOLMSnapshotParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_olm_snapshot", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if params.Scope == "global" {
			g, err := engine.BuildGlobalOLMSnapshot(ctx, deps.Store, learnerID)
			if err != nil {
				deps.Logger.Error("get_olm_snapshot global: build failed", "err", err, "learner", learnerID)
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
			r, _ := jsonResult(g)
			return r, nil, nil
		}

		// Default — session scope (existing behavior).
		snap, err := engine.BuildOLMSnapshot(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil {
			deps.Logger.Error("get_olm_snapshot: build failed", "err", err, "learner", learnerID)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		r, _ := jsonResult(snap)
		return r, nil, nil
	})
}
