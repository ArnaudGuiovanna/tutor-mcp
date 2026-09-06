// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultPedagogicalSnapshotsLimit = 20
	maxPedagogicalSnapshotsLimit     = 100
)

type GetPedagogicalSnapshotsParams struct {
	DomainID string `json:"domain_id,omitempty" jsonschema:"domain ID (optional; ownership is enforced when provided)"`
	Concept  string `json:"concept,omitempty" jsonschema:"filter by concept (optional)"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum snapshots to return (optional; default 20, max 100)"`
}

func registerGetPedagogicalSnapshots(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "get_pedagogical_snapshots",
		Description: "Read stored pedagogical decision snapshots for the authenticated learner, optionally filtered by domain and concept.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetPedagogicalSnapshotsParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_pedagogical_snapshots", err)
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
				deps.Logger.Error("get_pedagogical_snapshots: domain not found", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			domainID = domain.ID
		}

		limit := boundedPedagogicalSnapshotsLimit(params.Limit)
		snapshots, err := deps.Store.GetPedagogicalSnapshots(ctx, learnerID, domainID, params.Concept, limit)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to fetch pedagogical snapshots", err)
			return r, nil, nil
		}

		r, _ := jsonResult(map[string]any{
			"snapshots": normalizePedagogicalSnapshots(snapshots),
		})
		return r, nil, nil
	})
}

func boundedPedagogicalSnapshotsLimit(limit int) int {
	if limit <= 0 {
		return defaultPedagogicalSnapshotsLimit
	}
	if limit > maxPedagogicalSnapshotsLimit {
		return maxPedagogicalSnapshotsLimit
	}
	return limit
}

func normalizePedagogicalSnapshots(snapshots []*models.PedagogicalSnapshot) any {
	if snapshots == nil {
		return []any{}
	}
	out := make([]map[string]any, 0, len(snapshots))
	for _, s := range snapshots {
		if s == nil {
			continue
		}
		applicability := "not_invalidated"
		if s.CurriculumInvalidatedVersion > 0 {
			applicability = "superseded"
		}
		if s.CurriculumInvalidatedVersion < 0 {
			applicability = "unknown"
		}
		out = append(out, map[string]any{
			"curriculum_applicability":       applicability,
			"curriculum_invalidated_version": s.CurriculumInvalidatedVersion,
			"id":                             s.ID,
			"interaction_id":                 s.InteractionID,
			"learner_id":                     s.LearnerID,
			"domain_id":                      s.DomainID,
			"concept":                        s.Concept,
			"activity_type":                  s.ActivityType,
			"before":                         parseSnapshotJSON(s.BeforeJSON),
			"observation":                    parseSnapshotJSON(s.ObservationJSON),
			"after":                          parseSnapshotJSON(s.AfterJSON),
			"decision":                       parseSnapshotJSON(s.DecisionJSON),
			"interpretation_brief":           s.InterpretationBrief,
			"created_at":                     s.CreatedAt,
		})
	}
	return out
}

func parseSnapshotJSON(raw string) any {
	if raw == "" {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]any{"raw": raw, "parse_error": err.Error()}
	}
	return out
}
