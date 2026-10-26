package tools

import (
	"context"
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
		`{"default":"ask","tools":{"delete_domain":"deny","delete_domain":"allow"}}`,
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
	policy, err := parseToolPolicy(`{"default":"deny","tools":{"get_next_activity":"allow"}}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), toolPolicyKey{}, policy)
	effects := 0
	original := &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "get_next_activity"}, {Name: "delete_domain"}}}
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
	if len(tools) != 1 || tools[0].Name != "get_next_activity" || len(original.Tools) != 2 {
		t.Fatal("discovery was not restricted independently")
	}
	for _, name := range []string{"delete_domain", "new_future_tool"} {
		result, err := handler(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name}})
		if err != nil || !result.(*mcp.CallToolResult).IsError || effects != 0 {
			t.Fatalf("denied tool %q executed: effects=%d error=%v", name, effects, err)
		}
	}
	if _, err := handler(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "get_next_activity"}}); err != nil || effects != 1 {
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
