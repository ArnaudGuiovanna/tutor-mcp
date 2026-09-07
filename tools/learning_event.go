// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

type RecordLearningEventParams struct {
	IdempotentMutationParams
	EventKey  string `json:"event_key" jsonschema:"stable unique key for this delivered event; retain across retries"`
	DomainID  string `json:"domain_id" jsonschema:"own active domain ID"`
	Concept   string `json:"concept" jsonschema:"concept receiving feedback or instruction"`
	AttemptID string `json:"attempt_id,omitempty" jsonschema:"related assessment attempt, if any"`
	Kind      string `json:"kind" jsonschema:"feedback or instruction; never report a response here"`
}

func registerRecordLearningEvent(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{Name: "record_learning_event", Description: "After delivering feedback or instruction, record that exposure once. Server receipt time is used. This does not score a response, advance BKT or award an FSRS success; submit_assessment_attempt records bound responses separately."}, func(ctx context.Context, _ *mcp.CallToolRequest, p RecordLearningEventParams) (*mcp.CallToolResult, any, error) {
		actor, ok := auth.GetPrincipal(ctx)
		if !ok {
			r, _ := errorResult("authenticated principal required")
			return r, nil, nil
		}
		event, replayed, err := deps.Store.RecordLearningEvent(ctx, actor, models.LearningEventRequest{EventKey: p.EventKey, DomainID: p.DomainID, ConceptID: p.Concept, AttemptID: p.AttemptID, Kind: p.Kind})
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "learning event rejected; check the active domain, concept, attempt and event key", err)
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{"event": event, "replayed": replayed, "model_updated": false, "presentation_verified": false})
		return r, nil, nil
	})
}
