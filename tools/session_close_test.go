// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestRecordSessionClose_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordSessionClose, "", "record_session_close", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestRecordSessionClose_NoDomain(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{})
	// Issue #33: uniform needs_domain_setup signal across chat-side tools.
	out := decodeResult(t, res)
	if got, _ := out["needs_domain_setup"].(bool); !got {
		t.Fatalf("expected needs_domain_setup=true, got %v", out)
	}
}

func TestRecordSessionClose_HappyPath_NoIntention(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	// Seed a successful session interaction so RecapBrief shows wins.
	if err := store.CreateInteraction(context.Background(), &models.Interaction{
		LearnerID:    "L_owner",
		Concept:      "a",
		ActivityType: "RECALL_EXERCISE",
		Success:      true,
		Confidence:   0.8,
	}); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	recap, ok := out["recap_brief"].(map[string]any)
	if !ok {
		t.Fatalf("expected recap_brief, got %v", out)
	}
	wins, _ := recap["wins"].([]any)
	if len(wins) == 0 || wins[0] != "a" {
		t.Fatalf("expected wins=[a], got %v", wins)
	}
	summaryRequest, ok := out["summary_request"].(map[string]any)
	if !ok {
		t.Fatalf("expected summary_request when memory is enabled, got %v", out)
	}
	template, _ := summaryRequest["template"].(string)
	if !strings.Contains(template, "You have just closed a session") {
		t.Fatalf("unexpected summary template: %q", template)
	}
}

func TestRecordSessionClose_PersistsImplementationIntention(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	scheduled := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	res := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{
		"domain_id": d.ID,
		"implementation_intention": map[string]any{
			"trigger":       "demain matin au cafe",
			"action":        "ferai 1 exercice de derivees",
			"scheduled_for": scheduled,
		},
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	// DB state: intention persisted.
	intentions, err := store.GetRecentImplementationIntentions(context.Background(), "L_owner", time.Now().UTC().Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(intentions) != 1 {
		t.Fatalf("expected 1 intention, got %d", len(intentions))
	}
	if intentions[0].Trigger != "demain matin au cafe" || intentions[0].Action != "ferai 1 exercice de derivees" {
		t.Fatalf("intention not persisted correctly: %+v", intentions[0])
	}
}

func TestRecordSessionClose_EmptyIntentionFieldsSkipped(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{
		"domain_id": d.ID,
		"implementation_intention": map[string]any{
			"trigger": "x",
			"action":  "", // empty action — should not insert
		},
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	intentions, _ := store.GetRecentImplementationIntentions(context.Background(), "L_owner", time.Now().UTC().Add(-time.Hour), 10)
	if len(intentions) != 0 {
		t.Fatalf("should not have persisted intention with empty action, got %d", len(intentions))
	}
}

func TestRecordSessionClose_InstructionMentionsOLM(t *testing.T) {
	store, deps := setupToolsTest(t)
	_ = makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{})
	if res.IsError {
		t.Fatalf("error: %q", resultText(res))
	}
	out := decodeResult(t, res)
	recap, ok := out["recap_brief"].(map[string]any)
	if !ok {
		t.Fatalf("recap_brief missing or wrong shape: %+v", out)
	}
	instruction, _ := recap["instruction"].(string)
	if !strings.Contains(instruction, "get_olm_snapshot") {
		t.Errorf("instruction should mention get_olm_snapshot: %q", instruction)
	}
	if !strings.Contains(instruction, "olm:") {
		t.Errorf("instruction should mention 'olm:' kind: %q", instruction)
	}
	if !strings.Contains(instruction, "13h") {
		t.Errorf("instruction should mention 13h UTC dispatch: %q", instruction)
	}
}

func TestMapKeysHelper(t *testing.T) {
	if got := mapKeys(nil); len(got) != 0 {
		t.Fatalf("expected empty for nil, got %v", got)
	}
	got := mapKeys(map[string]bool{"a": true, "b": true})
	if len(got) != 2 {
		t.Fatalf("expected 2 keys, got %v", got)
	}
}
