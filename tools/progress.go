// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	storeport "tutor-mcp/store"
)

func progressTool(name string) bool {
	return name == "list_trainer_cohorts" || name == "get_cohort_insights" || name == "get_learner_progress" || name == "get_learner_badges" || name == "get_cohort_statistics"
}

type CohortInsightsParams struct {
	CohortID     string `json:"cohort_id" jsonschema:"cohort ID returned by list_trainer_cohorts"`
	After        string `json:"after,omitempty" jsonschema:"next_after for the enrollment roster"`
	ConceptAfter string `json:"concept_after,omitempty" jsonschema:"next_concept_after for the separate concept summary page"`
	Limit        int    `json:"limit,omitempty" jsonschema:"page size 1..100, default 20"`
}

type CohortStatisticsParams struct {
	CohortID     string `json:"cohort_id" jsonschema:"cohort ID returned by list_trainer_cohorts"`
	ConceptAfter string `json:"concept_after,omitempty" jsonschema:"next_concept_after for the next concept page"`
	Limit        int    `json:"limit,omitempty" jsonschema:"concept page size 1..100, default 20"`
}

type LearnerProgressParams struct {
	EnrollmentID string `json:"enrollment_id" jsonschema:"exact enrollment ID from get_cohort_insights, never an arbitrary learner or domain ID"`
	After        string `json:"after,omitempty" jsonschema:"next_after for the next concept page"`
	Limit        int    `json:"limit,omitempty" jsonschema:"page size 1..100, default 20"`
}

func registerProgressTools(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{Name: "list_trainer_cohorts", Description: "Discover published institutional cohorts you are authorized to follow as staff, including archived cohorts. Requires progress:read. Follow next_after to list all cohorts; choose one before reading progress."}, func(ctx context.Context, _ *mcp.CallToolRequest, input LearnerFormationPageParams) (*mcp.CallToolResult, any, error) {
		p, _ := auth.GetPrincipal(ctx)
		s, ok := deps.Store.(storeport.ProgressStore)
		if !ok {
			return progressToolResult(deps, nil, storeport.ErrProgressUnavailable)
		}
		if input.Limit == 0 {
			input.Limit = 20
		}
		out, err := s.ListTrainerCohorts(ctx, p, input.After, input.Limit)
		return progressToolResult(deps, out, err)
	})
	addTool(server, &mcp.Tool{Name: "get_cohort_insights", Description: "Read an authorized cohort's named learner roster, evidence-based attention signals and concept mastery estimates. Roster and concept lists have independent cursors. Concept averages require five observed active learners. Generate the requested synthesis in the client using synthesis_guidance; this tool returns observations, never a diagnosis or certification."}, func(ctx context.Context, _ *mcp.CallToolRequest, input CohortInsightsParams) (*mcp.CallToolResult, any, error) {
		p, _ := auth.GetPrincipal(ctx)
		s, ok := deps.Store.(storeport.ProgressStore)
		if !ok {
			return progressToolResult(deps, nil, storeport.ErrProgressUnavailable)
		}
		if input.Limit == 0 {
			input.Limit = 20
		}
		out, err := s.GetCohortInsights(ctx, p, input.CohortID, input.After, input.ConceptAfter, input.Limit)
		return progressToolResult(deps, out, err)
	})
	addTool(server, &mcp.Tool{Name: "get_cohort_statistics", Description: "Read the anonymous worker-computed statistics of an authorized cohort: learner counts, mean and median mastery estimates, completion, badge holder counts and per-concept distribution. Requires progress:read. A null value with status insufficient_data is suppressed to protect learners (fewer than five, or a small complementary group): never estimate or subtract it. Statistics are a snapshot refreshed about hourly; respect status, stale and computed_at. Follow next_concept_after for all concepts and write the synthesis in the client using synthesis_guidance."}, func(ctx context.Context, _ *mcp.CallToolRequest, input CohortStatisticsParams) (*mcp.CallToolResult, any, error) {
		p, _ := auth.GetPrincipal(ctx)
		s, ok := deps.Store.(storeport.StatisticsStore)
		if !ok {
			return progressToolResult(deps, nil, storeport.ErrProgressUnavailable)
		}
		if input.Limit == 0 {
			input.Limit = 20
		}
		out, err := s.GetCohortStatistics(ctx, p, input.CohortID, input.ConceptAfter, input.Limit)
		return progressToolResult(deps, out, err)
	})
	addTool(server, &mcp.Tool{Name: "get_learner_progress", Description: "Read one learner's progress for an exact authorized enrollment: concepts, reviews, evidence counts and the ten most recent session summaries. Historical enrollments stay bound to their own cohort/version. Private chats, answers and affect are excluded. Follow next_after for all concepts and use synthesis_guidance to write a grounded staff synthesis in the client."}, func(ctx context.Context, _ *mcp.CallToolRequest, input LearnerProgressParams) (*mcp.CallToolResult, any, error) {
		p, _ := auth.GetPrincipal(ctx)
		s, ok := deps.Store.(storeport.ProgressStore)
		if !ok {
			return progressToolResult(deps, nil, storeport.ErrProgressUnavailable)
		}
		if input.Limit == 0 {
			input.Limit = 20
		}
		out, err := s.GetLearnerProgress(ctx, p, input.EnrollmentID, input.After, input.Limit)
		return progressToolResult(deps, out, err)
	})
}

func progressToolResult(deps *Deps, out any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		public := storeport.PublicProgressError(err)
		if public.Retryable && deps != nil && deps.Logger != nil {
			deps.Logger.Error("progress read failed", "error_type", "store")
		}
		payload := map[string]any{"error": public}
		result, _ := jsonResult(payload)
		result.IsError = true
		return result, payload, nil
	}
	result, _ := jsonResult(out)
	return result, out, nil
}
