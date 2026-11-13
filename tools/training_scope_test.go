// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

// trainingScopeCases covers the per-call scope escalation of the
// action-selected training tools: a read-only grant may preview, never apply.
var trainingScopeCases = []struct {
	tool, args string
	read       bool
}{
	{"edit_training", `{"action":"preview"}`, true},
	{"edit_training", `{"action":"propose"}`, false},
	{"edit_training", `{"action":"save"}`, false},
	{"manage_training_lifecycle", `{"action":"delete","preview":true}`, true},
	{"manage_training_lifecycle", `{"action":"archive","preview":true}`, true},
	{"manage_training_lifecycle", `{"action":"delete"}`, false},
	{"manage_training_lifecycle", `{"action":"withdraw","preview":false}`, false},
}

func TestActionSelectedTrainingToolsAdvertiseReadAndEscalatePerCall(t *testing.T) {
	for _, name := range []string{"edit_training", "manage_training_lifecycle"} {
		if !trainingTools[name].actionSelected {
			t.Fatalf("%s is not action-selected", name)
		}
		scopes, known := requiredOAuthScopesForTool(name)
		if !known || !slices.Equal(scopes, []string{models.OAuthScopeLearnerRead}) {
			t.Fatalf("%s advertises %v", name, scopes)
		}
		if slices.Contains(trainingInputSchema(name).Required, "idempotency_key") {
			t.Fatalf("%s requires idempotency_key for previews", name)
		}
	}
	if !slices.Contains(trainingInputSchema("join_training").Required, "idempotency_key") {
		t.Fatal("plain training write lost its idempotency_key requirement")
	}
	for _, tc := range trainingScopeCases {
		call := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: tc.tool, Arguments: json.RawMessage(tc.args)}}
		scopes, known, err := requiredOAuthScopesForCall(call)
		want := []string{models.OAuthScopeLearnerWrite}
		if tc.read {
			want = []string{models.OAuthScopeLearnerRead}
		}
		if err != nil || !known || !slices.Equal(scopes, want) {
			t.Fatalf("%s %s: scopes=%v known=%v err=%v, want %v", tc.tool, tc.args, scopes, known, err, want)
		}
	}
	bad := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "edit_training", Arguments: json.RawMessage(`{"action":"rewrite"}`)}}
	if _, _, err := requiredOAuthScopesForCall(bad); err == nil {
		t.Fatal("unknown action selected a scope")
	}
}

func TestReadOnlyGrantCannotApplyTrainingChangesAtAnyBoundary(t *testing.T) {
	deps := &Deps{BaseURL: "https://test.example", OAuthGranularScopes: true, Regulation: RegulationFlagsFromEnv()}
	readCtx := func(ctx context.Context) context.Context {
		ctx = context.WithValue(ctx, auth.LearnerIDKey, "training-scope-learner")
		return auth.WithOAuthScope(ctx, models.OAuthScopeLearnerRead)
	}

	t.Run("http", func(t *testing.T) {
		for _, tc := range trainingScopeCases {
			called := false
			handler := OAuthScopeHTTPMiddleware(deps, 4096, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":` + tc.args + `}}`
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, scopedHTTPRequest(t, body, models.OAuthScopeLearnerRead))
			if called != tc.read || (tc.read && rec.Code != http.StatusNoContent) || (!tc.read && rec.Code != http.StatusForbidden) {
				t.Fatalf("%s %s: status=%d called=%v", tc.tool, tc.args, rec.Code, called)
			}
		}
	})

	t.Run("mcp middleware", func(t *testing.T) {
		for _, tc := range trainingScopeCases {
			called := false
			next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
				called = true
				return &mcp.CallToolResult{}, nil
			}
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: tc.tool, Arguments: json.RawMessage(tc.args)}}
			result, err := toolOAuthScopeMiddleware(deps)(next)(readCtx(t.Context()), "tools/call", req)
			if err != nil || called != tc.read || result.(*mcp.CallToolResult).IsError == tc.read {
				t.Fatalf("%s %s: called=%v result=%v err=%v", tc.tool, tc.args, called, result, err)
			}
		}
	})

	t.Run("addTool handler", func(t *testing.T) {
		type probeInput struct {
			Action  string `json:"action"`
			Preview bool   `json:"preview,omitempty"`
		}
		calls := 0
		server := mcp.NewServer(&mcp.Implementation{Name: "training-scope", Version: "0"}, nil)
		for _, name := range []string{"edit_training", "manage_training_lifecycle"} {
			addTool(server, nil, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, probeInput) (*mcp.CallToolResult, any, error) {
				calls++
				result, _ := jsonResult(map[string]bool{"called": true})
				return result, nil, nil
			})
		}
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				return next(readCtx(ctx), method, req)
			}
		})
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		if _, err := server.Connect(t.Context(), serverTransport, nil); err != nil {
			t.Fatal(err)
		}
		session, err := mcp.NewClient(&mcp.Implementation{Name: "training-scope-client", Version: "0"}, nil).Connect(t.Context(), clientTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		for _, tc := range trainingScopeCases {
			before := calls
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.tool, Arguments: json.RawMessage(tc.args)})
			if err != nil {
				t.Fatal(err)
			}
			if executed := calls != before; executed != tc.read || result.IsError == tc.read {
				t.Fatalf("%s %s: executed=%v isError=%v", tc.tool, tc.args, executed, result.IsError)
			}
		}
	})
}
