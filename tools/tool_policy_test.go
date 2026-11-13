package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolPolicyRejectsAmbiguousOrUnboundedConsent(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `[]`, `{}`, `{"default":"always"}`,
		`{"default":"deny","default":"allow"}`,
		`{"default":"ask","tools":{"manage_domain.delete":"deny","manage_domain.delete":"allow"}}`,
		`{"default":"allow","tools":{"read all files":"allow"}}`,
		`{"default":"allow","tools":{"x":null}}`,
		`{"default":"allow","tools":[]}`,
		`{"default":"allow","unknown":true}`,
		`{"default":"allow"} {"default":"deny"}`,
		strings.Repeat(" ", maxToolPolicyBytes+1),
	} {
		t.Run(raw[:min(len(raw), 70)], func(t *testing.T) {
			called := false
			handler := ToolPolicyHTTPMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			request.Header.Set(TutorToolPolicyHeader, raw)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if called || response.Code != http.StatusBadRequest {
				t.Fatalf("invalid policy reached SDK: called=%t status=%d", called, response.Code)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Add(TutorToolPolicyHeader, `{"default":"deny"}`)
	request.Header.Add(TutorToolPolicyHeader, `{"default":"allow"}`)
	response := httptest.NewRecorder()
	ToolPolicyHTTPMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("repeated header admitted") })).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatal(response.Code)
	}
}

func TestToolPolicyHidesAndBlocksDeniedToolsBeforeEffects(t *testing.T) {
	policy, err := parseToolPolicy(`{"default":"deny","tools":{"get_learner_state":"allow"}}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), toolPolicyKey{}, policy)
	effects := 0
	original := &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "get_learner_state"}, {Name: "manage_domain"}}}
	next := func(_ context.Context, method string, _ mcp.Request) (mcp.Result, error) {
		if method == "tools/list" {
			return original, nil
		}
		effects++
		return &mcp.CallToolResult{}, nil
	}
	handler := ToolPolicyMiddleware()(next)
	listed, err := handler(ctx, "tools/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := listed.(*mcp.ListToolsResult).Tools
	if len(tools) != 1 || tools[0].Name != "get_learner_state" || len(original.Tools) != 2 {
		t.Fatal("discovery was not restricted independently")
	}
	for _, name := range []string{"manage_domain", "new_future_tool"} {
		result, err := handler(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name}})
		if err != nil || !result.(*mcp.CallToolResult).IsError || effects != 0 {
			t.Fatalf("denied tool %q executed: effects=%d error=%v", name, effects, err)
		}
	}
	if _, err := handler(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "get_learner_state"}}); err != nil || effects != 1 {
		t.Fatalf("allowed tool did not reach existing authorization: effects=%d error=%v", effects, err)
	}
}

func TestToolPolicyCannotOverrideInstitutionAuthorization(t *testing.T) {
	denied := errors.New("institution scope denied")
	policy, err := parseToolPolicy(`{"default":"allow"}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), toolPolicyKey{}, policy)
	handler := ToolPolicyMiddleware()(func(context.Context, string, mcp.Request) (mcp.Result, error) { return nil, denied })
	_, err = handler(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "delete_domain"}})
	if !errors.Is(err, denied) {
		t.Fatal("local allow replaced institution authorization")
	}
	_, err = handler(t.Context(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "delete_domain"}})
	if !errors.Is(err, denied) {
		t.Fatal("missing local policy replaced institution authorization")
	}
}

