// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetAvailabilityModelParams struct{}

func registerGetAvailabilityModel(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_availability_model",
		Description: "Retrieve the learner's availability model (time slots, average session duration, frequency).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetAvailabilityModelParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_availability_model", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		avail, err := deps.Store.GetAvailability(learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to get availability", err)
			return r, nil, nil
		}

		// Parse windows JSON
		var windows []models.TimeWindow
		_ = json.Unmarshal([]byte(avail.WindowsJSON), &windows)
		if windows == nil {
			windows = []models.TimeWindow{}
		}

		// Get last active
		learner, _ := deps.Store.GetLearnerByID(learnerID)
		lastActive := ""
		if learner != nil && !learner.LastActive.IsZero() {
			lastActive = learner.LastActive.Format("2006-01-02T15:04:05Z")
		}

		r, _ := jsonResult(map[string]interface{}{
			"preferred_windows":            windows,
			"avg_session_duration_minutes": avail.AvgDuration,
			"sessions_per_week":            avail.SessionsWeek,
			"last_active":                  lastActive,
			"do_not_disturb":               avail.DoNotDisturb,
		})
		return r, nil, nil
	})
}
