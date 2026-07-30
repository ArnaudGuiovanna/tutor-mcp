// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Deps holds shared dependencies for all MCP tool handlers.
type Deps struct {
	Store   storeport.Store
	Logger  *slog.Logger
	BaseURL string
}

func getLearnerID(ctx context.Context) (string, error) {
	id := auth.GetLearnerID(ctx)
	if id == "" {
		return "", fmt.Errorf("authentication required")
	}
	return id, nil
}

func logAuthFailure(deps *Deps, tool string, err error) {
	if deps == nil || deps.Logger == nil {
		return
	}
	deps.Logger.Info(tool+": auth failed", "err", err)
}

// resolveDomain resolves a domain by ID or falls back to the learner's most recent domain.
//
// Archived domains are explicitly rejected when resolved by ID: see issue #94.
// Without this guard, callers like record_interaction would silently advance
// BKT/FSRS state on a domain the learner has explicitly archived. Archive-
// specific tools (archive_domain, unarchive_domain, delete_domain) do not go
// through resolveDomain — they call store.GetDomainByID directly because they
// legitimately need to operate on archived rows.
func resolveDomain(ctx context.Context, store storeport.Store, learnerID, domainID string) (*models.Domain, error) {
	if domainID != "" {
		d, err := store.GetDomainByID(ctx, domainID)
		if err != nil {
			return nil, err
		}
		if d.LearnerID != learnerID {
			return nil, fmt.Errorf("domain not found")
		}
		if d.Archived {
			return nil, fmt.Errorf("domain not found")
		}
		return d, nil
	}
	return store.GetDomainByLearner(ctx, learnerID)
}

func jsonResult(v interface{}) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		// Most handlers intentionally ignore this helper's Go error because MCP
		// application failures are represented in CallToolResult. Never return
		// a nil/empty result when an internal value (for example NaN) cannot be
		// encoded.
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "failed to encode tool response"}},
			IsError: true,
		}, nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
		StructuredContent: v,
	}, nil
}

func errorResult(msg string) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}, nil
}

// safeErrorResult logs the full underlying error server-side and returns an
// errorResult carrying only the public, LLM-facing message. Issue #3: raw
// SQLite/internal error strings must never be interpolated into model context;
// they leak schema and storage internals. Handlers pass a clean public message
// and the real err — the err is recorded in the server log, not the response.
func safeErrorResult(logger *slog.Logger, publicMsg string, err error) (*mcp.CallToolResult, error) {
	if logger != nil {
		logger.Error(publicMsg, "err", err)
	}
	return errorResult(publicMsg)
}

// noActiveDomainResult returns the canonical "no active domain" payload that
// every chat-side tool emits when called without an explicit DomainID and the
// learner has no domain yet. Issue #33: a uniform shape lets the LLM branch
// on `needs_domain_setup:true` regardless of which tool it called and recover
// by issuing `init_domain`. For an explicit DomainID that does not match the
// learner, callers should keep emitting errorResult("domain not found") —
// that's a genuine dev-facing 404, not a setup precondition.
func noActiveDomainResult() (*mcp.CallToolResult, any) {
	payload := map[string]any{
		"needs_domain_setup":  true,
		"reason":              "no active domain for this learner",
		"next_action_for_llm": "appelle init_domain(name, concepts, prerequisites)",
	}
	r, _ := jsonResult(payload)
	return r, payload
}

// RegisterTools registers all MCP tools and prompts on the given server.
func RegisterTools(server *mcp.Server, deps *Deps) {
	registerGetPendingAlerts(server, deps)
	registerGetNextActivity(server, deps)
	registerRecordInteraction(server, deps)
	registerCheckMastery(server, deps)
	registerGetLearnerContext(server, deps)
	registerGetAvailabilityModel(server, deps)
	registerInitDomain(server, deps)
	registerAddConcepts(server, deps)
	registerValidateDomainGraph(server, deps)
	registerUpdateLearnerProfile(server, deps)
	registerRecordAffect(server, deps)
	registerCalibrationCheck(server, deps)
	registerRecordCalibrationResult(server, deps)
	registerGetAutonomyMetrics(server, deps)
	registerGetMetacognitiveMirror(server, deps)
	registerGetOLMSnapshot(server, deps)
	registerGetPedagogicalSnapshots(server, deps)
	registerGetDecisionReplaySummary(server, deps)
	registerFeynmanChallenge(server, deps)
	registerTransferChallenge(server, deps)
	registerRecordTransferResult(server, deps)
	registerLearningNegotiation(server, deps)
	registerSetDomainPriority(server, deps)
	registerUpdateLearnerMemory(server, deps)
	registerReadRawSession(server, deps)
	registerGetMemoryState(server, deps)
	registerArchiveDomain(server, deps)
	registerUnarchiveDomain(server, deps)
	registerDeleteDomain(server, deps)
	registerGetMisconceptions(server, deps)
	registerRecordSessionClose(server, deps)
	registerQueueWebhookMessage(server, deps)
	registerGetDashboardState(server, deps)
	// [1] GoalDecomposer — gated by REGULATION_GOAL=on. When off, neither
	// tool is registered, so the surface is invisible to the LLM and the
	// system prompt has no instruction to call them.
	registerSetGoalRelevance(server, deps)
	registerGetGoalRelevance(server, deps)
	RegisterPrompt(server)
}
