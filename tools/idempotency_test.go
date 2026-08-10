// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/db"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIdempotencyMiddlewareReplaysSuccessfulMutation(t *testing.T) {
	_, deps := setupToolsTest(t)
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "L_owner")
	var executions atomic.Int32
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		executions.Add(1)
		return jsonResult(map[string]any{"updated": true, "execution": executions.Load()})
	}
	handler := idempotencyMiddleware(deps)(next)

	first, err := handler(ctx, "tools/call", idempotencyToolRequest(t, "record_interaction", "retry-1", map[string]any{"concept": "a", "success": true}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := handler(ctx, "tools/call", idempotencyToolRequest(t, "record_interaction", "retry-1", map[string]any{"success": true, "concept": "a"}))
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatalf("downstream executions=%d, want 1", executions.Load())
	}
	firstResult := first.(*mcp.CallToolResult)
	secondResult := second.(*mcp.CallToolResult)
	if firstToolResultText(firstResult) != firstToolResultText(secondResult) {
		t.Fatalf("cached response differs: first=%q second=%q", firstToolResultText(firstResult), firstToolResultText(secondResult))
	}
	structured, ok := secondResult.StructuredContent.(map[string]any)
	if !ok || structured["idempotent_replay"] != true {
		t.Fatalf("cached result does not identify replay: %#v", secondResult.StructuredContent)
	}

	conflict, err := handler(ctx, "tools/call", idempotencyToolRequest(t, "record_interaction", "retry-1", map[string]any{"concept": "b", "success": true}))
	if err != nil || !conflict.(*mcp.CallToolResult).IsError {
		t.Fatalf("changed payload should conflict: result=%#v err=%v", conflict, err)
	}
}

func TestLearningNegotiationParticipatesInIdempotencyPolicy(t *testing.T) {
	_, deps := setupToolsTest(t)
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "L_owner")
	var executions atomic.Int32
	handler := idempotencyMiddleware(deps)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		executions.Add(1)
		return jsonResult(map[string]any{"accepted": true})
	})
	request := func() *mcp.CallToolRequest {
		return idempotencyToolRequest(t, "learning_negotiation", "negotiation-retry-1", map[string]any{
			"session_id": "session-1",
			"concept":    "fractions",
		})
	}

	first, err := handler(ctx, "tools/call", request())
	if err != nil || first.(*mcp.CallToolResult).IsError {
		t.Fatalf("first negotiation: result=%#v err=%v", first, err)
	}
	replay, err := handler(ctx, "tools/call", request())
	if err != nil || replay.(*mcp.CallToolResult).IsError {
		t.Fatalf("negotiation replay: result=%#v err=%v", replay, err)
	}
	if executions.Load() != 1 {
		t.Fatalf("negotiation executions=%d, want 1", executions.Load())
	}
}

func TestIdempotencyMiddlewareExpiredResponseNeverReexecutes(t *testing.T) {
	store, deps := setupToolsTest(t)
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "L_owner")
	var executions atomic.Int32
	handler := idempotencyMiddleware(deps)(func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		executions.Add(1)
		return jsonResult(map[string]any{"updated": true, "private_detail": "learner response"})
	})
	request := func() *mcp.CallToolRequest {
		return idempotencyToolRequest(t, "record_interaction", "expired-response", map[string]any{"concept": "a", "success": true})
	}

	first, err := handler(ctx, "tools/call", request())
	if err != nil || first.(*mcp.CallToolResult).IsError || executions.Load() != 1 {
		t.Fatalf("initial mutation: result=%#v executions=%d err=%v", first, executions.Load(), err)
	}
	report, err := store.RunDataRetention(ctx, db.RetentionPolicy{IdempotencyResponseDays: 1}, time.Now().UTC().Add(48*time.Hour), true)
	if err != nil {
		t.Fatalf("expire cached response: %v", err)
	}
	if report.IdempotencyResponsePlaintext.Applied != 1 {
		t.Fatalf("expired responses=%d, want 1", report.IdempotencyResponsePlaintext.Applied)
	}

	for retry := 0; retry < 2; retry++ {
		result, err := handler(ctx, "tools/call", request())
		toolResult := result.(*mcp.CallToolResult)
		if err != nil || !toolResult.IsError {
			t.Fatalf("expired retry %d: result=%#v err=%v", retry, result, err)
		}
		if text := firstToolResultText(toolResult); !strings.Contains(text, "mutation already completed") || !strings.Contains(text, "cached response expired") {
			t.Fatalf("expired retry %d returned ambiguous error %q", retry, text)
		}
	}
	if executions.Load() != 1 {
		t.Fatalf("expired response caused downstream re-execution: executions=%d, want 1", executions.Load())
	}
}