func TestToolPolicyAddressesOperationsOfMergedTools(t *testing.T) {
	type call struct {
		tool, args string
		denied     bool
	}
	for _, test := range []struct {
		name, policy string
		hidden       []string
		calls        []call
	}{
		{"operation deny", `{"default":"allow","tools":{"manage_domain.delete":"deny"}}`, nil, []call{
			{"manage_domain", `{"action":"delete","domain_id":"d"}`, true},
			{"manage_domain", `{"action":"archive","domain_id":"d"}`, false},
			{"manage_domain", `{"action":"unarchive","domain_id":"d"}`, false},
			{"get_learner_state", `{}`, false},
		}},
		{"bare deny blocks every operation", `{"default":"allow","tools":{"manage_domain":"deny","manage_domain.archive":"allow"}}`, []string{"manage_domain"}, []call{
			{"manage_domain", `{"action":"delete"}`, true},
			{"manage_domain", `{"action":"archive"}`, true},
			{"manage_domain", `{}`, true},
		}},
		{"operation allow under deny default", `{"default":"deny","tools":{"manage_training_lifecycle.archive":"allow"}}`, []string{"edit_training"}, []call{
			{"manage_training_lifecycle", `{"action":"archive","preview":true}`, false},
			{"manage_training_lifecycle", `{"action":"delete","preview":true}`, true},
			{"manage_training_lifecycle", `{"action":"withdraw"}`, true},
			{"edit_training", `{"action":"preview"}`, true},
		}},
		{"edit actions", `{"default":"allow","tools":{"edit_training.save":"deny"}}`, nil, []call{
			{"edit_training", `{"action":"save"}`, true},
			{"edit_training", `{"action":"propose"}`, false},
			{"edit_training", `{"action":"preview"}`, false},
		}},
		{"activity mode replaces get_next_activity", `{"default":"allow","tools":{"prepare_pedagogical_turn.activity":"deny"}}`, nil, []call{
			{"prepare_pedagogical_turn", `{"mode":"activity"}`, true},
			{"prepare_pedagogical_turn", `{"mode":"context"}`, false},
			{"prepare_pedagogical_turn", `{}`, false},
		}},
		{"omitted mode is context", `{"default":"allow","tools":{"prepare_pedagogical_turn.context":"deny"}}`, nil, []call{
			{"prepare_pedagogical_turn", `{}`, true},
			{"prepare_pedagogical_turn", `{"mode":""}`, true},
			{"prepare_pedagogical_turn", `{"mode":"activity"}`, false},
		}},
		{"undecodable selector fails closed", `{"default":"allow","tools":{"manage_domain.delete":"deny"}}`, nil, []call{
			{"manage_domain", `{"action":["delete"]}`, true},
			{"manage_domain", `not json`, true},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := parseToolPolicy(test.policy)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), toolPolicyKey{}, policy)
			all := []string{"manage_domain", "edit_training", "manage_training_lifecycle", "prepare_pedagogical_turn", "get_learner_state"}
			effects := 0
			handler := ToolPolicyMiddleware()(func(_ context.Context, method string, _ mcp.Request) (mcp.Result, error) {
				if method == "tools/list" {
					list := &mcp.ListToolsResult{}
					for _, name := range all {
						list.Tools = append(list.Tools, &mcp.Tool{Name: name})
					}
					return list, nil
				}
				effects++
				return &mcp.CallToolResult{}, nil
			})
			listed, err := handler(ctx, "tools/list", nil)
			if err != nil {
				t.Fatal(err)
			}
			visible := map[string]bool{}
			for _, tool := range listed.(*mcp.ListToolsResult).Tools {
				visible[tool.Name] = true
			}
			for _, name := range test.hidden {
				if visible[name] {
					t.Fatalf("%s listed although every operation is denied", name)
				}
			}
			for _, c := range test.calls {
				if (policy.defaultDecision == "allow" || c.tool == "manage_training_lifecycle") && !c.denied && !visible[c.tool] {
					t.Fatalf("%s hidden although %s is allowed", c.tool, c.args)
				}
				before := effects
				result, err := handler(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: c.tool, Arguments: json.RawMessage(c.args)}})
				if err != nil {
					t.Fatal(err)
				}
				if denied := result.(*mcp.CallToolResult).IsError; denied != c.denied || (effects == before) != c.denied {
					t.Fatalf("%s %s: denied=%v effects=%d->%d, want denied=%v", c.tool, c.args, denied, before, effects, c.denied)
				}
			}
		})
	}
}
