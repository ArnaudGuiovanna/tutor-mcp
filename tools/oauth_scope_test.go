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
	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type oauthScopeClaimCountingStore struct {
	storeport.Store
	claimCalls atomic.Int64
}

func (s *oauthScopeClaimCountingStore) ClaimIdempotencyKey(
	ctx context.Context, learnerID, toolName, key, requestHash string, now time.Time,
) (string, bool, error) {
	s.claimCalls.Add(1)
	return s.Store.ClaimIdempotencyKey(ctx, learnerID, toolName, key, requestHash, now)
}

func TestAddToolEnforcesGranularOAuthScopes(t *testing.T) {
	for _, tc := range []struct {
		name                string
		granted             string
		wantRead, wantWrite bool
		wantReadWrite       bool
	}{
		{name: "read cannot mutate", granted: models.OAuthScopeLearnerRead, wantRead: true},
		{name: "write cannot read", granted: models.OAuthScopeLearnerWrite, wantWrite: true},
		{name: "granular bundle", granted: models.OAuthScopeLearnerReadWrite, wantRead: true, wantWrite: true, wantReadWrite: true},
		{name: "bounded legacy bundle", granted: models.OAuthScopeLearner, wantRead: true, wantWrite: true, wantReadWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var readCalls, writeCalls, readWriteCalls int
			server := mcp.NewServer(&mcp.Implementation{Name: "scope-test", Version: "0"}, nil)
			registerProbe := func(name string, calls *int) {
				addTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
					*calls++
					result, _ := jsonResult(map[string]bool{"called": true})
					return result, map[string]bool{"called": true}, nil
				})
			}
			registerProbe("get_pending_alerts", &readCalls)
			registerProbe("record_interaction", &writeCalls)
			registerProbe("get_next_activity", &readWriteCalls)
			server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					ctx = context.WithValue(ctx, auth.LearnerIDKey, "scope-learner")
					ctx = auth.WithOAuthScope(ctx, tc.granted)
					return next(ctx, method, req)
				}
			})

			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
				t.Fatal(err)
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "scope-client", Version: "0"}, nil)
			session, err := client.Connect(context.Background(), clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()

			call := func(name string) *mcp.CallToolResult {
				t.Helper()
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
					Name: name, Arguments: json.RawMessage(`{}`),
				})
				if err != nil {
					t.Fatalf("call %s: %v", name, err)
				}
				return result
			}
			readResult := call("get_pending_alerts")
			writeResult := call("record_interaction")
			readWriteResult := call("get_next_activity")
			if readResult.IsError == tc.wantRead {
				t.Fatalf("read IsError=%v, want success=%v; body=%q", readResult.IsError, tc.wantRead, resultText(readResult))
			}
			if writeResult.IsError == tc.wantWrite {
				t.Fatalf("write IsError=%v, want success=%v; body=%q", writeResult.IsError, tc.wantWrite, resultText(writeResult))
			}
			if readWriteResult.IsError == tc.wantReadWrite {
				t.Fatalf("read+write IsError=%v, want success=%v; body=%q", readWriteResult.IsError, tc.wantReadWrite, resultText(readWriteResult))
			}
			if readCalls != boolInt(tc.wantRead) || writeCalls != boolInt(tc.wantWrite) || readWriteCalls != boolInt(tc.wantReadWrite) {
				t.Fatalf("handler calls read=%d write=%d read+write=%d, want %d/%d/%d", readCalls, writeCalls, readWriteCalls, boolInt(tc.wantRead), boolInt(tc.wantWrite), boolInt(tc.wantReadWrite))
			}
		})
	}
}

