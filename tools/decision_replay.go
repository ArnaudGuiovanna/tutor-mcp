// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"time"

	"tutor-mcp/engine"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultDecisionReplaySnapshotsLimit = 100
	maxDecisionReplaySnapshotsLimit     = 500
)

type GetDecisionReplaySummaryParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional; ownership is enforced when provided)"`
	Concept  string `json:"concept,omitempty" jsonschema:"filter by concept (optional)"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum snapshots to replay (optional; default 100, max 500)"`
}

func registerGetDecisionReplaySummary(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_decision_replay_summary",
		Description: "Build an offline replay summary from stored pedagogical snapshots: mastery deltas, suspicious jumps, missing rubric evidence, transfer-after-mastery gaps, and malformed snapshot JSON.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetDecisionReplaySummaryParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_decision_replay_summary", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if err := validateString("domain_id", params.DomainID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateString("concept", params.Concept, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		domainID := params.DomainID
		if domainID != "" {
			domain, err := resolveDomain(ctx, deps.Store, learnerID, domainID)
			if err != nil || domain == nil {
				deps.Logger.Error("get_decision_replay_summary: domain not found", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			domainID = domain.ID
		}

		limit := boundedDecisionReplaySnapshotsLimit(params.Limit)
		snapshots, err := deps.Store.GetPedagogicalSnapshots(ctx, learnerID, domainID, params.Concept, limit)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to fetch pedagogical snapshots", err)
			return r, nil, nil
		}

		since := oldestSnapshotCreatedAt(snapshots)
		if since.IsZero() {
			since = time.Now().UTC().Add(-30 * 24 * time.Hour)
		} else {
			since = since.Add(-time.Minute)
		}
		interactions, err := deps.Store.GetInteractionsSince(ctx, learnerID, since)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to fetch replay interactions", err)
			return r, nil, nil
		}

		summary := engine.BuildDecisionReplaySummary(snapshots, interactions)
		r, _ := jsonResult(map[string]any{
			"domain_id":      domainID,
			"concept":        params.Concept,
			"snapshot_limit": limit,
			"summary":        summary,
		})
		return r, nil, nil
	})
}

func boundedDecisionReplaySnapshotsLimit(limit int) int {
	if limit <= 0 {
		return defaultDecisionReplaySnapshotsLimit
	}
	if limit > maxDecisionReplaySnapshotsLimit {
		return maxDecisionReplaySnapshotsLimit
	}
	return limit
}

func oldestSnapshotCreatedAt(snapshots []*models.PedagogicalSnapshot) time.Time {
	var oldest time.Time
	for _, snapshot := range snapshots {
		if snapshot == nil || snapshot.CreatedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || snapshot.CreatedAt.Before(oldest) {
			oldest = snapshot.CreatedAt
		}
	}
	return oldest
}