func TestIdempotencyMiddlewareAbortsApplicationError(t *testing.T) {
	_, deps := setupToolsTest(t)
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "L_owner")
	var executions atomic.Int32
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if executions.Add(1) == 1 {
			return errorResult("validation failed")
		}
		return jsonResult(map[string]any{"updated": true})
	}
	handler := idempotencyMiddleware(deps)(next)
	req := func() *mcp.CallToolRequest {
		return idempotencyToolRequest(t, "record_affect", "retry-after-fix", map[string]any{"energy": 2})
	}
	first, err := handler(ctx, "tools/call", req())
	if err != nil || !first.(*mcp.CallToolResult).IsError {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	second, err := handler(ctx, "tools/call", req())
	if err != nil || second.(*mcp.CallToolResult).IsError || executions.Load() != 2 {
		t.Fatalf("retry did not execute: result=%#v executions=%d err=%v", second, executions.Load(), err)
	}
}

func TestIdempotencyMiddlewareRejectsInvalidKeyBeforeMutation(t *testing.T) {
	_, deps := setupToolsTest(t)
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "L_owner")
	var executions atomic.Int32
	handler := idempotencyMiddleware(deps)(func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		executions.Add(1)
		return jsonResult(map[string]any{"updated": true})
	})
	result, err := handler(ctx, "tools/call", idempotencyToolRequest(t, "record_interaction", "contains spaces", map[string]any{}))
	if err != nil || !result.(*mcp.CallToolResult).IsError || executions.Load() != 0 {
		t.Fatalf("invalid key reached handler: result=%#v executions=%d err=%v", result, executions.Load(), err)
	}
}

func TestIdempotencyMiddlewareAcceptsBodyKeyAndExcludesItFromHash(t *testing.T) {
	_, deps := setupToolsTest(t)
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "L_owner")
	var executions atomic.Int32
	handler := idempotencyMiddleware(deps)(func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		executions.Add(1)
		return jsonResult(map[string]any{"updated": true})
	})

	request := func(withMeta bool) *mcp.CallToolRequest {
		raw, err := json.Marshal(map[string]any{
			"idempotency_key": "body-retry-1",
			"concept":         "fractions",
		})
		if err != nil {
			t.Fatal(err)
		}
		params := &mcp.CallToolParamsRaw{Name: "record_interaction", Arguments: raw}
		if withMeta {
			params.Meta = mcp.Meta{idempotencyMetaKey: "body-retry-1"}
		}
		return &mcp.CallToolRequest{Params: params}
	}
	if result, err := handler(ctx, "tools/call", request(false)); err != nil || result.(*mcp.CallToolResult).IsError {
		t.Fatalf("body-key call failed: result=%#v err=%v", result, err)
	}
	if result, err := handler(ctx, "tools/call", request(true)); err != nil || result.(*mcp.CallToolResult).IsError {
		t.Fatalf("matching body/meta replay failed: result=%#v err=%v", result, err)
	}
	if executions.Load() != 1 {
		t.Fatalf("downstream executions=%d, want 1", executions.Load())
	}
}

func TestIdempotencyMiddlewareRejectsMismatchedBodyAndMetadataKeys(t *testing.T) {
	_, deps := setupToolsTest(t)
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "L_owner")
	var executions atomic.Int32
	handler := idempotencyMiddleware(deps)(func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		executions.Add(1)
		return jsonResult(map[string]any{"updated": true})
	})
	raw, err := json.Marshal(map[string]any{"idempotency_key": "body-key"})
	if err != nil {
		t.Fatal(err)
	}
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Meta:      mcp.Meta{idempotencyMetaKey: "metadata-key"},
		Name:      "record_interaction",
		Arguments: raw,
	}}
	result, err := handler(ctx, "tools/call", req)
	if err != nil || !result.(*mcp.CallToolResult).IsError || executions.Load() != 0 {
		t.Fatalf("mismatched keys reached handler: result=%#v executions=%d err=%v", result, executions.Load(), err)
	}
}

func TestCanonicalToolRequestHashPreservesLargeIntegers(t *testing.T) {
	first := canonicalToolRequestHash("record_interaction", json.RawMessage(`{"sequence":9007199254740992}`))
	second := canonicalToolRequestHash("record_interaction", json.RawMessage(`{"sequence":9007199254740993}`))
	if first == second {
		t.Fatal("distinct integers above 2^53 produced the same idempotency hash")
	}
	reordered := canonicalToolRequestHash("record_interaction", json.RawMessage(`{"b":2,"a":1}`))
	canonical := canonicalToolRequestHash("record_interaction", json.RawMessage(`{"a":1,"b":2}`))
	if reordered != canonical {
		t.Fatal("object key order changed the canonical idempotency hash")
	}
}

func idempotencyToolRequest(t *testing.T, toolName, key string, args any) *mcp.CallToolRequest {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Meta:      mcp.Meta{idempotencyMetaKey: key},
		Name:      toolName,
		Arguments: raw,
	}}
}
