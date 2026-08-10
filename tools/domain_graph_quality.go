// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"

	"tutor-mcp/engine"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ValidateDomainGraphParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional; if absent, the learner's active domain is used)"`
}

func registerValidateDomainGraph(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "validate_domain_graph",
		Description: "Audit the active domain graph deterministically and return structural quality issues plus a prompt the LLM can use to propose graph repairs.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params ValidateDomainGraphParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "validate_domain_graph", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateString("domain_id", params.DomainID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("validate_domain_graph: domain not found", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			r, payload := noActiveDomainResult()
			return r, payload, nil
		}

		report := engine.EvaluateGraphQuality(domain.Graph)
		r, _ := jsonResult(map[string]any{
			"domain_id":              domain.ID,
			"domain_name":            domain.Name,
			"graph_quality_report":   report,
			"graph_quality_guidance": graphQualityGuidance(report),
		})
		return r, nil, nil
	})
}

func graphQualityBlockedResult(report engine.GraphQualityReport) (*mcp.CallToolResult, error) {
	payload := map[string]any{
		"blocked":                true,
		"error":                  "domain graph quality is critical",
		"graph_quality_report":   report,
		"graph_quality_guidance": graphQualityGuidance(report),
	}
	r, err := jsonResult(payload)
	if r != nil {
		r.IsError = true
	}
	return r, err
}

func graphQualityGuidance(report engine.GraphQualityReport) map[string]any {
	if !report.ShouldAskLLMReview {
		return map[string]any{
			"required": false,
			"message":  "graph quality is ok; no repair proposal needed",
		}
	}
	return map[string]any{
		"required": report.Quality == engine.GraphQualityCritical,
		"message":  "use graph_quality_report to propose graph repairs; ask the learner before mutating the domain",
		"prompt":   report.LLMRepairPrompt,
	}
}

func copyPrerequisites(src map[string][]string) map[string][]string {
	out := make(map[string][]string, len(src))
	for concept, prereqs := range src {
		out[concept] = append([]string(nil), prereqs...)
	}
	return out
}

func mergePrerequisites(dst map[string][]string, src map[string][]string) {
	if dst == nil {
		return
	}
	for concept, prereqs := range src {
		existing := make(map[string]bool, len(dst[concept]))
		for _, p := range dst[concept] {
			existing[p] = true
		}
		for _, p := range prereqs {
			if !existing[p] {
				dst[concept] = append(dst[concept], p)
				existing[p] = true
			}
		}
	}
}
