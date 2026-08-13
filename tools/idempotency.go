// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const idempotencyMetaKey = "idempotency_key"

type idempotencyContextKey struct{}

func idempotencyKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyContextKey{}).(string)
	return key
}

// IdempotentMutationParams is embedded by write-tool inputs so hosts that
// cannot set MCP request metadata can still opt into replay protection. A
// transport may equivalently send the key in `_meta.idempotency_key`.
type IdempotentMutationParams struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"optional retry key (1..128 safe characters); reuse only for an exact retry of the same mutation"`
}

var validIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// idempotentWriteTools is intentionally explicit. Read-only calls ignore the
// metadata, while every tool here can otherwise duplicate a durable side
// effect when an MCP transport retries after an ambiguous disconnect.
var idempotentWriteTools = map[string]bool{
	"start_learning_session":          true,
	"get_next_activity":               true, // opens/touches a session and may enqueue a mirror
	"record_interaction":              true,
	"prepare_assessment_attempt":      true,
	"submit_assessment_attempt":       true,
	"cancel_assessment_attempt":       true,
	"init_domain":                     true,
	"add_concepts":                    true,
	"update_learner_profile":          true,
	"record_affect":                   true,
	"calibration_check":               true,
	"record_calibration_result":       true,
	"learning_negotiation":            true,
	"set_domain_priority":             true,
	"update_learner_memory":           true,
	"archive_domain":                  true,
	"unarchive_domain":                true,
	"delete_domain":                   true,
	"record_session_close":            true,
	"update_implementation_intention": true,
	"queue_webhook_message":           true,
	"set_goal_relevance":              true,
	"record_transfer_result":          true,
	"update_availability_model":       true,
	"mark_domain_high_stakes":         true,
	"publish_curriculum_revision":     true,
}

func addIdempotencyMiddleware(server *mcp.Server, deps *Deps) {
	server.AddReceivingMiddleware(idempotencyMiddleware(deps))
}

func idempotencyMiddleware(deps *Deps) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" || deps == nil || deps.Store == nil {
				return next(ctx, method, req)
			}
			call, ok := req.(*mcp.CallToolRequest)
			if !ok || call.Params == nil || !idempotentWriteTools[call.Params.Name] {
				return next(ctx, method, req)
			}
			key, exists, keyErr := requestIdempotencyKey(call.Params)
			if !exists {
				return next(ctx, method, req)
			}
			if keyErr != nil || !validIdempotencyKey.MatchString(key) {
				result, _ := errorResult("idempotency_key must match in arguments and _meta and use 1..128 letters, digits, '.', '_', ':', or '-'")
				return result, nil
			}
			learnerID, err := getLearnerID(ctx)
			if err != nil {
				// Preserve the canonical authentication error/logging path.
				return next(ctx, method, req)
			}
			requestHash := canonicalToolRequestHash(call.Params.Name, call.Params.Arguments)
			cached, execute, err := deps.Store.ClaimIdempotencyKey(
				ctx, learnerID, call.Params.Name, key, requestHash, time.Now().UTC())
			switch {
			case errors.Is(err, storeport.ErrIdempotencyKeyConflict):
				result, _ := errorResult("idempotency_key was already used with different arguments")
				return result, nil
			case errors.Is(err, storeport.ErrIdempotencyInProgress):
				result, _ := errorResult("a request with this idempotency_key is already in progress; retry later")
				return result, nil
			case errors.Is(err, storeport.ErrIdempotencyResponseExpired):
				result, _ := errorResult("mutation already completed; cached response expired; the operation will not be executed again")
				return result, nil
			case err != nil:
				result, _ := safeErrorResult(deps.Logger, "failed to reserve idempotent tool call", err)
				return result, nil
			case !execute:
				return cachedToolCallResult(cached), nil
			}

			ctx = context.WithValue(ctx, idempotencyContextKey{}, key)
			result, callErr := next(ctx, method, req)
			toolResult, isToolResult := result.(*mcp.CallToolResult)
			if callErr != nil || !isToolResult || toolResult == nil || toolResult.IsError {
				if abortErr := deps.Store.AbortIdempotencyKey(ctx, learnerID, call.Params.Name, key, requestHash); abortErr != nil && deps.Logger != nil {
					deps.Logger.Error("abort idempotency reservation", "err", abortErr, "tool", call.Params.Name, "learner", learnerID)
				}
				return result, callErr
			}

			responseText := firstToolResultText(toolResult)
			if responseText == "" {
				responseText = `{"completed":true,"response_not_cacheable":true}`
			}
			if completeErr := deps.Store.CompleteIdempotencyKey(
				ctx, learnerID, call.Params.Name, key, requestHash, responseText, time.Now().UTC(),
			); completeErr != nil && deps.Logger != nil {
				// Never release an ambiguous successful write: leaving the claim in
				// processing sacrifices availability but prevents a duplicate.
				deps.Logger.Error("complete idempotency reservation", "err", completeErr, "tool", call.Params.Name, "learner", learnerID)
			}
			return result, nil
		}
	}
}

func requestIdempotencyKey(params *mcp.CallToolParamsRaw) (string, bool, error) {
	if params == nil {
		return "", false, nil
	}
	var metaKey string
	metaPresent := false
	if raw, ok := params.GetMeta()[idempotencyMetaKey]; ok {
		metaPresent = true
		var valid bool
		metaKey, valid = raw.(string)
		if !valid {
			return "", true, errors.New("metadata idempotency key is not a string")
		}
	}
	var body map[string]any
	bodyPresent := false
	bodyKey := ""
	if len(bytes.TrimSpace(params.Arguments)) > 0 && json.Unmarshal(params.Arguments, &body) == nil {
		if raw, ok := body[idempotencyMetaKey]; ok {
			bodyPresent = true
			var valid bool
			bodyKey, valid = raw.(string)
			if !valid {
				return "", true, errors.New("argument idempotency key is not a string")
			}
		}
	}
	if metaPresent && bodyPresent && metaKey != bodyKey {
		return "", true, errors.New("metadata and argument idempotency keys differ")
	}
	if metaPresent {
		return metaKey, true, nil
	}
	if bodyPresent {
		return bodyKey, true, nil
	}
	return "", false, nil
}

func canonicalToolRequestHash(toolName string, raw json.RawMessage) string {
	canonical := bytes.TrimSpace(raw)
	var value any
	if len(canonical) == 0 {
		canonical = []byte(`{}`)
	} else if decoder := json.NewDecoder(bytes.NewReader(canonical)); json.Valid(canonical) {
		// Preserve arbitrary-precision JSON integers. Decoding into interface{}
		// with json.Unmarshal coerces every number to float64, making distinct
		// values above 2^53 collide in the idempotency request hash.
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			value = nil
		}
	}
	if value != nil {
		if object, ok := value.(map[string]any); ok {
			delete(object, idempotencyMetaKey)
		}
		if encoded, err := json.Marshal(value); err == nil {
			canonical = encoded
		}
	}
	sum := sha256.Sum256(append(append([]byte(toolName), 0), canonical...))
	return hex.EncodeToString(sum[:])
}

func firstToolResultText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

func cachedToolCallResult(responseText string) *mcp.CallToolResult {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: responseText}},
	}
	var structured map[string]any
	if json.Unmarshal([]byte(responseText), &structured) == nil && structured != nil {
		structured["idempotent_replay"] = true
		result.StructuredContent = structured
	}
	return result
}
