// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/models"
)

// The advertised tool is conservatively mutating, but its context mode must
// still work with a read-only grant and a real read transaction. Every request
// boundary uses the same mode parser, before reserving an idempotency key.
func requiredOAuthScopesForCall(call *mcp.CallToolRequest) ([]string, bool, error) {
	if call == nil || call.Params == nil {
		return nil, false, errors.New("missing tool arguments")
	}
	if call.Params.Name == "prepare_pedagogical_turn" {
		mode, err := pedagogicalTurnModeFromRequest(call)
		if err != nil {
			return nil, true, err
		}
		if mode == "activity" {
			return []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite}, true, nil
		}
		return []string{models.OAuthScopeLearnerRead}, true, nil
	}
	if trainingTools[call.Params.Name].actionSelected {
		// preview reads; propose/save and lifecycle changes write.
		read, err := trainingCallReadOnly(call)
		if err != nil {
			return nil, true, err
		}
		if read {
			return []string{models.OAuthScopeLearnerRead}, true, nil
		}
		return []string{models.OAuthScopeLearnerWrite}, true, nil
	}
	scopes, known := requiredOAuthScopesForTool(call.Params.Name)
	return scopes, known, nil
}

func readOnlyToolCall(req mcp.Request) (bool, error) {
	if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil && call.Params.Name == "prepare_pedagogical_turn" {
		mode, err := pedagogicalTurnModeFromRequest(call)
		return mode == "context", err
	}
	return readOnlyTools[toolNameFromRequest(req)], nil
}
