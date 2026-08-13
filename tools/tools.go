// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	"tutor-mcp/observability"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Deps holds shared dependencies for all MCP tool handlers.
type Deps struct {
	Store               storeport.Store
	Logger              *slog.Logger
	BaseURL             string
	OAuthGranularScopes bool
}

var toolRegistrationOAuthModes sync.Map // map[*mcp.Server]bool; populated only during RegisterTools

var readOnlyTools = map[string]bool{
	"get_pending_alerts":             true,
	"check_mastery":                  true,
	"get_learner_context":            true,
	"get_availability_model":         true,
	"get_olm_snapshot":               true,
	"get_pedagogical_snapshots":      true,
	"get_decision_replay_summary":    true,
	"validate_domain_graph":          true,
	"get_autonomy_metrics":           true,
	"feynman_challenge":              true,
	"transfer_challenge":             true,
	"read_raw_session":               true,
	"get_misconceptions":             true,
	"list_implementation_intentions": true,
	"get_dashboard_state":            true,
	"get_goal_relevance":             true,
}

// readWriteTools contains read-oriented tools whose handlers also persist
// learner state or enqueue external work. A write-only grant must not expose
// their primary read payload, while a read-only grant must not authorize their
// side effects, so these tools require both granular capabilities.
var readWriteTools = map[string]bool{
	"get_curriculum_snapshot":  true,
	"get_memory_state":         true,
	"get_next_activity":        true,
	"get_metacognitive_mirror": true,
}

var writeTools = map[string]bool{
	"add_concepts":                    true,
	"archive_domain":                  true,
	"calibration_check":               true,
	"cancel_assessment_attempt":       true,
	"delete_domain":                   true,
	"init_domain":                     true,
	"learning_negotiation":            true,
	"mark_domain_high_stakes":         true,
	"prepare_assessment_attempt":      true,
	"publish_curriculum_revision":     true,
	"queue_webhook_message":           true,
	"record_affect":                   true,
	"record_calibration_result":       true,
	"record_interaction":              true,
	"record_session_close":            true,
	"record_transfer_result":          true,
	"set_domain_priority":             true,
	"set_goal_relevance":              true,
	"start_learning_session":          true,
	"submit_assessment_attempt":       true,
	"unarchive_domain":                true,
	"update_availability_model":       true,
	"update_implementation_intention": true,
	"update_learner_memory":           true,
	"update_learner_profile":          true,
}

// additiveWriteTools contains mutations that create or append state without
// replacing/removing existing learner state. All other writes are marked
// destructive conservatively so hosts can put an approval boundary around
// overwrites, lifecycle transitions, and deletions.
var additiveWriteTools = map[string]bool{
	"start_learning_session":     true,
	"get_next_activity":          true,
	"get_curriculum_snapshot":    true,
	"get_memory_state":           true,
	"record_interaction":         true,
	"prepare_assessment_attempt": true,
	"init_domain":                true,
	"add_concepts":               true,
	"record_affect":              true,
	"calibration_check":          true,
	"record_calibration_result":  true,
	"record_transfer_result":     true,
	"unarchive_domain":           true,
	"get_metacognitive_mirror":   true,
}

// openWorldTools includes tools that directly enqueue work intended for an
// external webhook destination, even when the network dispatch itself happens
// asynchronously in the scheduler.
var openWorldTools = map[string]bool{
	"get_next_activity":        true,
	"get_metacognitive_mirror": true,
	"queue_webhook_message":    true,
}

func boolHint(v bool) *bool { return &v }

func requiredOAuthScopesForTool(name string) ([]string, bool) {
	if readWriteTools[name] {
		return []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite}, true
	}
	if readOnlyTools[name] {
		return []string{models.OAuthScopeLearnerRead}, true
	}
	if writeTools[name] {
		return []string{models.OAuthScopeLearnerWrite}, true
	}
	return nil, false
}

func oauthToolAvailable(name string) bool {
	switch name {
	case "set_goal_relevance", "get_goal_relevance":
		return regulationGoalEnabled()
	default:
		return true
	}
}

