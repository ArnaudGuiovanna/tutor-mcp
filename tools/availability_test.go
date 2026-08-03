// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestGetAvailabilityModel_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetAvailabilityModel, "", "get_availability_model", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestGetAvailabilityModel_DefaultsWhenNoRow(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetAvailabilityModel, "L_owner", "get_availability_model", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["avg_session_duration_minutes"].(float64) != 30 {
		t.Fatalf("expected default avg duration 30, got %v", out["avg_session_duration_minutes"])
	}
	if out["sessions_per_week"].(float64) != 3 {
		t.Fatalf("expected default sessions/week 3, got %v", out["sessions_per_week"])
	}
	wins, ok := out["preferred_windows"].([]any)
	if !ok {
		t.Fatalf("expected preferred_windows array")
	}
	if len(wins) != 0 {
		t.Fatalf("expected empty windows, got %v", wins)
	}
	if out["timezone"] != "UTC" || out["notification_consent"] != false || out["version"].(float64) != 0 {
		t.Fatalf("unexpected safe defaults: %v", out)
	}
}

func TestGetAvailabilityModel_PersistedRow(t *testing.T) {
	store, deps := setupToolsTest(t)
	a := &models.Availability{
		LearnerID:              "L_owner",
		Timezone:               "Europe/Paris",
		WindowsJSON:            `[{"day":"Mon","start":"09:00","end":"10:00"}]`,
		AvgDuration:            45,
		SessionsWeek:           5,
		DoNotDisturb:           true,
		NotificationConsent:    true,
		NotificationFrequency:  models.NotificationFrequencyWeekly,
		MaxNotificationsPerDay: 2,
		AccessibilityJSON:      `{"screen_reader_optimized":true}`,
	}
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerGetAvailabilityModel, "L_owner", "get_availability_model", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["avg_session_duration_minutes"].(float64) != 45 {
		t.Fatalf("expected duration 45, got %v", out["avg_session_duration_minutes"])
	}
	if out["sessions_per_week"].(float64) != 5 {
		t.Fatalf("expected sessions=5, got %v", out["sessions_per_week"])
	}
	if out["do_not_disturb"] != true {
		t.Fatalf("expected dnd=true, got %v", out["do_not_disturb"])
	}
	wins, _ := out["preferred_windows"].([]any)
	if len(wins) != 1 {
		t.Fatalf("expected 1 window, got %v", wins)
	}
	if out["timezone"] != "Europe/Paris" || out["notification_frequency"] != "weekly" {
		t.Fatalf("extended availability did not round-trip: %v", out)
	}
}

func TestUpdateAvailabilityModelValidatesAndRejectsStaleVersion(t *testing.T) {
	_, deps := setupToolsTest(t)
	args := map[string]any{
		"expected_version":             0,
		"timezone":                     "America/New_York",
		"preferred_windows":            []map[string]any{{"day": "monday", "start": "09:00", "end": "11:00"}},
		"avg_session_duration_minutes": 45,
		"sessions_per_week":            4,
		"do_not_disturb":               false,
		"notification_consent":         true,
		"notification_frequency":       "daily",
		"max_notifications_per_day":    2,
		"accessibility_preferences":    map[string]any{"screen_reader_optimized": true, "avoid_emojis": true, "prefer_plain_language": true, "extra_processing_time": true},
	}
	res := callTool(t, deps, registerUpdateAvailabilityModel, "L_owner", "update_availability_model", args)
	if res.IsError {
		t.Fatalf("update failed: %s", resultText(res))
	}
	out := decodeResult(t, res)
	if out["version"].(float64) != 1 || out["notification_consent"] != true || out["timezone"] != "America/New_York" {
		t.Fatalf("unexpected update response: %v", out)
	}

	stale := callTool(t, deps, registerUpdateAvailabilityModel, "L_owner", "update_availability_model", args)
	if !stale.IsError || !strings.Contains(resultText(stale), "changed concurrently") {
		t.Fatalf("stale update response: %q", resultText(stale))
	}
}

func TestUpdateAvailabilityModelRejectsInvalidTimezoneAndOverlap(t *testing.T) {
	_, deps := setupToolsTest(t)
	base := map[string]any{
		"expected_version":             0,
		"timezone":                     "Not/A_Real_Zone",
		"preferred_windows":            []map[string]any{},
		"avg_session_duration_minutes": 30,
		"sessions_per_week":            3,
		"do_not_disturb":               false,
		"notification_consent":         true,
		"notification_frequency":       "as_scheduled",
		"max_notifications_per_day":    1,
		"accessibility_preferences":    map[string]any{},
	}
	res := callTool(t, deps, registerUpdateAvailabilityModel, "L_owner", "update_availability_model", base)
	if !res.IsError {
		t.Fatalf("invalid timezone accepted: %v", decodeResult(t, res))
	}
	base["timezone"] = "UTC"
	base["preferred_windows"] = []map[string]any{
		{"day": "monday", "start": "09:00", "end": "11:00"},
		{"day": "Mon", "start": "10:00", "end": "12:00"},
	}
	res = callTool(t, deps, registerUpdateAvailabilityModel, "L_owner", "update_availability_model", base)
	if !res.IsError || !strings.Contains(resultText(res), "failed to update availability") {
		t.Fatalf("overlap response: %q", resultText(res))
	}
}
