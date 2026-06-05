// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"

	"tutor-mcp/db"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetMisconceptionsParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional; all domains if absent)"`
	Concept  string `json:"concept,omitempty" jsonschema:"filter by concept (optional)"`
}

func registerGetMisconceptions(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_misconceptions",
		Description: "List misconceptions detected per concept, with their status (active/resolved) and frequency. Enables tracking of the learner's recurring confusions.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetMisconceptionsParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_misconceptions", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		// Build concept filter from domain if provided
		var conceptFilter map[string]bool
		if params.DomainID != "" {
			domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
			if err != nil {
				deps.Logger.Error("get_misconceptions: domain resolution failed", "err", err, "domain", params.DomainID)
				r, _ := errorResult(fmt.Sprintf("domain not found: %s", params.DomainID))
				return r, nil, nil
			}
			conceptFilter = make(map[string]bool)
			for _, concept := range domain.Graph.Concepts {
				conceptFilter[concept] = true
			}
		}

		// Narrow filter to specific concept if provided
		if params.Concept != "" {
			if conceptFilter != nil && !conceptFilter[params.Concept] {
				// Concept not in domain, return empty
				r, _ := jsonResult(map[string]any{"misconceptions": []db.MisconceptionGroup{}})
				return r, nil, nil
			}
			conceptFilter = map[string]bool{params.Concept: true}
		}

		// Get misconception groups
		groups, err := deps.Store.GetMisconceptionGroups(ctx, learnerID, conceptFilter)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to fetch misconceptions", err)
			return r, nil, nil
		}

		// Replace nil with empty slice for JSON serialization
		if groups == nil {
			groups = []db.MisconceptionGroup{}
		}

		r, _ := jsonResult(map[string]any{"misconceptions": groups})
		return r, nil, nil
	})
}