func hasRequiredOAuthScopes(ctx context.Context, required []string) bool {
	for _, scope := range required {
		if !auth.HasOAuthScope(ctx, scope) {
			return false
		}
	}
	return true
}

// addTool is the single registration boundary for authorization and safety
// metadata. ChatGPT consumes the securitySchemes compatibility entry from
// _meta, while standard MCP clients consume ToolAnnotations.
func addTool[In, Out any](server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	readOnly := readOnlyTools[tool.Name]
	requiredScopes, known := requiredOAuthScopesForTool(tool.Name)
	if !known {
		panic("missing OAuth scope policy for MCP tool " + tool.Name)
	}
	granularScopes, _ := toolRegistrationOAuthModes.Load(server)
	granular, _ := granularScopes.(bool)
	advertisedScopes := requiredScopes
	if !granular {
		advertisedScopes = []string{models.OAuthScopeLearner}
	}
	destructive := !readOnly && !additiveWriteTools[tool.Name]
	openWorld := openWorldTools[tool.Name]
	tool.Annotations = &mcp.ToolAnnotations{
		ReadOnlyHint:    readOnly,
		DestructiveHint: boolHint(destructive),
		// MCP defines this hint as an unconditional guarantee for repeated calls
		// with identical arguments. Our replay protection is conditional on an
		// optional idempotency_key, so advertising true would make keyless retries
		// look safe when several mutations can still duplicate durable effects.
		IdempotentHint: false,
		OpenWorldHint:  boolHint(openWorld),
	}
	if tool.Meta == nil {
		tool.Meta = mcp.Meta{}
	}
	tool.Meta["securitySchemes"] = []map[string]any{{
		"type":   "oauth2",
		"scopes": advertisedScopes,
	}}
	scopedHandler := func(ctx context.Context, req *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		startedAt := time.Now()
		principal, _ := auth.GetPrincipal(ctx)
		outcome := "succeeded"
		defer func() {
			observability.RecordMCPTool(ctx, principal.TenantID, principal.MembershipID,
				"", tool.Name, outcome, time.Since(startedAt))
		}()
		// Leave missing authentication to the handler's existing canonical path,
		// but fail closed when a principal exists without the capability required
		// by this exact tool. The bounded legacy learner grant is accepted by
		// OAuthScopeAllows and cannot silently cover future scope families.
		if auth.GetLearnerID(ctx) != "" && !hasRequiredOAuthScopes(ctx, requiredScopes) {
			outcome = "denied"
			result := insufficientOAuthScopeResult(ctx, "", requiredScopes, granular)
			return result, zero, nil
		}
		result, output, err := handler(ctx, req, input)
		if err != nil || (result != nil && result.IsError) {
			outcome = "failed"
		}
		return result, output, err
	}
	mcp.AddTool(server, tool, scopedHandler)
}

