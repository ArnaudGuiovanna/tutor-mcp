// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Package tools — set_goal_relevance / get_goal_relevance handlers.
//
// Component [1] of the regulation pipeline. The runtime never produces the
// relevance vector itself: the LLM decomposes the learner's personal_goal
// against the domain's concept list and writes the result via
// set_goal_relevance. Read-only observation via get_goal_relevance lets the
// operator see what the LLM produced before [4] ConceptSelector consumes
// it. See docs/regulation-design/01-goal-decomposer.md.
package tools

import (
	"context"
	"fmt"
	"math"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// regulationGoalEnabled gates registration and execution of both
// set_goal_relevance and get_goal_relevance. Default-on, opt-out via
// REGULATION_GOAL=off — same convention as REGULATION_THRESHOLD.
func regulationGoalEnabled() bool {
	return os.Getenv("REGULATION_GOAL") != "off"
}

const (
	maxGoalRelevanceEntries  = 500 // mirrors maxConceptsPerCall
	maxGoalRelevanceConceptL = 200 // mirrors maxConceptNameLen
)

// ─── set_goal_relevance ──────────────────────────────────────────────────────

type SetGoalRelevanceParams struct {
	DomainID  string             `json:"domain_id,omitempty" jsonschema:"target domain ID (optional; last active domain if absent)"`
	Relevance map[string]float64 `json:"relevance" jsonschema:"map of concept -> relevance score as a 0..1 float. Unknown concepts produce an explicit error. Incremental semantics: only provided concepts are updated."`
}

func registerSetGoalRelevance(server *mcp.Server, deps *Deps) {
	if !regulationGoalEnabled() {
		return
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "set_goal_relevance",
		Description: "Decompose the learner's personal_goal against the domain's concepts. " +
			"For each concept, provide a score in 0..1: 1.0 = central to the goal, " +
			"0.0 = orthogonal. Incremental semantics (merge): only provided concepts are updated, " +
			"others retain their previous score. An unknown concept (absent from the graph) returns an explicit error.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params SetGoalRelevanceParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "set_goal_relevance", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if len(params.Relevance) == 0 {
			r, _ := errorResult("relevance map must be non-empty")
			return r, nil, nil
		}
		if len(params.Relevance) > maxGoalRelevanceEntries {
			r, _ := errorResult(fmt.Sprintf("too many entries: %d (max %d)", len(params.Relevance), maxGoalRelevanceEntries))
			return r, nil, nil
		}
		for k := range params.Relevance {
			if k == "" {
				r, _ := errorResult("empty concept id in relevance map")
				return r, nil, nil
			}
			if len(k) > maxGoalRelevanceConceptL {
				r, _ := errorResult(fmt.Sprintf("concept id too long: %q (max %d chars)", k, maxGoalRelevanceConceptL))
				return r, nil, nil
			}
		}

		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("set_goal_relevance: domain not found by id", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("set_goal_relevance: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult()
			return r, nil, nil
		}

		// OQ-1.4 strictness: every supplied concept MUST be in the graph.
		// First unknown breaks the operation explicitly. No silent ignore.
		known := make(map[string]bool, len(domain.Graph.Concepts))
		for _, c := range domain.Graph.Concepts {
			known[c] = true
		}
		for k := range params.Relevance {
			if !known[k] {
				r, _ := errorResult(fmt.Sprintf("unknown concept %q - not present in domain %q (call get_learner_context to see the concept list)", k, domain.ID))
				return r, nil, nil
			}
		}

		// Clamp to [0,1] / reject NaN. Count clamps for observability.
		clamped := 0
		for k, v := range params.Relevance {
			if math.IsNaN(v) {
				r, _ := errorResult(fmt.Sprintf("NaN score for concept %q", k))
				return r, nil, nil
			}
			if v < 0 {
				params.Relevance[k] = 0
				clamped++
			} else if v > 1 {
				params.Relevance[k] = 1
				clamped++
			}
		}

		merged, err := deps.Store.MergeDomainGoalRelevance(ctx, domain.ID, params.Relevance)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "persist failed", err)
			return r, nil, nil
		}

		// Compute uncovered against the graph captured at read time. If
		// add_concepts ran between read and write, uncovered may include
		// the freshly added concepts — that is the correct stale-after-set
		// signal.
		fresh, _ := deps.Store.GetDomainByID(ctx, domain.ID)
		var uncovered []string
		var staleAfterSet bool
		if fresh != nil {
			uncovered = fresh.UncoveredConcepts()
			staleAfterSet = fresh.GraphVersion > merged.ForGraphVersion
		}

		deps.Logger.Info("goal_relevance updated",
			"learner", learnerID,
			"domain", domain.ID,
			"concepts_updated", len(params.Relevance),
			"covered_total", len(merged.Relevance),
			"all_concepts", len(domain.Graph.Concepts),
			"uncovered", len(uncovered),
			"version", merged.ForGraphVersion,
			"stale_after_set", staleAfterSet,
		)
		r, _ := jsonResult(map[string]any{
			"domain_id":              domain.ID,
			"for_graph_version":      merged.ForGraphVersion,
			"concepts_updated":       len(params.Relevance),
			"concepts_clamped":       clamped,
			"covered_concepts_count": len(merged.Relevance),
			"all_concepts_count":     len(domain.Graph.Concepts),
			"uncovered_concepts":     uncovered,
			"stale_after_set":        staleAfterSet,
		})
		return r, nil, nil
	})
}

// ─── get_goal_relevance ──────────────────────────────────────────────────────

type GetGoalRelevanceParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
}

func registerGetGoalRelevance(server *mcp.Server, deps *Deps) {
	if !regulationGoalEnabled() {
		return
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_goal_relevance",
		Description: "Read the stored relevance vector for a domain and the list of concepts not yet covered. " +
			"Observation tool: use to decide whether to complete with set_goal_relevance (e.g. after add_concepts).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetGoalRelevanceParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_goal_relevance", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("get_goal_relevance: domain not found by id", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("get_goal_relevance: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult()
			return r, nil, nil
		}

		gr := domain.ParseGoalRelevance()
		var relevance map[string]float64
		var setAt any = nil
		forGraphVersion := 0
		if gr != nil {
			relevance = gr.Relevance
			forGraphVersion = gr.ForGraphVersion
			setAt = gr.SetAt
		}
		uncovered := domain.UncoveredConcepts()
		coveredCount := 0
		if relevance != nil {
			coveredCount = len(relevance)
		}

		payload := map[string]any{
			"domain_id":              domain.ID,
			"graph_version":          domain.GraphVersion,
			"for_graph_version":      forGraphVersion,
			"stale":                  domain.IsGoalRelevanceStale(),
			"all_concepts_count":     len(domain.Graph.Concepts),
			"covered_concepts_count": coveredCount,
			"uncovered_concepts":     uncovered,
			"set_at":                 setAt,
		}
		// Emit relevance only when non-empty so a fresh-uninstrumented
		// domain doesn't return a misleading "{}".
		if len(relevance) > 0 {
			payload["relevance"] = relevance
		}
		r, _ := jsonResult(payload)
		return r, nil, nil
	})
}
