package tools

import (
	"context"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/observability"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolCallObservationKey struct{}
type toolCallObservation struct{ deniedResult *mcp.CallToolResult }

func markToolCallDenied(ctx context.Context, result *mcp.CallToolResult) {
	if observation, ok := ctx.Value(toolCallObservationKey{}).(*toolCallObservation); ok {
		observation.deniedResult = result
	}
}

func recordToolCall(ctx context.Context, name, outcome string, started time.Time) {
	principal, _ := auth.GetPrincipal(ctx)
	observability.RecordMCPTool(ctx, principal.TenantID, principal.MembershipID, "", name, outcome, time.Since(started))
}

// Install outside deadlines, scope checks, tenant transactions and replay
// handling. The final result includes commit failures and deadline replacement.
// Direct addTool embedders do not install this server-level observation.
func toolCallObservationMiddleware(registered map[string]struct{}) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			name := "unknown"
			if _, ok := registered[toolNameFromRequest(req)]; ok {
				name = toolNameFromRequest(req)
			}
			started := time.Now()
			observation := &toolCallObservation{}
			ctx = context.WithValue(ctx, toolCallObservationKey{}, observation)
			outcome := "failed" // Also records a panic, while leaving recovery to the caller.
			defer func() { recordToolCall(ctx, name, outcome, started) }()
			result, err = next(ctx, method, req)
			if err == nil && ctx.Err() == nil {
				if call, ok := result.(*mcp.CallToolResult); ok && call != nil && !call.IsError {
					outcome = "succeeded"
				}
			}
			if err == nil && ctx.Err() == nil && observation.deniedResult != nil && result == observation.deniedResult {
				outcome = "denied"
			}
			return result, err
		}
	}
}
