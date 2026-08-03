// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type MarkDomainHighStakesParams struct {
	IdempotentMutationParams
	DomainID string `json:"domain_id" jsonschema:"owned active domain to classify as high stakes"`
}

func registerMarkDomainHighStakes(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mark_domain_high_stakes",
		Description: "Apply the one-way high-stakes safety classification to an owned active domain. Demonstrated claims and intrusive suggestions then require a trusted human-reviewed evaluation; no external evaluator is created or assumed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params MarkDomainHighStakesParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "mark_domain_high_stakes", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if strings.TrimSpace(params.DomainID) == "" {
			r, _ := errorResult("domain_id is required")
			return r, nil, nil
		}
		if err := validateString("domain_id", params.DomainID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			r, _ := errorResult("domain not found")
			return r, nil, nil
		}
		if err := deps.Store.MarkDomainHighStakes(ctx, domain.ID, learnerID); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to apply high-stakes policy", err)
			return r, nil, nil
		}
		humanReviewed, err := deps.Store.HasHumanReviewedEvaluationInDomain(ctx, learnerID, domain.ID)
		if err != nil {
			// The one-way safety classification is already durable. Fail closed
			// for the derived permissions and retain a successful mutation result
			// so idempotency middleware never releases this completed write.
			deps.Logger.Warn("mark_domain_high_stakes: human-review readback degraded", "err", err, "learner", learnerID, "domain", domain.ID)
			r, _ := jsonResult(map[string]any{
				"domain_id":                       domain.ID,
				"high_stakes":                     true,
				"human_reviewed_evaluation":       nil,
				"demonstrated_claims_allowed":     false,
				"intrusive_notifications_allowed": false,
				"policy":                          "trusted human_review evidence is required; host_llm and external_service evaluations do not satisfy this gate",
				"degraded_components":             []string{"human_review_evidence_readback"},
			})
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{
			"domain_id":                       domain.ID,
			"high_stakes":                     true,
			"human_reviewed_evaluation":       humanReviewed,
			"demonstrated_claims_allowed":     humanReviewed,
			"intrusive_notifications_allowed": humanReviewed,
			"policy":                          "trusted human_review evidence is required; host_llm and external_service evaluations do not satisfy this gate",
		})
		return r, nil, nil
	})
}
