// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestUpdateLearnerMemory_WriteSessionAndReadRawSession(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)

	ts := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)
	content := `---
timestamp: 2026-05-14T09:30:00Z
duration_minutes: 30
affect_start: focused
affect_end: satisfied
energy_level: high
concepts_touched: ["a"]
session_type: review
novelty_flag: true
---

## Summary
The learner built a first retrieval cue.`
	res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":     "session",
		"timestamp": ts.Format(time.RFC3339),
		"content":   content,
	})
	if res.IsError {
		t.Fatalf("update_learner_memory failed: %s", resultText(res))
	}
	out := decodeResult(t, res)
	if out["ok"] != true || out["bytes_written"].(float64) == 0 {
		t.Fatalf("unexpected update response: %v", out)
	}

	read := callTool(t, deps, registerReadRawSession, "L_owner", "read_raw_session", map[string]any{
		"timestamp": ts.Format(time.RFC3339),
	})
	if read.IsError {
		t.Fatalf("read_raw_session failed: %s", resultText(read))
	}
	readOut := decodeResult(t, read)
	payload, ok := readOut["session_payload"].(map[string]any)
	if !ok {
		t.Fatalf("session_payload = %T, want map", readOut["session_payload"])
	}
	body, _ := payload["body"].(string)
	if !strings.Contains(body, "retrieval cue") {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestUpdateLearnerMemory_ConceptMustBeActive(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":        "concept",
		"concept_slug": "unknown",
		"section_key":  "Current state",
		"content":      "Observation.",
	})
	if !res.IsError || !strings.Contains(resultText(res), "active concept") {
		t.Fatalf("expected active concept validation, got %q", resultText(res))
	}

	ok := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":        "concept",
		"concept_slug": "a",
		"section_key":  "Current state",
		"content":      "Observation.",
	})
	if ok.IsError {
		t.Fatalf("expected active concept write to pass: %s", resultText(ok))
	}
}

func TestUpdateLearnerMemory_ArchiveMarksConsolidationCompleted(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	now := time.Date(2026, time.May, 3, 13, 30, 0, 0, time.UTC)
	if err := store.UpsertPendingConsolidation(context.Background(), "L_owner", "monthly", "2026-04", now); err != nil {
		t.Fatalf("UpsertPendingConsolidation: %v", err)
	}

	res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":       "archive",
		"period_type": "monthly",
		"period_key":  "2026-04",
		"content":     "# Consolidation 2026-04\n\n## Period trajectory\nStable progress.",
	})
	if res.IsError {
		t.Fatalf("update_learner_memory archive failed: %s", resultText(res))
	}
	item, err := store.GetConsolidation(context.Background(), "L_owner", "monthly", "2026-04")
	if err != nil {
		t.Fatalf("GetConsolidation: %v", err)
	}
	if item.Status != "completed" || item.CompletedAt == nil {
		t.Fatalf("archive write should complete consolidation, got %#v", item)
	}
}

func TestGetMemoryState_ReturnsCounts(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)
	ts := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)
	_ = callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":     "session",
		"timestamp": ts.Format(time.RFC3339),
		"content":   "---\ntimestamp: 2026-05-14T09:30:00Z\nduration_minutes: 30\naffect_start: focused\naffect_end: satisfied\nenergy_level: high\nconcepts_touched: [\"a\"]\nsession_type: review\nnovelty_flag: true\n---\n\n## Summary\nSignal.",
	})
	_ = callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":   "memory_pending",
		"content": "- one pending item",
	})

	res := callTool(t, deps, registerGetMemoryState, "L_owner", "get_memory_state", map[string]any{})
	if res.IsError {
		t.Fatalf("get_memory_state failed: %s", resultText(res))
	}
	out := decodeResult(t, res)
	if out["session_count"] != float64(1) || out["pending_count"] != float64(1) {
		t.Fatalf("unexpected memory state: %v", out)
	}
	if out["has_recent_narrative_signal"] != true {
		t.Fatalf("expected narrative signal: %v", out)
	}
}

func TestMemoryTools_NotEnabled(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "false")
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":   "memory_pending",
		"content": "ignored",
	})
	if res.IsError {
		t.Fatalf("expected structured not_enabled response, got %s", resultText(res))
	}
	out := decodeResult(t, res)
	if out["ok"] != false || out["status"] != "not_enabled" {
		t.Fatalf("unexpected response: %v", out)
	}
}
