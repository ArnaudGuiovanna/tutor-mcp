package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TutorToolPolicyHeader carries additional client restrictions. It never
// supplies identity, grants scopes, or replaces the resource's authorization.
const TutorToolPolicyHeader = "Tutor-Tool-Policy"

const maxToolPolicyBytes = 64 * 1024

type toolPolicyKey struct{}
type toolPolicy struct {
	defaultDecision string
	tools           map[string]string
}

func validToolDecision(value string) bool {
	return value == "ask" || value == "allow" || value == "deny"
}

func validPolicyToolName(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

var errInvalidToolPolicy = errors.New("invalid Tutor tool policy")

// Decode explicitly so duplicate keys cannot silently replace a deny with an
// allow. Keep the header bounded independently of the HTTP body and SDK limit.
func parseToolPolicy(raw string) (*toolPolicy, error) {
	if len(raw) == 0 || len(raw) > maxToolPolicyBytes {
		return nil, errInvalidToolPolicy
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errInvalidToolPolicy
	}
	policy := &toolPolicy{tools: make(map[string]string)}
	seen := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return nil, errInvalidToolPolicy
		}
		seen[name] = true
		switch name {
		case "default":
			if err := decoder.Decode(&policy.defaultDecision); err != nil || !validToolDecision(policy.defaultDecision) {
				return nil, errInvalidToolPolicy
			}
		case "tools":
			open, err := decoder.Token()
			if err != nil || open != json.Delim('{') {
				return nil, errInvalidToolPolicy
			}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || !validPolicyToolName(name) || len(policy.tools) >= 4096 {
					return nil, errInvalidToolPolicy
				}
				if _, duplicate := policy.tools[name]; duplicate {
					return nil, errInvalidToolPolicy
				}
				var decision string
				if err := decoder.Decode(&decision); err != nil || !validToolDecision(decision) {
					return nil, errInvalidToolPolicy
				}
				policy.tools[name] = decision
			}
			if close, err := decoder.Token(); err != nil || close != json.Delim('}') {
				return nil, errInvalidToolPolicy
			}
		default:
			return nil, errInvalidToolPolicy
		}
	}
	if close, err := decoder.Token(); err != nil || close != json.Delim('}') || !seen["default"] {
		return nil, errInvalidToolPolicy
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errInvalidToolPolicy
	}
	return policy, nil
}

func (policy *toolPolicy) denies(name string) bool {
	decision := policy.defaultDecision
	if override, ok := policy.tools[name]; ok {
		decision = override
	}
	return decision == "deny"
}

// ToolPolicyHTTPMiddleware is placed inside authentication. Invalid, repeated
// or oversized policies fail closed and cannot be interpreted differently by
// different layers of a proxy chain.
func ToolPolicyHTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(TutorToolPolicyHeader)
		if len(values) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if len(values) != 1 {
			http.Error(w, "invalid Tutor tool policy", http.StatusBadRequest)
			return
		}
		policy, err := parseToolPolicy(values[0])
		if err != nil {
			http.Error(w, "invalid Tutor tool policy", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), toolPolicyKey{}, policy)))
	})
}

// ToolPolicyMiddleware applies the same restriction to discovery and direct
// invocation, before transaction/replay reservation. This also enforces a
// revoked local consent when a provider remembered an earlier approval.
func ToolPolicyMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			policy, ok := ctx.Value(toolPolicyKey{}).(*toolPolicy)
			if !ok {
				return next(ctx, method, req)
			}
			if method == "tools/call" {
				call, ok := req.(*mcp.CallToolRequest)
				if ok && call.Params != nil && policy.denies(call.Params.Name) {
					result := &mcp.CallToolResult{
						IsError:           true,
						Content:           []mcp.Content{&mcp.TextContent{Text: "This tool is disabled in your Tutor permission settings."}},
						StructuredContent: map[string]any{"error": "tool_policy_denied"},
					}
					markToolCallDenied(ctx, result)
					return result, nil
				}
			}
			result, err := next(ctx, method, req)
			if method == "tools/list" && err == nil {
				if list, ok := result.(*mcp.ListToolsResult); ok {
					copy := *list
					copy.Tools = make([]*mcp.Tool, 0, len(list.Tools))
					for _, tool := range list.Tools {
						if tool != nil && !policy.denies(tool.Name) {
							copy.Tools = append(copy.Tools, tool)
						}
					}
					return &copy, nil
				}
			}
			return result, err
		}
	}
}
