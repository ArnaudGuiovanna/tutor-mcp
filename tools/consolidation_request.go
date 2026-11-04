// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/memory"
	"tutor-mcp/models"
)

type ClaimMemoryMaintenanceParams struct {
	IdempotencyKey string `json:"idempotency_key" jsonschema:"required unique claim request key; reuse only to retry this same claim, and use a new key for the next poll or batch"`
}

type CommitMemoryMaintenanceParams struct {
	IdempotentMutationParams
	ClaimToken     string                         `json:"claim_token" jsonschema:"opaque token from an unexpired claim_memory_maintenance response"`
	ArchiveSummary string                         `json:"archive_summary" jsonschema:"complete cumulative archive summary combining the previous archive_summary and every supplied session; max 32 KiB"`
	Updates        []models.MemoryMaintenanceItem `json:"updates" jsonschema:"one final content for every claimed snapshot, preserving its exact scope/key/domain_id; include unchanged snapshots too"`
}

func maintenanceToolError(deps *Deps, err error) (*mcp.CallToolResult, any, error) {
	if errors.Is(err, models.ErrMemoryMaintenanceLease) || errors.Is(err, models.ErrMemoryMaintenanceConflict) || errors.Is(err, models.ErrMemoryMaintenanceBudget) || errors.Is(err, memory.ErrNarrativeMutationConflict) {
		r, _ := errorResult(err.Error())
		return r, nil, nil
	}
	r, _ := safeErrorResult(deps.Logger, "memory maintenance unavailable", err)
	return r, nil, nil
}

// Claim has intrinsic durable idempotency. Do not put it behind a generic
// response replay cache: replay must revalidate the current lease and versions.
func registerClaimMemoryMaintenance(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{Name: "claim_memory_maintenance", Description: "Background-only learner memory maintenance. Never call before answering the learner or generating an activity. Claims at most one bounded batch (8 sessions,16 concepts,128 KiB) for 5 minutes. Return to the learner immediately when they send a message; an interrupted batch is resumable. Summarize using the existing host model, then call commit_memory_maintenance once with every snapshot update. idle/busy/expired means no usable batch; poll later with a new idempotency_key."}, func(ctx context.Context, _ *mcp.CallToolRequest, params ClaimMemoryMaintenanceParams) (*mcp.CallToolResult, any, error) {
		learner, err := getLearnerID(ctx)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if !memory.Enabled() {
			r, _ := jsonResult(map[string]any{"status": "not_enabled"})
			return r, nil, nil
		}
		if !memory.UsingSharedNarrativeStore() {
			r, _ := jsonResult(map[string]any{"status": "requires_shared_memory"})
			return r, nil, nil
		}
		if !validIdempotencyKey.MatchString(params.IdempotencyKey) {
			r, _ := errorResult("idempotency_key must contain 1..128 safe characters")
			return r, nil, nil
		}
		principal, _ := auth.GetPrincipal(ctx)
		scope := principal.TenantScope()
		scope.LearnerID = learner
		batch, err := deps.Store.ClaimMemoryMaintenance(ctx, scope, params.IdempotencyKey, time.Now().UTC())
		if err != nil {
			return maintenanceToolError(deps, err)
		}
		r, _ := jsonResult(batch)
		return r, nil, nil
	})
}

func registerCommitMemoryMaintenance(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{Name: "commit_memory_maintenance", Description: "Atomically commit a claimed background memory batch. Send the cumulative archive_summary and exactly all supplied memory/pending/concept snapshots with their final contents. Preserve stable sections; arbitrate pending observations; do not infer mastery or score learner answers. Partial batches keep the archive summary encrypted and unpublished. Only the last complete batch publishes the archive and closes the job. Missing updates, stale sources, changed memory or expired/replaced leases fail without partial writes."}, func(ctx context.Context, _ *mcp.CallToolRequest, params CommitMemoryMaintenanceParams) (*mcp.CallToolResult, any, error) {
		learner, err := getLearnerID(ctx)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if !memory.Enabled() {
			r, _ := jsonResult(map[string]any{"ok": false, "status": "not_enabled"})
			return r, nil, nil
		}
		if !memory.UsingSharedNarrativeStore() {
			r, _ := jsonResult(map[string]any{"ok": false, "status": "requires_shared_memory"})
			return r, nil, nil
		}
		principal, _ := auth.GetPrincipal(ctx)
		scope := principal.TenantScope()
		scope.LearnerID = learner
		result, err := deps.Store.CommitMemoryMaintenance(ctx, scope, params.ClaimToken, params.ArchiveSummary, params.Updates, time.Now().UTC())
		if err != nil {
			return maintenanceToolError(deps, err)
		}
		r, _ := jsonResult(result)
		return r, nil, nil
	})
}
