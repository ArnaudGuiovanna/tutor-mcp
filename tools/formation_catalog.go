// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

type formationCatalogStore interface {
	ListFormations(context.Context, models.Principal, string, int) (models.FormationPage, error)
	ListFormationVersions(context.Context, models.Principal, string, string, int) (models.FormationVersionPage, error)
	ReviewFormationVersion(context.Context, models.Principal, string) (*models.FormationReview, error)
	PublishFormationToCatalogIdempotent(context.Context, models.Principal, string, string) (*models.FormationPublication, bool, error)
}

type FormationPageParams struct {
	After string `json:"after,omitempty" jsonschema:"next_after from the previous page; omit for the first page"`
	Limit int    `json:"limit,omitempty" jsonschema:"page size between 1 and 100; defaults to 20"`
}
type FormationVersionsParams struct {
	FormationID string `json:"formation_id" jsonschema:"real formation ID returned by list_formations, never an intention ID"`
	After       string `json:"after,omitempty" jsonschema:"next_after from the previous page"`
	Limit       int    `json:"limit,omitempty" jsonschema:"page size between 1 and 100; defaults to 20"`
}

func registerFormationCatalogTools(server *mcp.Server, deps *Deps) {
	actor := func(ctx context.Context) (models.Principal, formationCatalogStore, bool) {
		p, ok := auth.GetPrincipal(ctx)
		s, available := deps.Store.(formationCatalogStore)
		return p, s, ok && available && p.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: p.TenantID})
	}
	addTool(server, &mcp.Tool{Name: "list_formations", Description: "List the real institution formations you may design, including drafts. Start here in authoring mode, then use list_formation_versions. This is not the trainer cohort or learner catalogue."}, func(ctx context.Context, _ *mcp.CallToolRequest, input FormationPageParams) (*mcp.CallToolResult, any, error) {
		p, s, ok := actor(ctx)
		if !ok {
			r, _ := errorResult("formation reading permission is required")
			return r, nil, nil
		}
		if input.Limit == 0 {
			input.Limit = 20
		}
		page, err := s.ListFormations(ctx, p, input.After, input.Limit)
		return formationToolResult(deps, page, err)
	})
	addTool(server, &mcp.Tool{Name: "list_formation_versions", Description: "List the real version IDs of a formation, newest first, including drafts and published versions. Always obtain version_id here or from draft_formation; an intention ID is not a version ID."}, func(ctx context.Context, _ *mcp.CallToolRequest, input FormationVersionsParams) (*mcp.CallToolResult, any, error) {
		p, s, ok := actor(ctx)
		if !ok {
			r, _ := errorResult("formation reading permission is required")
			return r, nil, nil
		}
		if input.Limit == 0 {
			input.Limit = 20
		}
		page, err := s.ListFormationVersions(ctx, p, input.FormationID, input.After, input.Limit)
		return formationToolResult(deps, page, err)
	})
	addTool(server, &mcp.Tool{Name: "review_formation_version", Description: "Read the complete formation version and the server's publication checks. Present its real modules, concepts, objectives and remaining issues before requesting a decision to publish."}, func(ctx context.Context, _ *mcp.CallToolRequest, input FormationVersionParams) (*mcp.CallToolResult, any, error) {
		p, s, ok := actor(ctx)
		if !ok {
			r, _ := errorResult("formation reading permission is required")
			return r, nil, nil
		}
		out, err := s.ReviewFormationVersion(ctx, p, input.VersionID)
		return formationToolResult(deps, out, err)
	})
	addTool(server, &mcp.Tool{Name: "publish_formation_to_catalog", Description: "After explicit human confirmation, publish the reviewed version and make it available for voluntary enrollment to all members of THIS institution. Never enrolls anyone or exposes it to other institutions. Existing learners keep their version."}, func(ctx context.Context, _ *mcp.CallToolRequest, input PublishFormationParams) (*mcp.CallToolResult, any, error) {
		p, s, ok := actor(ctx)
		if !ok {
			r, _ := errorResult("formation authoring permission is required")
			return r, nil, nil
		}
		if !validIdempotencyKey.MatchString(input.IdempotencyKey) {
			r, _ := errorResult("a valid idempotency_key is required")
			return r, nil, nil
		}
		out, _, err := s.PublishFormationToCatalogIdempotent(ctx, p, input.IdempotencyKey, input.VersionID)
		return formationToolResult(deps, out, err)
	})
}
