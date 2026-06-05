// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
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
}

func TestGetAvailabilityModel_PersistedRow(t *testing.T) {
	store, deps := setupToolsTest(t)
	a := &models.Availability{
		LearnerID:    "L_owner",
		WindowsJSON:  `[{"day":"Mon","start":"09:00","end":"10:00"}]`,
		AvgDuration:  45,
		SessionsWeek: 5,
		DoNotDisturb: true,
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
}