func TestRegisterToolsRejectsReadOnlyMutationBeforeIdempotencyClaim(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "off")
	oauthStore, deps := setupToolsTest(t)
	deps.BaseURL = "https://test.example"
	deps.OAuthGranularScopes = true
	countingStore := &oauthScopeClaimCountingStore{Store: deps.Store}
	deps.Store = countingStore
	server := mcp.NewServer(&mcp.Implementation{Name: "scope-order-test", Version: "0"}, nil)
	RegisterTools(server, deps)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ctx = context.WithValue(ctx, auth.LearnerIDKey, "L_owner")
			ctx = auth.WithOAuthScope(ctx, models.OAuthScopeLearnerRead)
			return next(ctx, method, req)
		}
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "scope-order-client", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "record_interaction",
		Arguments: json.RawMessage(`{
			"idempotency_key":"read-only-denied-before-claim"
		}`),
	})
	if err != nil {
		t.Fatalf("call mutation: %v", err)
	}
	if !result.IsError || resultText(result) != "insufficient OAuth scope: "+models.OAuthScopeLearnerWrite {
		t.Fatalf("unexpected denial: IsError=%v body=%q", result.IsError, resultText(result))
	}
	challenge := oauthChallengeFromMeta(t, result.Meta)
	for _, want := range []string{
		`error="insufficient_scope"`,
		`scope="learner:read learner:write"`,
		`resource_metadata="https://test.example/.well-known/oauth-protected-resource"`,
	} {
		if !strings.Contains(challenge, want) {
			t.Fatalf("challenge %q missing %q", challenge, want)
		}
	}
	if got := countingStore.claimCalls.Load(); got != 0 {
		t.Fatalf("unauthorized mutation reached ClaimIdempotencyKey %d times", got)
	}
	var claims int
	if err := oauthStore.RawDB().QueryRow(
		`SELECT COUNT(*) FROM tool_call_idempotency
		 WHERE learner_id = ? AND tool_name = ? AND idempotency_key = ?`,
		"L_owner", "record_interaction", "read-only-denied-before-claim",
	).Scan(&claims); err != nil {
		t.Fatalf("count idempotency claims: %v", err)
	}
	if claims != 0 {
		t.Fatalf("unauthorized mutation reserved %d idempotency rows", claims)
	}
}

func TestToolOAuthScopeMiddlewareDefersUnknownToolToSDK(t *testing.T) {
	called := false
	next := func(_ context.Context, method string, req mcp.Request) (mcp.Result, error) {
		called = true
		if method != "tools/call" {
			t.Fatalf("method = %q", method)
		}
		return &mcp.CallToolResult{}, nil
	}
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "unknown-tool-learner")
	ctx = auth.WithOAuthScope(ctx, models.OAuthScopeLearnerRead)
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "tool_that_does_not_exist"}}
	if _, err := toolOAuthScopeMiddleware("https://test.example", true)(next)(ctx, "tools/call", req); err != nil {
		t.Fatalf("middleware: %v", err)
	}
	if !called {
		t.Fatal("unknown tool was converted into an OAuth step-up instead of being delegated to the SDK")
	}
}

func TestToolOAuthScopeMiddlewareDefersDisabledToolToSDK(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "off")
	called := false
	next := func(_ context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	}
	ctx := context.WithValue(context.Background(), auth.LearnerIDKey, "disabled-tool-learner")
	ctx = auth.WithOAuthScope(ctx, models.OAuthScopeLearnerRead)
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "set_goal_relevance"}}
	if _, err := toolOAuthScopeMiddleware("https://test.example", true)(next)(ctx, "tools/call", req); err != nil {
		t.Fatalf("middleware: %v", err)
	}
	if !called {
		t.Fatal("disabled tool was converted into an OAuth step-up instead of being delegated to the SDK")
	}
}

func oauthChallengeFromMeta(t *testing.T, meta mcp.Meta) string {
	t.Helper()
	raw, ok := meta[mcpWWWAuthenticateMetaKey]
	if !ok {
		t.Fatalf("result metadata missing %q: %v", mcpWWWAuthenticateMetaKey, meta)
	}
	switch value := raw.(type) {
	case string:
		return value
	case []string:
		if len(value) == 1 {
			return value[0]
		}
	case []any:
		if len(value) == 1 {
			if challenge, ok := value[0].(string); ok {
				return challenge
			}
		}
	}
	t.Fatalf("unexpected %s metadata: %#v", mcpWWWAuthenticateMetaKey, raw)
	return ""
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
