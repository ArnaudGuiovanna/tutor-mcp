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

func TestStartLearningSession_IdempotentAndOwned(t *testing.T) {
	store, deps := setupToolsTest(t)
	first := callTool(t, deps, registerStartLearningSession, "L_owner", "start_learning_session", map[string]any{
		"session_id": "sess_tool_key",
	})
	if first.IsError {
		t.Fatalf("first start: %q", resultText(first))
	}
	firstOut := decodeResult(t, first)
	if firstOut["session_id"] != "sess_tool_key" {
		t.Fatalf("unexpected canonical session: %v", firstOut)
	}
	repeated := callTool(t, deps, registerStartLearningSession, "L_owner", "start_learning_session", map[string]any{
		"session_id": "sess_tool_key",
	})
	if repeated.IsError || decodeResult(t, repeated)["session_id"] != "sess_tool_key" {
		t.Fatalf("idempotent start failed: %q", resultText(repeated))
	}
	attacker := callTool(t, deps, registerStartLearningSession, "L_attacker", "start_learning_session", map[string]any{
		"session_id": "sess_tool_key",
	})
	if !attacker.IsError {
		t.Fatalf("attacker reused another learner's session: %v", decodeResult(t, attacker))
	}
	if _, err := store.GetLearningSession(context.Background(), "L_attacker", "sess_tool_key"); err == nil {
		t.Fatal("attacker acquired the owner's session")
	}
}

func TestRecordSessionClose_ClosesOwnedSessionIdempotently(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	session := openTestLearningSession(t, store, "L_owner", "sess_close", domain.ID)

	first := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{
		"session_id": session.ID,
		"domain_id":  domain.ID,
	})
	if first.IsError {
		t.Fatalf("close: %q", resultText(first))
	}
	closed, err := store.GetLearningSession(context.Background(), "L_owner", session.ID)
	if err != nil || closed.Status != models.LearningSessionStatusClosed || closed.ClosedAt == nil {
		t.Fatalf("session not closed: session=%+v err=%v", closed, err)
	}

	// A retry may capture the commitment that the learner supplied after the
	// closing prompt; it must not create/reopen another session.
	retry := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{
		"session_id": session.ID,
		"domain_id":  domain.ID,
		"implementation_intention": map[string]any{
			"trigger": "tomorrow after breakfast",
			"action":  "practice one exercise",
		},
	})
	if retry.IsError {
		t.Fatalf("idempotent close retry: %q", resultText(retry))
	}
	retryAgain := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{
		"session_id": session.ID,
		"domain_id":  domain.ID,
		"implementation_intention": map[string]any{
			"trigger": "tomorrow after breakfast",
			"action":  "practice one exercise",
		},
	})
	if retryAgain.IsError {
		t.Fatalf("second idempotent close retry: %q", resultText(retryAgain))
	}
	intentions, err := store.GetRecentImplementationIntentions(context.Background(), "L_owner", time.Now().UTC().Add(-time.Hour), 10)
	if err != nil || len(intentions) != 1 || intentions[0].SessionID != session.ID {
		t.Fatalf("retry intention not linked to closed session: intentions=%+v err=%v", intentions, err)
	}
	active, err := store.GetActiveLearningSession(context.Background(), "L_owner")
	if err == nil || active != nil {
		t.Fatalf("close retry opened another session: %+v", active)
	}
}

func TestRecordSessionClose_RejectsForeignSession(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	openTestLearningSession(t, store, "L_owner", "sess_private", domain.ID)
	res := callTool(t, deps, registerRecordSessionClose, "L_attacker", "record_session_close", map[string]any{
		"session_id": "sess_private",
	})
	if !res.IsError || !strings.Contains(resultText(res), "not found") {
		t.Fatalf("foreign close result = %q", resultText(res))
	}
}

func TestImplementationIntentionTools_LifecycleAndOwnership(t *testing.T) {
	store, deps := setupToolsTest(t)
	session := openTestLearningSession(t, store, "L_owner", "sess_intent_tool", "")
	id, err := store.InsertImplementationIntentionForSession(
		context.Background(), "L_owner", "", session.ID, "at lunch", "review notes", time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}

	listed := callTool(t, deps, registerListImplementationIntentions, "L_owner", "list_implementation_intentions", map[string]any{
		"status": "pending",
	})
	if listed.IsError {
		t.Fatalf("list intentions: %q", resultText(listed))
	}
	rows, _ := decodeResult(t, listed)["intentions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("pending intentions = %v", rows)
	}

	attacker := callTool(t, deps, registerUpdateImplementationIntention, "L_attacker", "update_implementation_intention", map[string]any{
		"id": id, "status": "cancelled",
	})
	if !attacker.IsError {
		t.Fatal("attacker resolved owner's intention")
	}
	updated := callTool(t, deps, registerUpdateImplementationIntention, "L_owner", "update_implementation_intention", map[string]any{
		"id": id, "status": "honored",
	})
	if updated.IsError {
		t.Fatalf("resolve intention: %q", resultText(updated))
	}
	intention, _ := decodeResult(t, updated)["intention"].(map[string]any)
	if intention["status"] != models.IntentionStatusHonored || intention["resolved_at"] == nil {
		t.Fatalf("unexpected resolved intention: %v", intention)
	}
}
