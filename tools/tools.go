// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/engine"
	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"modernc.org/sqlite"
)

// Deps holds shared dependencies for all MCP tool handlers.
type Deps struct {
	Institution         bool
	LocalStdio          bool
	Store               storeport.Store
	Logger              *slog.Logger
	BaseURL             string
	OAuthGranularScopes bool
	// Zero leaves direct RegisterTools embedders without a server deadline.
	ToolCallTimeout time.Duration
}

var toolRegistrationLocalModes sync.Map

var toolRegistrationOAuthModes sync.Map // map[*mcp.Server]bool; populated only during RegisterTools

var toolRegistrationNames sync.Map // map[*mcp.Server]map[string]struct{}; registration only

var readOnlyTools = map[string]bool{
	"get_conversation_context":       true,
	"get_training_groups":            true,
	"list_formations":                true,
	"list_formation_versions":        true,
	"review_formation_version":       true,
	"get_my_badges":                  true,
	"get_learner_badges":             true,
	"list_trainer_cohorts":           true,
	"get_cohort_insights":            true,
	"get_cohort_statistics":          true,
	"get_learner_progress":           true,
	"list_available_formations":      true,
	"get_my_formations":              true,
	"get_learner_formation":          true,
	"get_formation_version":          true,
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
	// trainingPolicy only reads tenants.policy_json when it is not setting it.
	"get_training_policy": true,
}

// readWriteTools contains read-oriented tools whose handlers also persist
// learner state or enqueue external work. A write-only grant must not expose
// their primary read payload, while a read-only grant must not authorize their
// side effects, so these tools require both granular capabilities.
//
// Training reads are not listed: their scope comes from
// models.TrainingReadAction (see requiredOAuthScopesForTool).
var readWriteTools = map[string]bool{
	"claim_memory_maintenance":  true,
	"commit_memory_maintenance": true,
	"prepare_pedagogical_turn":  true,
	"get_curriculum_snapshot":   true,
	"get_memory_state":          true,
	"get_next_activity":         true,
	"get_metacognitive_mirror":  true,
}

// toolReadOnlyHint is the advertised MCP readOnlyHint. It differs from
// readOnlyTools, which selects a read-only database transaction: training
// reads (get_training, list_training_library, preview_training_edit,
// preview_training_lifecycle, find_training_members) change nothing the caller
// can observe and need only a read grant, but they may materialize the
// immutable curriculum baseline of a legacy personal domain or take a
// PostgreSQL row lock, so they keep a write transaction.
func toolReadOnlyHint(name string) bool {
	return readOnlyTools[name] || models.TrainingReadAction(name)
}

var writeTools = map[string]bool{
	"archive_training": true, "restore_training": true, "delete_training": true, "withdraw_training": true,
	"select_conversation_training": true, "save_training_edit": true, "propose_training_edit": true, "share_training": true, "submit_training": true, "decide_training": true, "publish_training": true, "set_training_access": true, "set_training_policy": true, "join_training": true,
	"publish_formation_to_catalog":    true,
	"join_formation":                  true,
	"leave_formation":                 true,
	"draft_formation":                 true,
	"add_formation_concepts":          true,
	"publish_formation":               true,
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
	"record_learning_event":           true,
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
	"prepare_pedagogical_turn":   true,
	"draft_formation":            true,
	"start_learning_session":     true,
	"get_next_activity":          true,
	"get_curriculum_snapshot":    true,
	"get_memory_state":           true,
	"record_interaction":         true,
	"record_learning_event":      true,
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
	"prepare_pedagogical_turn": true,
	"get_next_activity":        true,
	"get_metacognitive_mirror": true,
	"queue_webhook_message":    true,
}

func boolHint(v bool) *bool { return &v }

