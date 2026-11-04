// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"context"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	storeport "tutor-mcp/store"
)

const conversationMetaKey = "tutor/conversation_id"

// A shared desktop stdio transport can bind each call without restarting the
// server. Metadata selects a conversation within the authenticated principal;
// it never grants access and cannot override a transport's fixed binding.
func conversationBindingMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if method != "tools/call" || !ok || call.Params == nil {
				return next(ctx, method, req)
			}
			raw, exists := call.Params.GetMeta()[conversationMetaKey]
			if !exists {
				return next(ctx, method, req)
			}
			id, valid := raw.(string)
			if !valid || id == "" || len(id) > 128 || strings.TrimSpace(id) != id || strings.IndexFunc(id, unicode.IsControl) >= 0 {
				failure, _ := errorResult("invalid conversation metadata")
				return failure, nil
			}
			if bound := storeport.ConversationID(ctx); bound != "" && bound != id {
				failure, _ := errorResult("conversation metadata does not match the transport binding")
				return failure, nil
			}
			return next(storeport.WithConversation(ctx, id), method, req)
		}
	}
}
