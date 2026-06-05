// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SetDomainPriorityParams struct {
	DomainID string `json:"domain_id" jsonschema:"target domain ID"`
	Rank     *int   `json:"rank" jsonschema:"positive integer priority rank. Rank 1 is highest priority; explicit ranks route before unranked domains."`
}

func registerSetDomainPriority(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "set_domain_priority",
		Description: "Set the learner-controlled priority for an active domain. " +
			"Use rank 1 for the preferred domain; lower numbers win. Domains with any explicit rank are routed before domains with no rank. " +
			"When ranks are equal or absent, routing falls back to newest created_at first.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params SetDomainPriorityParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "set_domain_priority", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.DomainID == "" {
			r, _ := errorResult("domain_id is required")
			return r, nil, nil
		}
		if params.Rank == nil {
			r, _ := errorResult("rank is required")
			return r, nil, nil
		}
		if *params.Rank < 1 {
			r, _ := errorResult("rank must be >= 1")
			return r, nil, nil
		}

		domain, err := deps.Store.GetDomainByID(params.DomainID)
		if err != nil {
			r, _ := errorResult(fmt.Sprintf("domain not found: %s", params.DomainID))
			return r, nil, nil
		}
		if domain.LearnerID != learnerID {
			r, _ := errorResult("domain not found")
			return r, nil, nil
		}
		if domain.Archived {
			r, _ := errorResult("domain is archived; unarchive before setting priority")
			return r, nil, nil
		}

		if err := deps.Store.SetDomainPriority(domain.ID, learnerID, *params.Rank); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to set domain priority", err)
			return r, nil, nil
		}

		deps.Logger.Info("set_domain_priority: success", "domain", domain.ID, "rank", *params.Rank, "learner", learnerID)
		r, _ := jsonResult(map[string]any{
			"domain_id":      domain.ID,
			"domain_name":    domain.Name,
			"priority_rank":  *params.Rank,
			"routing_policy": "explicit ranks first, lower rank wins, then created_at desc",
		})
		return r, nil, nil
	})
}
