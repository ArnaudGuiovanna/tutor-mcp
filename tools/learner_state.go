// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"

	"tutor-mcp/engine"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Views of get_learner_state. Each view keeps the response of the read tool it
// replaced, so the caller selects one bounded projection instead of a
// catch-all payload.
const (
	learnerViewOverview        = "overview"
	learnerViewProgress        = "progress"
	learnerViewModel           = "model"
	learnerViewModelAllDomains = "model_all_domains"
)

var learnerStateViews = []string{learnerViewOverview, learnerViewProgress, learnerViewModel, learnerViewModelAllDomains}

type GetLearnerStateParams struct {
	View            string `json:"view,omitempty" jsonschema:"overview (default): session-start context - profile, objective, active and archived domains, priority concept, opening message and progress narrative. progress: per-concept evidence stages, retention alerts and every pending alert with has_critical, trajectory signal, autonomy, calibration and affect. model: open learner model of one domain - estimates, evidence-backed stages, focus and metacognitive signals, shown identically to learner and tutor. model_all_domains: the open learner model aggregated across active domains"`
	DomainID        string `json:"domain_id,omitempty" jsonschema:"domain ID; overview and model default to the last active domain, progress aggregates every active domain when absent; ignored by model_all_domains"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"progress view only: also show the progress of archived domains when domain_id is absent; archived domains never produce pending alerts"`
}

func registerGetLearnerState(server *mcp.Server, deps *Deps) {
	addTool(server, deps, &mcp.Tool{
		Name: "get_learner_state",
		Description: "Read the learner's state in one of four views (read-only). Use overview at session start, progress when the learner asks how they are doing or to review pending alerts (has_critical=true must be handled first), model for a transparent learner-model snapshot of one domain (for example before a closing message), and model_all_domains for the same across domains. " +
			"Do not call it to choose the next activity: prepare_pedagogical_turn(mode=activity) already applies alerts, mirror, tutor mode and motivation. needs_domain_setup=true means the learner has no domain yet.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetLearnerStateParams) (*mcp.CallToolResult, any, error) {
		view := params.View
		if view == "" {
			view = learnerViewOverview
		}
		if err := validateEnum("view", view, learnerStateViews); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateString("domain_id", params.DomainID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		switch view {
		case learnerViewProgress:
			return learnerProgressResult(ctx, deps, GetDashboardStateParams{DomainID: params.DomainID, IncludeArchived: params.IncludeArchived})
		case learnerViewModel, learnerViewModelAllDomains:
			return learnerModelResult(ctx, deps, params.DomainID, view == learnerViewModelAllDomains)
		default:
			return learnerOverviewResult(ctx, deps, GetLearnerContextParams{DomainID: params.DomainID})
		}
	})
}

// learnerModelResult returns the open learner model of one domain or, with
// allDomains, its aggregation across the learner's active domains.
func learnerModelResult(ctx context.Context, deps *Deps, domainID string, allDomains bool) (*mcp.CallToolResult, any, error) {
	learnerID, err := getLearnerID(ctx)
	if err != nil {
		logAuthFailure(deps, "get_learner_state", err)
		r, _ := errorResult(err.Error())
		return r, nil, nil
	}
	if allDomains {
		global, err := engine.BuildGlobalOLMSnapshot(ctx, deps.Store, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to build the learner model", err)
			return r, nil, nil
		}
		r, _ := jsonResult(global)
		return r, nil, nil
	}
	if domainID == "" {
		domains, err := deps.Store.GetDomainsByLearner(ctx, learnerID, false)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load active domains", err)
			return r, nil, nil
		}
		if len(domains) == 0 {
			r, _ := noActiveDomainResult(deps)
			return r, nil, nil
		}
	} else {
		domain, err := deps.Store.GetLearnerDomainByID(ctx, learnerID, domainID)
		if err != nil {
			r, _ := domainLookupErrorResult(deps, err)
			return r, nil, nil
		}
		if domain.Archived {
			r, _ := errorResult("domain not found")
			return r, nil, nil
		}
	}
	snapshot, err := engine.BuildOLMSnapshot(ctx, deps.Store, learnerID, domainID)
	if err != nil {
		r, _ := safeErrorResult(deps.Logger, "failed to build the learner model", err)
		return r, nil, nil
	}
	r, _ := jsonResult(snapshot)
	return r, nil, nil
}