// toolOAuthScopeMiddleware is installed after the idempotency middleware, so
// it executes first and an unauthorized mutation cannot reserve a replay key.
// addTool repeats the check because many package-level tests and embedders
// register one tool directly instead of calling RegisterTools.
func toolOAuthScopeMiddleware(baseURL string, granular bool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" || auth.GetLearnerID(ctx) == "" {
				return next(ctx, method, req)
			}
			call, ok := req.(*mcp.CallToolRequest)
			if !ok || call.Params == nil {
				return next(ctx, method, req)
			}
			requiredScopes, known := requiredOAuthScopesForTool(call.Params.Name)
			if !known || !oauthToolAvailable(call.Params.Name) {
				return next(ctx, method, req)
			}
			if hasRequiredOAuthScopes(ctx, requiredScopes) {
				return next(ctx, method, req)
			}
			return insufficientOAuthScopeResult(ctx, baseURL, requiredScopes, granular), nil
		}
	}
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
	deps.Logger.Info(tool+": auth failed", "error_type", fmt.Sprintf("%T", err))
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
			return nil, fmt.Errorf("domain not found: %w", storeport.ErrNotFound)
		}
		if d.Archived {
			return nil, fmt.Errorf("domain not found: %w", storeport.ErrNotFound)
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

// safeErrorResult logs only the underlying error class and returns an
// errorResult carrying the public, LLM-facing message. Raw database errors can
// contain learner values, filesystem paths, SQL details, or credentials, so
// they belong in an explicitly protected diagnostic sink rather than ordinary
// application logs.
func safeErrorResult(logger *slog.Logger, publicMsg string, err error) (*mcp.CallToolResult, error) {
	if logger != nil {
		logger.Error(publicMsg, "error_type", fmt.Sprintf("%T", err))
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
	granularScopes := deps != nil && deps.OAuthGranularScopes
	toolRegistrationOAuthModes.Store(server, granularScopes)
	defer toolRegistrationOAuthModes.Delete(server)
	addIdempotencyMiddleware(server, deps)
	registerStartLearningSession(server, deps)
	registerGetPendingAlerts(server, deps)
	registerGetNextActivity(server, deps)
	registerRecordInteraction(server, deps)
	registerPrepareAssessmentAttempt(server, deps)
	registerSubmitAssessmentAttempt(server, deps)
	registerCancelAssessmentAttempt(server, deps)
	registerCheckMastery(server, deps)
	registerGetLearnerContext(server, deps)
	registerGetAvailabilityModel(server, deps)
	registerUpdateAvailabilityModel(server, deps)
	registerInitDomain(server, deps)
	registerAddConcepts(server, deps)
	registerGetCurriculumSnapshot(server, deps)
	registerReviseCurriculum(server, deps)
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
	registerMarkDomainHighStakes(server, deps)
	registerUpdateLearnerMemory(server, deps)
	registerReadRawSession(server, deps)
	registerGetMemoryState(server, deps)
	registerArchiveDomain(server, deps)
	registerUnarchiveDomain(server, deps)
	registerDeleteDomain(server, deps)
	registerGetMisconceptions(server, deps)
	registerRecordSessionClose(server, deps)
	registerListImplementationIntentions(server, deps)
	registerUpdateImplementationIntention(server, deps)
	registerQueueWebhookMessage(server, deps)
	registerGetDashboardState(server, deps)
	// [1] GoalDecomposer — gated by REGULATION_GOAL=on. When off, neither
	// tool is registered, so the surface is invisible to the LLM and the
	// system prompt has no instruction to call them.
	registerSetGoalRelevance(server, deps)
	registerGetGoalRelevance(server, deps)
	RegisterPrompt(server)
	baseURL := ""
	if deps != nil {
		baseURL = deps.BaseURL
	}
	server.AddReceivingMiddleware(
		toolOAuthScopeMiddleware(baseURL, granularScopes),
		toolTenantTransactionMiddleware(deps),
	)
}

func toolTenantTransactionMiddleware(deps *Deps) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" || deps == nil || deps.Store == nil {
				return next(ctx, method, req)
			}
			principal, ok := auth.GetPrincipal(ctx)
			if !ok {
				result, _ := errorResult("authenticated tenant principal is required")
				return result, nil
			}
			if !principal.Authorize(models.PermissionLearningSelf, models.AuthorizationResource{
				TenantID: principal.TenantID, OwnerUserID: principal.UserID,
			}) {
				result, _ := errorResult("tenant principal is not authorized for learner tools")
				return result, nil
			}
			var result mcp.Result
			var callErr error
			err := deps.Store.WithTenantTx(ctx, principal.TenantScope(), func(txCtx context.Context, _ storeport.Store) error {
				result, callErr = next(txCtx, method, req)
				return callErr
			})
			if err != nil {
				if callErr != nil {
					return nil, callErr
				}
				if deps.Logger != nil {
					deps.Logger.Error("tenant tool transaction failed", "err", err, "tool", toolNameFromRequest(req))
				}
				failure, _ := errorResult("tenant-scoped persistence failed")
				return failure, nil
			}
			return result, nil
		}
	}
}

func toolNameFromRequest(req mcp.Request) string {
	if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
		return call.Params.Name
	}
	return ""
}
