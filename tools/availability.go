// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetAvailabilityModelParams struct{}

type UpdateAvailabilityModelParams struct {
	IdempotentMutationParams
	ExpectedVersion        int                             `json:"expected_version" jsonschema:"version returned by get_availability_model; use 0 only when no persisted policy exists"`
	Timezone               string                          `json:"timezone" jsonschema:"valid IANA timezone, for example Europe/Paris or America/New_York"`
	PreferredWindows       []models.TimeWindow             `json:"preferred_windows" jsonschema:"non-overlapping weekly local-time windows; empty means any time after consent"`
	AvgSessionDuration     int                             `json:"avg_session_duration_minutes" jsonschema:"typical session duration, 5 to 240 minutes"`
	SessionsPerWeek        int                             `json:"sessions_per_week" jsonschema:"target session cadence, 1 to 21"`
	DoNotDisturb           bool                            `json:"do_not_disturb" jsonschema:"when true, suppress every proactive notification"`
	NotificationConsent    bool                            `json:"notification_consent" jsonschema:"explicit opt-in for proactive webhook notifications"`
	NotificationFrequency  string                          `json:"notification_frequency" jsonschema:"as_scheduled, daily, or weekly"`
	MaxNotificationsPerDay int                             `json:"max_notifications_per_day" jsonschema:"hard local-day cap, 1 to 10"`
	Accessibility          models.AccessibilityPreferences `json:"accessibility_preferences" jsonschema:"learner-selected accessibility accommodations"`
}

func registerGetAvailabilityModel(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "get_availability_model",
		Description: "Retrieve learner-owned availability, IANA timezone, weekly local-time windows, DND, notification consent/frequency/cap, accessibility accommodations and optimistic version.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetAvailabilityModelParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_availability_model", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		avail, err := deps.Store.GetAvailability(ctx, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to get availability", err)
			return r, nil, nil
		}

		windows, err := avail.Windows()
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "stored availability is invalid", err)
			return r, nil, nil
		}
		accessibility, err := avail.Accessibility()
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "stored accessibility preferences are invalid", err)
			return r, nil, nil
		}

		// Get last active
		learner, err := deps.Store.GetLearnerByID(ctx, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load learner activity", err)
			return r, nil, nil
		}
		lastActive := ""
		if learner != nil && !learner.LastActive.IsZero() {
			lastActive = learner.LastActive.Format("2006-01-02T15:04:05Z")
		}

		payload := availabilityPayload(avail, windows, accessibility)
		payload["last_active"] = lastActive
		r, _ := jsonResult(payload)
		return r, nil, nil
	})
}

func registerUpdateAvailabilityModel(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "update_availability_model",
		Description: "Replace the authenticated learner's availability and accessibility policy. Uses expected_version to reject concurrent lost updates. Notification consent is explicit; DND and local weekly windows are enforced again at delivery time.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params UpdateAvailabilityModelParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "update_availability_model", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.ExpectedVersion < 0 {
			r, _ := errorResult("expected_version must be non-negative")
			return r, nil, nil
		}
		windowsJSON, err := json.Marshal(params.PreferredWindows)
		if err != nil {
			r, _ := errorResult("preferred_windows are invalid")
			return r, nil, nil
		}
		accessibilityJSON, err := json.Marshal(params.Accessibility)
		if err != nil {
			r, _ := errorResult("accessibility_preferences are invalid")
			return r, nil, nil
		}
		availability := &models.Availability{
			LearnerID:              learnerID,
			Timezone:               params.Timezone,
			WindowsJSON:            string(windowsJSON),
			AvgDuration:            params.AvgSessionDuration,
			SessionsWeek:           params.SessionsPerWeek,
			DoNotDisturb:           params.DoNotDisturb,
			NotificationConsent:    params.NotificationConsent,
			NotificationFrequency:  params.NotificationFrequency,
			MaxNotificationsPerDay: params.MaxNotificationsPerDay,
			AccessibilityJSON:      string(accessibilityJSON),
		}
		// Normalize and decode the response-facing values before persistence.
		// Once UpdateAvailability succeeds, no fallible presentation work may
		// turn the committed version bump into an MCP error and release its
		// idempotency reservation.
		if err := availability.NormalizeAndValidate(); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to update availability", err)
			return r, nil, nil
		}
		windows, err := availability.Windows()
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to update availability", err)
			return r, nil, nil
		}
		accessibility, err := availability.Accessibility()
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to update availability", err)
			return r, nil, nil
		}
		updated, err := deps.Store.UpdateAvailability(ctx, availability, params.ExpectedVersion)
		if err != nil {
			if errors.Is(err, storeport.ErrAvailabilityVersionConflict) {
				r, _ := errorResult("availability changed concurrently; reload get_availability_model and retry with its version")
				return r, nil, nil
			}
			r, _ := safeErrorResult(deps.Logger, "failed to update availability", err)
			return r, nil, nil
		}
		r, _ := jsonResult(availabilityPayload(updated, windows, accessibility))
		return r, nil, nil
	})
}

func availabilityPayload(avail *models.Availability, windows []models.TimeWindow, accessibility models.AccessibilityPreferences) map[string]interface{} {
	updatedAt := ""
	if !avail.UpdatedAt.IsZero() {
		updatedAt = avail.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return map[string]interface{}{
		"timezone":                     avail.Timezone,
		"preferred_windows":            windows,
		"avg_session_duration_minutes": avail.AvgDuration,
		"sessions_per_week":            avail.SessionsWeek,
		"do_not_disturb":               avail.DoNotDisturb,
		"notification_consent":         avail.NotificationConsent,
		"notification_frequency":       avail.NotificationFrequency,
		"max_notifications_per_day":    avail.MaxNotificationsPerDay,
		"accessibility_preferences":    accessibility,
		"version":                      avail.Version,
		"updated_at":                   updatedAt,
		"policy_note":                  fmt.Sprintf("weekly windows are interpreted in %s and rechecked at delivery time", avail.Timezone),
	}
}