func requiredOAuthScopesForTool(name string) ([]string, bool) {
	if name == "prepare_pedagogical_turn" {
		// Discovery advertises the minimum capability. Activity-mode requests
		// additionally require write at both HTTP and MCP invocation boundaries.
		return []string{models.OAuthScopeLearnerRead}, true
	}
	if name == "get_training_groups" {
		return []string{models.OAuthScopeProgressRead}, true
	}
	if contributionTool(name) {
		return []string{models.OAuthScopeFormationContribute}, true
	}
	if models.ValidTrainingAction(name) {
		if models.TrainingReadAction(name) {
			return []string{models.OAuthScopeLearnerRead}, true
		}
		return []string{models.OAuthScopeLearnerWrite}, true
	}
	if progressTool(name) {
		return []string{models.OAuthScopeProgressRead}, true
	}
	if formationTool(name) {
		if name == "get_formation_version" || name == "list_formations" || name == "list_formation_versions" || name == "review_formation_version" {
			return []string{models.OAuthScopeFormationRead}, true
		}
		return []string{models.OAuthScopeFormationWrite}, true
	}
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

func hasToolOAuthScopes(ctx context.Context, name string, required []string) bool {
	return hasRequiredOAuthScopes(ctx, required) || (contributionCompatibleTool(name) && auth.HasOAuthScope(ctx, models.OAuthScopeFormationContribute))
}

// addTool is the single registration boundary for authorization and safety
// metadata. ChatGPT consumes the securitySchemes compatibility entry from
// _meta, while standard MCP clients consume ToolAnnotations.
// toolInputEnums pins the exact values a tool's string input accepts. The store
// already rejects everything else, but a jsonschema struct tag only becomes the
// property description, and a description is advice the caller is free to
// ignore: a recorded session shows a client inventing "diagnostic_feedback"
// four times while the description named the two real kinds. Listing them in
// the schema makes the same constraint part of what the client validates.
//
// Only inputs whose handler rejects every other value are listed, so no call
// that used to succeed can fail schema validation. An optional field whose
// empty string is accepted lists "" explicitly. Inputs normalized leniently by
// their handler (get_next_activity.intent, transfer context types, OLM scope)
// are deliberately absent, as are inputs whose handler answers with a more
// specific explanation than a schema mismatch (record_interaction's
// activity_type/error_type self-correction message, curriculum review status).
// Keys are property paths: "a.b" descends into an object, "a[]" into array
// items and "a{}" into map values.
var toolInputEnums = map[string]map[string][]any{
	"record_learning_event":      {"kind": {"feedback", "instruction"}},
	"prepare_pedagogical_turn":   {"mode": {"context", "activity"}, "intent": {"auto", "review"}},
	"prepare_assessment_attempt": {"activity_type": enumValues(allowedActivityTypes...)},
	"update_learner_memory": {
		"scope":     enumValues(allowedMemoryScopes...),
		"operation": enumValues(append([]string{""}, allowedMemoryOperations...)...),
	},
	"update_implementation_intention": {"status": enumValues(models.IntentionStatusHonored, models.IntentionStatusMissed, models.IntentionStatusCancelled)},
	"list_implementation_intentions":  {"status": enumValues("", models.IntentionStatusPending, models.IntentionStatusHonored, models.IntentionStatusMissed, models.IntentionStatusCancelled)},
	"learning_negotiation":            {"activity_type": enumValues(append([]string{""}, allowedLearningNegotiationActivityTypes...)...)},
	"publish_curriculum_revision": {
		"operation": enumValues(
			string(models.CurriculumOperationRename), string(models.CurriculumOperationUpdateMetadata),
			string(models.CurriculumOperationSplit), string(models.CurriculumOperationMerge),
			string(models.CurriculumOperationRemove), string(models.CurriculumOperationRepairPrerequisites),
		),
		"metadata.level":       curriculumLevelEnum,
		"new_concepts[].level": curriculumLevelEnum,
	},
	"init_domain":                {"details{}.level": curriculumLevelEnum},
	"add_concepts":               {"details{}.level": curriculumLevelEnum},
	"list_training_library":      {"filter": enumValues("", "personal", "institutional", "shared", "mine", "review", "archived")},
	"share_training":             {"audience": trainingAudienceEnum},
	"set_training_access":        {"audience": trainingAudienceEnum},
	"decide_training":            {"decision": enumValues("approve", "changes_requested")},
	"preview_training_lifecycle": {"decision": enumValues("archive_training", "restore_training", "delete_training", "withdraw_training")},
}

var curriculumLevelEnum = enumValues("", string(models.CurriculumLevelUnspecified), string(models.CurriculumLevelFoundation), string(models.CurriculumLevelIntermediate), string(models.CurriculumLevelAdvanced))

var trainingAudienceEnum = enumValues("", "open", "invitation", "approval")

func enumValues(values ...string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// schemaPropertyAt resolves a toolInputEnums path inside an object schema.
func schemaPropertyAt(schema *jsonschema.Schema, path string) *jsonschema.Schema {
	current := schema
	for _, segment := range strings.Split(path, ".") {
		if current == nil {
			return nil
		}
		name, items := strings.CutSuffix(segment, "[]")
		name, values := strings.CutSuffix(name, "{}")
		current = current.Properties[name]
		switch {
		case current == nil:
			return nil
		case items:
			current = current.Items
		case values:
			current = current.AdditionalProperties
		}
	}
	return current
}

// toolInputStringForms names inputs declared as an object that also accept a
// plain sentence, decoded by the type's own UnmarshalJSON. The schema has to
// advertise both shapes, because the SDK validates arguments against it before
// any decoding happens.
var toolInputStringForms = map[string][]string{
	"record_session_close": {"implementation_intention"},
}

// constrainInputSchema infers the schema the SDK would have inferred anyway and
// adds the enums and alternate shapes above, leaving every tool without an
// entry untouched.
func constrainInputSchema[In any](tool *mcp.Tool) {
	enums, hasEnums := toolInputEnums[tool.Name]
	stringForms, hasStringForms := toolInputStringForms[tool.Name]
	if !hasEnums && !hasStringForms {
		return
	}
	if tool.InputSchema != nil {
		// Hand-built schemas (training, prepare_assessment_attempt) receive
		// the same enums; alternate string shapes stay an inferred-only feature.
		explicit, ok := tool.InputSchema.(*jsonschema.Schema)
		if !ok {
			return
		}
		applyInputEnums(tool.Name, explicit, enums)
		return
	}
	schema, err := jsonschema.For[In](&jsonschema.ForOptions{})
	if err != nil {
		panic("cannot infer the input schema of MCP tool " + tool.Name + ": " + err.Error())
	}
	applyInputEnums(tool.Name, schema, enums)
	for _, field := range stringForms {
		property, ok := schema.Properties[field]
		if !ok {
			panic("MCP tool " + tool.Name + " has no " + field + " input to constrain")
		}
		object := *property
		object.Description = ""
		schema.Properties[field] = &jsonschema.Schema{
			Description: property.Description,
			AnyOf:       []*jsonschema.Schema{&object, {Type: "string"}},
		}
	}
	tool.InputSchema = schema
}

func applyInputEnums(toolName string, schema *jsonschema.Schema, enums map[string][]any) {
	for field, values := range enums {
		property := schemaPropertyAt(schema, field)
		if property == nil {
			panic("MCP tool " + toolName + " has no " + field + " input to constrain")
		}
		property.Enum = values
	}
}

func addTool[In, Out any](server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	readOnly := toolReadOnlyHint(tool.Name)
	requiredScopes, known := requiredOAuthScopesForTool(tool.Name)
	if !known {
		panic("missing OAuth scope policy for MCP tool " + tool.Name)
	}
	granularScopes, _ := toolRegistrationOAuthModes.Load(server)
	granular, _ := granularScopes.(bool)
	advertisedScopes := requiredScopes
	if !granular && !formationTool(tool.Name) && !progressTool(tool.Name) && !contributionTool(tool.Name) {
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
	if contributionCompatibleTool(tool.Name) {
		tool.Meta["securitySchemes"] = append(tool.Meta["securitySchemes"].([]map[string]any), map[string]any{"type": "oauth2", "scopes": []string{models.OAuthScopeFormationContribute}})
	}
	if local, _ := toolRegistrationLocalModes.Load(server); local == true {
		tool.Meta["securitySchemes"] = []map[string]any{{"type": "noauth"}}
	}
	scopedHandler := func(ctx context.Context, req *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		requestScopes, _, scopeErr := requiredOAuthScopesForCall(req)
		if scopeErr != nil {
			result, _ := errorResult(scopeErr.Error())
			return result, zero, nil
		}
		principal, _ := auth.GetPrincipal(ctx)
		// Leave missing authentication to the handler's existing canonical path,
		// but fail closed when a principal exists without the capability required
		// by this exact tool. The bounded legacy learner grant is accepted by
		// OAuthScopeAllows and cannot silently cover future scope families.
		if (principal.UserID != "" || auth.GetLearnerID(ctx) != "") && !hasToolOAuthScopes(ctx, tool.Name, requestScopes) {
			result := insufficientOAuthScopeResult(ctx, "", requestScopes, granular)
			markToolCallDenied(ctx, result)
			return result, zero, nil
		}
		return handler(ctx, req, input)
	}
	constrainInputSchema[In](tool)
	if names, ok := toolRegistrationNames.Load(server); ok {
		names.(map[string]struct{})[tool.Name] = struct{}{}
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
			if method == "tools/list" && auth.IsNativeClientSession(ctx) {
				result, err := next(ctx, method, req)
				if err != nil {
					return result, err
				}
				if list, ok := result.(*mcp.ListToolsResult); ok {
					copy := *list
					copy.Tools = authorizedNativeTools(ctx, list.Tools)
					return &copy, nil
				}
				return result, nil
			}
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			call, ok := req.(*mcp.CallToolRequest)
			if !ok || call.Params == nil {
				return next(ctx, method, req)
			}
			requiredScopes, known, scopeErr := requiredOAuthScopesForCall(call)
			if scopeErr != nil {
				result, _ := errorResult(scopeErr.Error())
				return result, nil
			}
			if !known || !oauthToolAvailable(call.Params.Name) {
				return next(ctx, method, req)
			}
			if hasToolOAuthScopes(ctx, call.Params.Name, requiredScopes) {
				return next(ctx, method, req)
			}
			result := insufficientOAuthScopeResult(ctx, baseURL, requiredScopes, granular)
			markToolCallDenied(ctx, result)
			return result, nil
		}
	}
}

func authorizedNativeTools(ctx context.Context, available []*mcp.Tool) []*mcp.Tool {
	visible := make([]*mcp.Tool, 0, len(available))
	actor, ok := auth.GetPrincipal(ctx)
	if !ok {
		return visible
	}
	for _, tool := range available {
		scopes, known := requiredOAuthScopesForTool(tool.Name)
		if !known || !hasToolOAuthScopes(ctx, tool.Name, scopes) {
			continue
		}
		if contributionTool(tool.Name) || (contributionCompatibleTool(tool.Name) && auth.HasOAuthScope(ctx, models.OAuthScopeFormationContribute)) {
			// The store checks ownership/assignment on each actual resource.
		} else if formationTool(tool.Name) {
			if !actor.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: actor.TenantID}) {
				continue
			}
		} else if progressTool(tool.Name) {
			if !actor.CanReadInstitutionProgress() {
				continue
			}
		} else if actor.LearnerID == "" || !actor.Authorize(models.PermissionLearningSelf, models.AuthorizationResource{TenantID: actor.TenantID, OwnerUserID: actor.UserID}) {
			continue
		}
		visible = append(visible, tool)
	}
	return visible
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

// domainLookupErrorResult reports a failed owner-scoped domain lookup. A
// missing domain and another learner's domain produce the same message, so the
// response never confirms that an identifier exists.
func domainLookupErrorResult(deps *Deps, err error) (*mcp.CallToolResult, error) {
	if errors.Is(err, storeport.ErrNotFound) {
		return errorResult("domain not found")
	}
	return safeErrorResult(deps.Logger, "failed to load domain", err)
}

// resolveDomain resolves a domain by ID or falls back to the learner's most recent domain.
//
// Archived domains are explicitly rejected when resolved by ID: see issue #94.
// Without this guard, callers like record_interaction would silently advance
// BKT/FSRS state on a domain the learner has explicitly archived. Archive-
// specific tools (archive_domain, unarchive_domain, delete_domain) do not go
// through resolveDomain — they call store.GetLearnerDomainByID directly because
// they legitimately need to operate on archived rows.
func resolveDomain(ctx context.Context, store storeport.Store, learnerID, domainID string) (*models.Domain, error) {
	if conversationID := storeport.ConversationID(ctx); conversationID != "" {
		p, ok := auth.GetPrincipal(ctx)
		training, available := store.(storeport.TrainingStore)
		if !ok || !available {
			return nil, fmt.Errorf("conversation context unavailable")
		}
		selected, err := training.TrainingAction(ctx, p, "get_conversation_context", models.TrainingRequest{ConversationID: conversationID})
		if err != nil {
			return nil, err
		}
		c, ok := selected.(*models.ConversationContext)
		if !ok || c.DomainID == "" {
			return nil, fmt.Errorf("select an enrolled or personal training for this conversation first")
		}
		if domainID != "" && domainID != c.DomainID {
			return nil, fmt.Errorf("domain differs from the selected conversation training; select it explicitly first")
		}
		domainID = c.DomainID
	}

	if domainID != "" {
		d, err := store.GetLearnerDomainByID(ctx, learnerID, domainID)
		if err != nil {
			return nil, err
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

// safeErrorResult returns an errorResult carrying the public, LLM-facing
// message. A store.ValidationError in the chain is a caller-correctable
// rejection written for the caller, so its message is appended verbatim
// ("<publicMsg>: <reason>") instead of being collapsed into publicMsg.
//
// The operator log keeps enough to diagnose a failing call: the error class
// and, unless the chain carries SQL driver output, a bounded copy of the
// message. Driver errors can quote learner values, SQL, connection strings or
// filesystem paths, so for them only the SQLSTATE (or SQLite result code) is
// logged.
func safeErrorResult(logger *slog.Logger, publicMsg string, err error) (*mcp.CallToolResult, error) {
	if reason, ok := storeport.ValidationMessage(err); ok {
		// The reason may quote the caller's own content: it is returned to
		// the caller but kept out of the log.
		if logger != nil {
			logger.Info(publicMsg, "error_type", fmt.Sprintf("%T", err), "error_class", "validation")
		}
		return errorResult(publicMsg + ": " + reason)
	}
	if logger != nil {
		logger.Error(publicMsg, diagnosticErrorAttrs(err)...)
	}
	return errorResult(publicMsg)
}

var sqlStateInText = regexp.MustCompile(`SQLSTATE ([0-9A-Z]{5})`)

// sqlDriverMarkers identify driver output that survived as plain text, for
// example after fmt.Errorf("%v", err) dropped the typed driver error.
var sqlDriverMarkers = []string{
	"sqlstate", "sqlite", "sql logic error", "constraint failed", "database is locked",
	"no such table", "no such column", "syntax error", "sql: ", "pgx", "pgconn",
	"failed to connect", "duplicate key value", "violates ", "relation \"",
}

// loggableSentinels have fixed messages that never embed request data, so
// their text may be logged when one is found in an error chain.
var loggableSentinels = []error{
	context.Canceled, context.DeadlineExceeded, sql.ErrNoRows, sql.ErrTxDone, sql.ErrConnDone,
	fs.ErrNotExist, fs.ErrExist, fs.ErrPermission, fs.ErrClosed,
	storeport.ErrNotFound, storeport.ErrLearningSessionClosed, storeport.ErrInvalidPrincipal,
	storeport.ErrInvalidOAuthScope, storeport.ErrCurriculumVersionConflict, storeport.ErrDomainPhaseConflict,
	storeport.ErrIdempotencyKeyConflict, storeport.ErrIdempotencyInProgress, storeport.ErrIdempotencyResponseExpired,
	storeport.ErrAvailabilityVersionConflict, storeport.ErrCohortCapacityReached, storeport.ErrWebhookQueueFull,
	storeport.ErrFormationVersionImmutable, storeport.ErrFormationDomainLocked, storeport.ErrFormationAdmissionRequired,
	storeport.ErrFormationUnavailable, storeport.ErrEnrollmentTransition, storeport.ErrInvalidProgressRequest,
	storeport.ErrProgressUnavailable, storeport.ErrTenantDSARBlocked, storeport.ErrAmbiguousIdentity,
	storeport.ErrInvalidAssessmentReviewRequest, storeport.ErrAssessmentReviewMaterialUnavailable,
	storeport.ErrAssessmentReviewConflict, storeport.ErrAssessmentReviewMaterialChanged,
	storeport.ErrInvalidAssessmentReviewScore,
	memory.ErrQuotaExceeded, memory.ErrNarrativeNotFound, memory.ErrNarrativeVersionConflict,
	memory.ErrNarrativeCorrupt, memory.ErrNarrativeMutationConflict, memory.ErrNarrativeBackfillConflict,
	memory.ErrSessionIndexIncomplete, memory.ErrNarrativeTenantScope,
	engine.ErrUnknownDomain, engine.ErrUnknownPhase, engine.ErrGateUnknownPhase, engine.ErrInvalidGateResult,
}

// diagnosticErrorAttrs returns slog attributes describing err without
// copying free text into the application log. Error messages can quote
// learner content, concept labels, identifiers, file paths or SQL, so only
// typed codes and fixed sentinel messages are logged.
func diagnosticErrorAttrs(err error) []any {
	attrs := []any{"error_type", fmt.Sprintf("%T", err)}
	if err == nil {
		return attrs
	}
	if _, ok := storeport.ValidationMessage(err); ok {
		return append(attrs, "error_class", "validation")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return append(attrs, "sqlstate", pgErr.Code)
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return append(attrs, "sqlite_code", sqliteErr.Code())
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return attrs
	}
	if message := err.Error(); containsSQLDriverMarker(message) {
		if code := sqlStateInText.FindStringSubmatch(message); code != nil {
			attrs = append(attrs, "sqlstate", code[1])
		}
		return attrs
	}
	if op, cause, ok := osErrorParts(err); ok {
		attrs = append(attrs, "os_op", op)
		var errno syscall.Errno
		if errors.As(cause, &errno) {
			return append(attrs, "errno", errno.Error())
		}
		return append(attrs, "cause_type", fmt.Sprintf("%T", cause))
	}
	for _, sentinel := range loggableSentinels {
		if errors.Is(err, sentinel) {
			return append(attrs, "sentinel", sentinel.Error())
		}
	}
	if root := rootError(err); root != err {
		attrs = append(attrs, "root_type", fmt.Sprintf("%T", root))
	}
	return attrs
}

func containsSQLDriverMarker(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range sqlDriverMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// osErrorParts extracts the operation and cause of an operating-system error
// without its path arguments.
func osErrorParts(err error) (string, error, bool) {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Op, pathErr.Err, true
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Op, linkErr.Err, true
	}
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) {
		return syscallErr.Syscall, syscallErr.Err, true
	}
	return "", nil, false
}

// rootError follows single-error wrapping to the innermost error.
func rootError(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}

// invalidInput marks a local validation error as safe to return verbatim.
func invalidInput(err error) error {
	if err == nil {
		return nil
	}
	return &storeport.ValidationError{Message: err.Error()}
}

// resultErrorText returns the caller-facing text of a failed tool result.
func resultErrorText(result *mcp.CallToolResult) string {
	if result == nil || !result.IsError {
		return ""
	}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

// noActiveDomainResult returns the canonical "no active domain" payload that
// every chat-side tool emits when called without an explicit DomainID and the
// learner has no domain yet. Issue #33: a uniform shape lets the LLM branch
// on `needs_domain_setup:true` regardless of which tool it called and recover
// by issuing `init_domain`. For an explicit DomainID that does not match the
// learner, callers should keep emitting errorResult("domain not found") —
// that's a genuine dev-facing 404, not a setup precondition.
func noActiveDomainResult(deps *Deps) (*mcp.CallToolResult, any) {
	payload := map[string]any{
		"needs_domain_setup":  true,
		"reason":              "no active domain for this learner",
		"next_action_for_llm": domainSetupInstruction(deps),
	}
	r, _ := jsonResult(payload)
	return r, payload
}

// Institution learners choose an offered cohort instead of trying to create
// a free domain, which their institution normally disables.
func domainSetupInstruction(deps *Deps) string {
	if deps != nil && deps.Institution {
		return "Call get_my_formations or list_available_formations, help the learner choose a cohort, then call join_formation only with their agreement. Resume using the returned domain_id."
	}
	return "Call init_domain(name, concepts, prerequisites) from the learner's goal, then resume with the returned domain_id."
}

// RegisterTools registers all MCP tools and prompts on the given server.
func RegisterTools(server *mcp.Server, deps *Deps) {
	names := make(map[string]struct{})
	toolRegistrationNames.Store(server, names)
	defer toolRegistrationNames.Delete(server)
	granularScopes := deps != nil && deps.OAuthGranularScopes
	toolRegistrationLocalModes.Store(server, deps != nil && deps.LocalStdio)
	defer toolRegistrationLocalModes.Delete(server)
	toolRegistrationOAuthModes.Store(server, granularScopes)
	defer toolRegistrationOAuthModes.Delete(server)
	addIdempotencyMiddleware(server, deps)
	registerTrainingTools(server, deps)
	if deps != nil && deps.Institution {
		registerFormationTools(server, deps)
		registerProgressTools(server, deps)
		registerLearnerFormationTools(server, deps)
		registerBadgeTools(server, deps)
	}
	registerStartLearningSession(server, deps)
	registerGetPendingAlerts(server, deps)
	registerGetNextActivity(server, deps)
	registerPreparePedagogicalTurn(server, deps)
	registerRecordInteraction(server, deps)
	registerRecordLearningEvent(server, deps)
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
	registerClaimMemoryMaintenance(server, deps)
	registerCommitMemoryMaintenance(server, deps)
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
		conversationBindingMiddleware(),
		toolOAuthScopeMiddleware(baseURL, granularScopes),
		toolTenantTransactionMiddleware(deps),
	)
	if deps != nil && deps.ToolCallTimeout != 0 {
		server.AddReceivingMiddleware(ToolCallDeadlineMiddleware(deps.ToolCallTimeout))
	}
	server.AddReceivingMiddleware(ToolPolicyMiddleware())
	var observationLogger *slog.Logger
	if deps != nil {
		observationLogger = deps.Logger
	}
	server.AddReceivingMiddleware(toolCallObservationMiddleware(names, observationLogger))
}

var errToolApplicationFailure = errors.New("MCP tool returned an application error")

func toolTenantTransactionMiddleware(deps *Deps) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" || deps == nil || deps.Store == nil {
				return next(ctx, method, req)
			}
			principal, ok := auth.GetPrincipal(ctx)
			if !ok {
				result, _ := errorResult("authenticated tenant principal is required")
				markToolCallDenied(ctx, result)
				return result, nil
			}
			permission := models.PermissionLearningSelf
			if formationTool(toolNameFromRequest(req)) {
				permission = models.PermissionFormationWrite
			}
			isProgress := progressTool(toolNameFromRequest(req))
			if isProgress && !principal.CanReadInstitutionProgress() {
				result, _, _ := progressToolResult(deps, nil, storeport.ErrInvalidPrincipal)
				markToolCallDenied(ctx, result)
				return result, nil
			}
			if !isProgress && !models.ValidTrainingAction(toolNameFromRequest(req)) && !principal.Authorize(permission, models.AuthorizationResource{TenantID: principal.TenantID, OwnerUserID: principal.UserID}) {
				result, _ := errorResult("tenant principal is not authorized for this tool")
				markToolCallDenied(ctx, result)
				return result, nil
			}
			var result mcp.Result
			var callErr error
			readOnly, modeErr := readOnlyToolCall(req)
			if modeErr != nil {
				failure, _ := errorResult(modeErr.Error())
				return failure, nil
			}
			transaction := deps.Store.WithTenantTx
			if readOnly {
				transaction = deps.Store.WithTenantReadTx
			}
			err := transaction(ctx, principal.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
				// HTTP authentication can precede a concurrent membership
				// revocation. Recheck within the transaction that owns the effect;
				// PostgreSQL writers keep the membership locked until commit.
				if err := scoped.ValidatePrincipal(txCtx, principal); err != nil {
					return err
				}
				result, callErr = next(txCtx, method, req)
				// MCP application errors normally carry a nil Go error. Nested
				// store operations share this transaction, so only this boundary
				// can roll back their partial writes and idempotency reservation.
				if toolResult, ok := result.(*mcp.CallToolResult); callErr == nil && ok && toolResult != nil && toolResult.IsError {
					return errToolApplicationFailure
				}
				return callErr
			})
			if err != nil {
				if errors.Is(err, storeport.ErrInvalidPrincipal) {
					failure, _ := errorResult("authenticated tenant principal is no longer valid")
					markToolCallDenied(ctx, failure)
					return failure, nil
				}
				if callErr != nil {
					return nil, callErr
				}
				if errors.Is(err, errToolApplicationFailure) {
					return result, nil
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
