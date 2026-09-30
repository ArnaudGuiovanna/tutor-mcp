// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type LearnerFormationPageParams struct {
	After string `json:"after,omitempty" jsonschema:"next_after from the previous page"`
	Limit int    `json:"limit,omitempty" jsonschema:"page size from 1 to 100; defaults to 20"`
}
type LearnerFormationParams struct {
	CohortID string `json:"cohort_id" jsonschema:"cohort selected from the learner's formation catalog"`
}
type LearnerFormationMutationParams struct {
	CohortID       string `json:"cohort_id" jsonschema:"cohort explicitly selected by the learner"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"unique retry key; reuse only for the same action, use a new key for a later rejoin or leave"`
}

func registerLearnerFormationTools(server *mcp.Server, deps *Deps) {
	for _, entry := range []struct {
		name, description string
		mine              bool
	}{
		{"list_available_formations", "List published formation cohorts accessible in your institution, their admission policy and available seats. Includes only your own enrollment status. Choose a cohort explicitly before joining.", false},
		{"get_my_formations", "List your formation enrollments and admission requests, including historical enrollments. The current enrollment status is authoritative; use its domain_id to resume active learning.", true},
	} {
		addTool(server, &mcp.Tool{Name: entry.name, Description: entry.description}, func(ctx context.Context, _ *mcp.CallToolRequest, input LearnerFormationPageParams) (*mcp.CallToolResult, any, error) {
			p, _ := auth.GetPrincipal(ctx)
			s, ok := deps.Store.(storeport.LearnerFormationStore)
			if !ok {
				return learnerFormationResult(deps, nil, storeport.ErrFormationUnavailable)
			}
			if input.Limit == 0 {
				input.Limit = 20
			}
			out, err := s.ListLearnerFormations(ctx, p, entry.mine, input.After, input.Limit)
			return learnerFormationResult(deps, out, err)
		})
	}
	addTool(server, &mcp.Tool{Name: "get_learner_formation", Description: "Read the published program, concepts and assessment criteria of an accessible formation cohort, plus your own enrollment. Drafts and other learners' progress are excluded."}, func(ctx context.Context, _ *mcp.CallToolRequest, input LearnerFormationParams) (*mcp.CallToolResult, any, error) {
		p, _ := auth.GetPrincipal(ctx)
		s, ok := deps.Store.(storeport.LearnerFormationStore)
		if !ok {
			return learnerFormationResult(deps, nil, storeport.ErrFormationUnavailable)
		}
		out, err := s.GetLearnerFormation(ctx, p, input.CohortID)
		return learnerFormationResult(deps, out, err)
	})
	for _, entry := range []struct {
		name, description string
		leave             bool
	}{
		{"join_formation", "Join a cohort after the learner explicitly chooses it. Open admission enrolls immediately; invitation requires a staff invitation; approval creates a pending request without consuming a seat. After approval, confirm with a new retry key. Rejoining the same cohort preserves history and domain_id.", false},
		{"leave_formation", "Leave a formation cohort or cancel its pending admission request, only after the learner explicitly confirms. Releases the seat and archives the learning domain while preserving progress and history. Rejoining requires capacity and valid admission.", true},
	} {
		addTool(server, &mcp.Tool{Name: entry.name, Description: entry.description}, func(ctx context.Context, _ *mcp.CallToolRequest, input LearnerFormationMutationParams) (*mcp.CallToolResult, any, error) {
			if !validIdempotencyKey.MatchString(input.IdempotencyKey) {
				r, _ := errorResult("a valid idempotency_key is required")
				return r, nil, nil
			}
			p, _ := auth.GetPrincipal(ctx)
			s, ok := deps.Store.(storeport.LearnerFormationStore)
			if !ok {
				return learnerFormationResult(deps, nil, storeport.ErrFormationUnavailable)
			}
			var out *models.FormationJoinResult
			var err error
			if entry.leave {
				out, _, err = s.LeaveFormation(ctx, p, input.IdempotencyKey, input.CohortID)
			} else {
				out, _, err = s.JoinFormation(ctx, p, input.IdempotencyKey, input.CohortID)
			}
			return learnerFormationResult(deps, out, err)
		})
	}
}

func learnerFormationResult(deps *Deps, output any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		message := "formation request rejected; refresh your catalog and check your access and retry key"
		for _, expected := range []error{storeport.ErrFormationAdmissionRequired, storeport.ErrFormationUnavailable, storeport.ErrEnrollmentTransition, storeport.ErrCohortCapacityReached} {
			if errors.Is(err, expected) {
				message = expected.Error()
				break
			}
		}
		r, _ := safeErrorResult(deps.Logger, message, err)
		return r, nil, nil
	}
	r, _ := jsonResult(output)
	return r, output, nil
}
