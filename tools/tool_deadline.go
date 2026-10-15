package tools

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errToolCallDeadline = errors.New("MCP tool call deadline exceeded")

// ToolCallDeadlineMiddleware bounds cooperative tool execution. A handler that
// ignores cancellation is not detached: it could still commit a mutation.
func ToolCallDeadlineMiddleware(timeout time.Duration) mcp.Middleware {
	if timeout <= 0 {
		panic("MCP tool call timeout must be positive")
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			deadlineCtx, cancel := context.WithTimeoutCause(ctx, timeout, errToolCallDeadline)
			defer cancel()
			result, err := next(deadlineCtx, method, req)
			if errors.Is(context.Cause(deadlineCtx), errToolCallDeadline) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "tool call exceeded the server deadline; retry a mutation only by reusing its original idempotency_key"}}, StructuredContent: map[string]any{"error": "tool_call_timeout", "retry_with_original_idempotency_key_only": true}, IsError: true}, nil
			}
			return result, err
		}
	}
}
