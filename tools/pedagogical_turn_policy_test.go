// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type pedagogicalTurnRoutedStore struct{ *routedToolStore }

func (s *pedagogicalTurnRoutedStore) WithTenantReadTx(ctx context.Context, scope models.TenantScope, fn func(context.Context, storeport.Store) error) error {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		return s.routedToolStore.WithTenantReadTx(ctx, scope, fn)
	}
	// The shared SQLite probe deliberately executes a forbidden write. On
	// PostgreSQL that aborts the transaction and masks the handler being tested.
	// Use the real read-only PG transaction without poisoning it; db's reader
	// suite separately proves that PostgreSQL rejects writes on this route.
	s.reads++
	return s.Store.WithTenantReadTx(ctx, scope, fn)
}

func TestPedagogicalTurnModeEnforcesScopeTransactionAndReplay(t *testing.T) {
	s, deps, ctx, p, _, selected := pedagogicalTurnFixture(t)
	wrapped := &pedagogicalTurnRoutedStore{routedToolStore: &routedToolStore{Store: s}}
	deps.Store = wrapped
	for _, test := range []struct {
		mode, grant  string
		denied, read bool
	}{
		{"context", models.OAuthScopeLearnerRead, false, true},
		{"", models.OAuthScopeLearnerRead, false, true},
		{"activity", models.OAuthScopeLearnerRead, true, false},
		{"invalid", models.OAuthScopeLearner, true, false},
		{"activity", models.OAuthScopeLearner, false, false},
	} {
		t.Run(test.mode+test.grant, func(t *testing.T) {
			principal := p
			principal.Scopes = []string{test.grant}
			ctx, err := auth.WithPrincipal(ctx, principal)
			if err != nil {
				t.Fatal(err)
			}
			ctx = auth.WithOAuthScope(ctx, test.grant)
			in := map[string]any{"conversation_id": selected.ConversationID, "mode": test.mode, "idempotency_key": "policy-once"}
			raw, _ := json.Marshal(in)
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "prepare_pedagogical_turn", Arguments: raw}}
			handler := toolOAuthScopeMiddleware("", true)(toolTenantTransactionMiddleware(deps)(idempotencyMiddleware(deps)(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				return jsonResult(map[string]any{"executed": true})
			})))
			beforeR, beforeW := wrapped.reads, wrapped.writes
			result, err := handler(ctx, "tools/call", req)
			if err != nil || result.(*mcp.CallToolResult).IsError != test.denied {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if test.denied {
				if wrapped.reads != beforeR || wrapped.writes != beforeW {
					t.Fatal("denied call reached transaction")
				}
			} else if test.read {
				if wrapped.reads != beforeR+1 || wrapped.writes != beforeW {
					t.Fatal("context obtained writer")
				}
			}
			var count int
			if err := s.RawDB().QueryRow("SELECT COUNT(*) FROM tool_call_idempotency WHERE tool_name='prepare_pedagogical_turn'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if test.mode == "activity" && !test.denied {
				want = 1
			}
			if count != want {
				t.Fatalf("reserved=%d want %d", count, want)
			}
		})
	}
}

func TestConversationMetadataCannotOverrideTransportOrReplayAcrossBinding(t *testing.T) {
	_, deps := setupToolsTest(t)
	ctx := context.WithValue(t.Context(), auth.LearnerIDKey, "L_owner")
	executions := 0
	handler := conversationBindingMiddleware()(idempotencyMiddleware(deps)(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		executions++
		return jsonResult(map[string]any{"conversation": storeport.ConversationID(ctx)})
	}))
	request := func(binding any) *mcp.CallToolRequest {
		r := idempotencyToolRequest(t, "record_interaction", "binding-key", map[string]any{"success": true})
		r.Params.Meta[conversationMetaKey] = binding
		return r
	}
	for _, invalid := range []any{"", 123, " space", "bad\n", string(make([]byte, 129))} {
		r, err := handler(ctx, "tools/call", request(invalid))
		if err != nil || !r.(*mcp.CallToolResult).IsError {
			t.Fatal("invalid binding accepted")
		}
	}
	r, _ := handler(storeport.WithConversation(ctx, "fixed"), "tools/call", request("other"))
	if !r.(*mcp.CallToolResult).IsError || executions != 0 {
		t.Fatal("overrode transport")
	}
	for i := 0; i < 2; i++ {
		r, err := handler(storeport.WithConversation(ctx, "one"), "tools/call", request("one"))
		if err != nil || r.(*mcp.CallToolResult).IsError {
			t.Fatal("valid retry rejected", err)
		}
	}
	r, _ = handler(ctx, "tools/call", request("two"))
	if !r.(*mcp.CallToolResult).IsError || executions != 1 {
		t.Fatal("mutation replayed across conversations")
	}
}
