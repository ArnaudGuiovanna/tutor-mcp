// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

type formationAuthoringStore interface {
	CreateFormationDraftIdempotent(context.Context, models.Principal, string, string, string) (*models.Formation, *models.FormationVersion, bool, error)
	CloneFormationVersionIdempotent(context.Context, models.Principal, string, string) (*models.FormationVersionDetail, bool, error)
	GetFormationVersion(context.Context, models.Principal, string) (*models.FormationVersionDetail, error)
	AddFormationConceptsIdempotent(context.Context, models.Principal, string, string, []models.FormationModuleInput, []models.FormationConceptInput) (*models.FormationVersionDetail, bool, error)
	PublishFormationVersionIdempotent(context.Context, models.Principal, string, string) (*models.FormationVersion, bool, error)
}

func formationTool(name string) bool {
	switch name {
	case "draft_formation", "add_formation_concepts", "publish_formation", "get_formation_version":
		return true
	}
	return false
}

func formationActor(ctx context.Context, deps *Deps) (models.Principal, formationAuthoringStore, error) {
	p, ok := auth.GetPrincipal(ctx)
	if !ok || !p.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: p.TenantID}) {
		return p, nil, fmt.Errorf("formation authoring permission is required")
	}
	s, ok := deps.Store.(formationAuthoringStore)
	if !ok {
		return p, nil, fmt.Errorf("formation authoring is unavailable")
	}
	return p, s, nil
}

type DraftFormationParams struct {
	Name            string `json:"name,omitempty" jsonschema:"name of the new formation; required unless cloning source_version_id"`
	Description     string `json:"description,omitempty" jsonschema:"formation description"`
	SourceVersionID string `json:"source_version_id,omitempty" jsonschema:"clone this existing version into a new draft of the same formation"`
	IdempotencyKey  string `json:"idempotency_key" jsonschema:"unique retry key; reuse only for the same request"`
}

type AddFormationConceptsParams struct {
	VersionID      string                         `json:"version_id" jsonschema:"draft formation version to edit"`
	Modules        []models.FormationModuleInput  `json:"modules,omitempty" jsonschema:"modules to add or replace by stable_key; defaults to a main module for an empty draft"`
	Concepts       []models.FormationConceptInput `json:"concepts" jsonschema:"concepts to add or replace by stable_key, with observable outcomes and assessment criteria; prerequisite keys may refer to another concept in this batch"`
	IdempotencyKey string                         `json:"idempotency_key" jsonschema:"unique retry key; reuse only for the same request"`
}

type FormationVersionParams struct {
	VersionID string `json:"version_id" jsonschema:"formation version ID"`
}
type PublishFormationParams struct {
	VersionID      string `json:"version_id" jsonschema:"complete draft version reviewed by the formateur"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"unique retry key; reuse only for the same request"`
}

func registerFormationTools(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{Name: "draft_formation", Description: "Create a formation draft, or clone an existing version. Requires explicit formation authoring access. Learners already enrolled remain on their published version."}, func(ctx context.Context, _ *mcp.CallToolRequest, input DraftFormationParams) (*mcp.CallToolResult, any, error) {
		actor, store, err := formationActor(ctx, deps)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if !validIdempotencyKey.MatchString(input.IdempotencyKey) {
			r, _ := errorResult("a valid idempotency_key is required")
			return r, nil, nil
		}
		var output any
		if input.SourceVersionID != "" {
			if input.Name != "" || input.Description != "" {
				r, _ := errorResult("source_version_id cannot be combined with a new name or description")
				return r, nil, nil
			}
			output, _, err = store.CloneFormationVersionIdempotent(ctx, actor, input.IdempotencyKey, input.SourceVersionID)
		} else {
			var f *models.Formation
			var v *models.FormationVersion
			f, v, _, err = store.CreateFormationDraftIdempotent(ctx, actor, input.IdempotencyKey, input.Name, input.Description)
			output = map[string]any{"formation": f, "version": v}
		}
		return formationToolResult(deps, output, err)
	})
	addTool(server, &mcp.Tool{Name: "add_formation_concepts", Description: "Add or replace draft concepts by stable key, including descriptions, outcomes, criteria and prerequisites. The batch is atomic. Published formations are immutable; clone a version first."}, func(ctx context.Context, _ *mcp.CallToolRequest, input AddFormationConceptsParams) (*mcp.CallToolResult, any, error) {
		actor, store, err := formationActor(ctx, deps)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if !validIdempotencyKey.MatchString(input.IdempotencyKey) || len(input.Concepts) == 0 || len(input.Concepts) > 500 || len(input.Modules) > 100 {
			r, _ := errorResult("provide a valid retry key and 1..500 concepts, with at most 100 modules")
			return r, nil, nil
		}
		out, _, err := store.AddFormationConceptsIdempotent(ctx, actor, input.IdempotencyKey, input.VersionID, input.Modules, input.Concepts)
		return formationToolResult(deps, out, err)
	})
	addTool(server, &mcp.Tool{Name: "get_formation_version", Description: "Read the complete version of a formation you own or are assigned to, including modules, concept IDs, pedagogical definitions and prerequisites. Review a draft before publishing."}, func(ctx context.Context, _ *mcp.CallToolRequest, input FormationVersionParams) (*mcp.CallToolResult, any, error) {
		actor, store, err := formationActor(ctx, deps)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		out, err := store.GetFormationVersion(ctx, actor, input.VersionID)
		return formationToolResult(deps, out, err)
	})
	addTool(server, &mcp.Tool{Name: "publish_formation", Description: "Publish a reviewed formation draft. Its content becomes immutable and available for cohort enrollment. Requires the formateur's explicit decision to publish; existing enrollments never migrate automatically."}, func(ctx context.Context, _ *mcp.CallToolRequest, input PublishFormationParams) (*mcp.CallToolResult, any, error) {
		actor, store, err := formationActor(ctx, deps)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if !validIdempotencyKey.MatchString(input.IdempotencyKey) {
			r, _ := errorResult("a valid idempotency_key is required")
			return r, nil, nil
		}
		out, _, err := store.PublishFormationVersionIdempotent(ctx, actor, input.IdempotencyKey, input.VersionID)
		return formationToolResult(deps, out, err)
	})
}

func formationToolResult(deps *Deps, output any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		r, _ := safeErrorResult(deps.Logger, "formation request rejected; check your assignment, draft state, retry key and concept definitions", err)
		return r, nil, nil
	}
	r, _ := jsonResult(output)
	return r, output, nil
}
