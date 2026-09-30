// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	storeport "tutor-mcp/store"
)

type BadgeParams struct {
	EnrollmentID string `json:"enrollment_id" jsonschema:"enrollment ID from get_my_formations (learner) or get_cohort_insights (authorized staff)"`
	After        string `json:"after,omitempty" jsonschema:"next_after from the preceding badge page"`
	Limit        int    `json:"limit,omitempty" jsonschema:"page size 1..100, default 20"`
}

func registerBadgeTools(server *mcp.Server, deps *Deps) {
	for _, entry := range []struct {
		name, description string
		staff             bool
	}{
		{"get_my_badges", "Read your dated mastery, formation completion and FSRS retention badges (7, 30, 90 days) for an enrollment from get_my_formations. Follow next_after for all badges. Badges retain evidence identifiers and its current evidence_status; they do not assert current memory strength or external certification.", false},
		{"get_learner_badges", "Read dated badges for an enrollment in your current staff assignments. Requires progress:read. Mastery, formation completion and FSRS retention badges cite their supporting evidence. Follow next_after, respect evidence_status, and never present a historical badge as proof of current memory strength or external certification.", true},
	} {
		addTool(server, &mcp.Tool{Name: entry.name, Description: entry.description}, func(ctx context.Context, _ *mcp.CallToolRequest, input BadgeParams) (*mcp.CallToolResult, any, error) {
			p, _ := auth.GetPrincipal(ctx)
			s, ok := deps.Store.(storeport.BadgeStore)
			if !ok {
				return badgeToolResult(deps, nil, storeport.ErrProgressUnavailable)
			}
			if input.Limit == 0 {
				input.Limit = 20
			}
			out, err := s.GetEnrollmentBadges(ctx, p, input.EnrollmentID, input.After, input.Limit, entry.staff)
			return badgeToolResult(deps, out, err)
		})
	}
}

func badgeToolResult(deps *Deps, out any, err error) (*mcp.CallToolResult, any, error) {
	if err == nil {
		result, _ := jsonResult(out)
		return result, out, nil
	}
	public := storeport.PublicProgressError(err)
	if public.Code == "forbidden" {
		public.Message = "Current membership and badge read access are required."
	}
	if public.Code == "not_found" {
		public.Message = "The requested enrollment is not available in your scope."
	}
	if public.Retryable {
		public.Message = "Badges are temporarily unavailable. Try again later."
		if deps.Logger != nil {
			deps.Logger.Error("badge read failed", "error_type", "store")
		}
	}
	payload := map[string]any{"error": public}
	result, _ := jsonResult(payload)
	result.IsError = true
	return result, payload, nil
}
