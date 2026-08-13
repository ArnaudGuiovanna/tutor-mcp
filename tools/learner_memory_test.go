// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/memory"
	storeport "tutor-mcp/store"
)

type failingMemoryConceptLookupStore struct {
	storeport.Store
	err error
}

func (s *failingMemoryConceptLookupStore) ActiveDomainConceptSet(context.Context, string) (map[string]bool, error) {
	return nil, s.err
}

func TestUpdateLearnerMemory_WriteSessionAndReadRawSession(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "memory-domain")
	session := openTestLearningSession(t, store, "L_owner", "sess_memory", domain.ID)

	ts := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)
	content := `---
session_id: sess_memory
domain_id: ` + domain.ID + `
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
		"scope":      "session",
		"session_id": session.ID,
		"domain_id":  domain.ID,
		"timestamp":  ts.Format(time.RFC3339),
		"content":    content,
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
	frontmatter, _ := payload["frontmatter"].(map[string]any)
	if frontmatter["session_id"] != session.ID {
		t.Fatalf("summary lost durable session ID: %v", frontmatter)
	}
}

func TestUpdateLearnerMemory_RejectsMismatchedOrForeignSession(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "memory-domain-mismatch")
	session := openTestLearningSession(t, store, "L_owner", "sess_owner_memory", domain.ID)
	ts := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)
	content := `---
session_id: wrong_session
domain_id: ` + domain.ID + `
timestamp: 2026-05-14T09:30:00Z
duration_minutes: 30
affect_start: focused
affect_end: satisfied
energy_level: high
concepts_touched: ["a"]
session_type: review
novelty_flag: false
---

## Summary
Signal.`
	mismatch := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope": "session", "session_id": session.ID, "timestamp": ts.Format(time.RFC3339), "content": content,
	})
	if !mismatch.IsError || !strings.Contains(resultText(mismatch), "does not match") {
		t.Fatalf("mismatched summary session accepted: %q", resultText(mismatch))
	}
	foreign := callTool(t, deps, registerUpdateLearnerMemory, "L_attacker", "update_learner_memory", map[string]any{
		"scope": "session", "session_id": session.ID, "timestamp": ts.Format(time.RFC3339), "content": content,
	})
	if !foreign.IsError || !strings.Contains(resultText(foreign), "not found") {
		t.Fatalf("foreign summary session accepted: %q", resultText(foreign))
	}
}

func TestUpdateLearnerMemory_ConceptMustBeActive(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":        "concept",
		"domain_id":    domain.ID,
		"concept_slug": "unknown",
		"section_key":  "Current state",
		"content":      "Observation.",
	})
	if !res.IsError || !strings.Contains(resultText(res), "not part of domain") {
		t.Fatalf("expected active concept validation, got %q", resultText(res))
	}

	ok := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":        "concept",
		"domain_id":    domain.ID,
		"concept_slug": "a",
		"section_key":  "Current state",
		"content":      "Observation.",
	})
	if ok.IsError {
		t.Fatalf("expected active concept write to pass: %s", resultText(ok))
	}
}

func TestUpdateLearnerMemory_ValidationDependencyFailureIsSafe(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	secretPath := "/private/tenant/L_owner/concepts.db"
	deps.Store = &failingMemoryConceptLookupStore{
		Store: store,
		err:   &os.PathError{Op: "open", Path: secretPath, Err: os.ErrPermission},
	}

	res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":        "concept",
		"concept_slug": "a",
		"section_key":  "Current state",
		"content":      "Observation.",
	})
	if !res.IsError || resultText(res) != "memory validation unavailable" {
		t.Fatalf("dependency failure was not surfaced safely: error=%v text=%q", res.IsError, resultText(res))
	}
	if strings.Contains(resultText(res), secretPath) {
		t.Fatalf("dependency failure leaked a path: %q", resultText(res))
	}
}

func TestUpdateLearnerMemory_WriteFailureIsSafeAndQuotaIsStable(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)
	originalWrite := writeLearnerMemory
	t.Cleanup(func() { writeLearnerMemory = originalWrite })

	secretPath := "/private/tenant/L_owner/MEMORY.md"
	writeLearnerMemory = func(memory.WriteRequest) error {
		return &os.PathError{Op: "rename", Path: secretPath, Err: os.ErrPermission}
	}
	res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope": "memory_pending", "operation": "append", "content": "- observation",
	})
	if !res.IsError || resultText(res) != "memory write failed" {
		t.Fatalf("write failure was not surfaced safely: error=%v text=%q", res.IsError, resultText(res))
	}
	if strings.Contains(resultText(res), secretPath) {
		t.Fatalf("write failure leaked a path: %q", resultText(res))
	}

	writeLearnerMemory = func(memory.WriteRequest) error {
		return fmt.Errorf("%w: private limit details", memory.ErrQuotaExceeded)
	}
	quota := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope": "memory_pending", "operation": "append", "content": "- observation",
	})
	if !quota.IsError || resultText(quota) != "memory quota exceeded" {
		t.Fatalf("quota failure = error=%v text=%q", quota.IsError, resultText(quota))
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

func TestMemoryTools_SharedBackendReplaysAppendWithoutLocalFiles(t *testing.T) {
	localRoot := filepath.Join(t.TempDir(), "must-remain-absent")
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", localRoot)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	encodedKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	keyring, err := db.NewIntegrationSecretKeyring("narrative:"+encodedKey, "narrative")
	if err != nil {
		t.Fatalf("build narrative keyring: %v", err)
	}
	store.SetIntegrationSecretKeyring(keyring)
	memory.ConfigureNarrativeStore(store)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })

	args := map[string]any{
		"scope":           "memory_pending",
		"operation":       "append",
		"content":         "- one shared observation",
		"idempotency_key": "shared-tool-retry-1",
	}
	for attempt := 0; attempt < 2; attempt++ {
		res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", args)
		if res.IsError {
			t.Fatalf("shared update attempt %d failed: %s", attempt+1, resultText(res))
		}
	}
	content, err := memory.Read("L_owner", memory.ScopeMemoryPending, "")
	if err != nil || strings.Count(content, "one shared observation") != 1 {
		t.Fatalf("shared replay content=%q err=%v", content, err)
	}

	state := callTool(t, deps, registerGetMemoryState, "L_owner", "get_memory_state", map[string]any{})
	if state.IsError {
		t.Fatalf("shared get_memory_state failed: %s", resultText(state))
	}
	if out := decodeResult(t, state); out["pending_count"] != float64(1) || out["memory_size_bytes"] == float64(0) {
		t.Fatalf("unexpected shared memory state: %v", out)
	}
	if _, err := os.Stat(localRoot); !os.IsNotExist(err) {
		t.Fatalf("shared backend unexpectedly touched local root: %v", err)
	}

	var ciphertext string
	if err := store.RawDB().QueryRow(
		`SELECT ciphertext FROM narrative_objects
		 WHERE learner_id = ? AND scope = ? AND domain_id = '' AND object_key = ''`,
		"L_owner", string(memory.ScopeMemoryPending),
	).Scan(&ciphertext); err != nil {
		t.Fatalf("read shared ciphertext: %v", err)
	}
	if strings.Contains(ciphertext, "one shared observation") {
		t.Fatal("shared database exposed narrative plaintext")
	}
}

func TestGetMemoryState_RequiredListingFailureIsSafe(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)

	original := listLearnerMemorySessions
	t.Cleanup(func() { listLearnerMemorySessions = original })
	secretPath := "/private/tenant/L_owner/sessions"
	listLearnerMemorySessions = func(string) ([]time.Time, error) {
		return nil, &os.PathError{Op: "readdir", Path: secretPath, Err: os.ErrPermission}
	}

	res := callTool(t, deps, registerGetMemoryState, "L_owner", "get_memory_state", map[string]any{})
	if !res.IsError || resultText(res) != "memory state unavailable" {
		t.Fatalf("listing failure was not surfaced safely: error=%v text=%q", res.IsError, resultText(res))
	}
	if strings.Contains(resultText(res), secretPath) {
		t.Fatalf("listing failure leaked a filesystem path: %q", resultText(res))
	}
}

func TestGetMemoryState_RequiredPendingReadFailureIsSafe(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)

	original := readLearnerMemory
	t.Cleanup(func() { readLearnerMemory = original })
	secretPath := "/private/tenant/L_owner/MEMORY_pending.md"
	readLearnerMemory = func(learnerID string, scope memory.Scope, key string) (string, error) {
		if scope == memory.ScopeMemoryPending {
			return "", &os.PathError{Op: "read", Path: secretPath, Err: errors.New("storage unavailable")}
		}
		return original(learnerID, scope, key)
	}

	res := callTool(t, deps, registerGetMemoryState, "L_owner", "get_memory_state", map[string]any{})
	if !res.IsError || resultText(res) != "memory state unavailable" {
		t.Fatalf("pending-memory failure was not surfaced safely: error=%v text=%q", res.IsError, resultText(res))
	}
	if strings.Contains(resultText(res), secretPath) {
		t.Fatalf("pending-memory failure leaked a filesystem path: %q", resultText(res))
	}
}

func TestGetMemoryState_StatFailureDegradesOptionalFields(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)
	learnerID := "L_owner"
	now := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	if err := memory.Write(memory.WriteRequest{
		LearnerID: learnerID,
		Scope:     memory.ScopeSession,
		Timestamp: now,
		Operation: memory.OpReplaceFile,
		Content:   "A valid legacy session summary.",
	}); err != nil {
		t.Fatalf("seed session memory: %v", err)
	}
	if err := memory.Write(memory.WriteRequest{
		LearnerID: learnerID,
		Scope:     memory.ScopeArchive,
		Period:    "2026",
		Operation: memory.OpReplaceFile,
		Content:   "A valid archive.",
	}); err != nil {
		t.Fatalf("seed archive memory: %v", err)
	}

	original := statLearnerMemoryPath
	t.Cleanup(func() { statLearnerMemoryPath = original })
	secretPath := "/private/tenant/L_owner/MEMORY.md"
	statLearnerMemoryPath = func(string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "lstat", Path: secretPath, Err: os.ErrPermission}
	}

	res := callTool(t, deps, registerGetMemoryState, learnerID, "get_memory_state", map[string]any{})
	if res.IsError {
		t.Fatalf("optional stat failure should be degraded, got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["ok"] != true || out["status"] != "degraded" {
		t.Fatalf("unexpected degraded response: %v", out)
	}
	if out["memory_size_bytes"] != nil || out["consolidation_lag_days"] != nil || out["has_recent_narrative_signal"] != nil {
		t.Fatalf("unavailable optional values must be null, not zero/false: %v", out)
	}
	assertMemoryDegradedComponents(t, out, "memory_statistics", "consolidation_lag", "narrative_signal")
	if strings.Contains(resultText(res), secretPath) {
		t.Fatalf("stat failure leaked a filesystem path: %q", resultText(res))
	}
}

func TestGetMemoryState_CorruptSessionDegradesNarrativeContext(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)
	learnerID := "L_owner"
	ts := time.Now().UTC().Truncate(time.Second)
	if err := memory.EnsureLearnerDirs(learnerID); err != nil {
		t.Fatalf("ensure learner memory: %v", err)
	}
	path, err := memory.PathForRead(learnerID, memory.ScopeSession, ts.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("session path: %v", err)
	}
	if err := os.WriteFile(path, []byte("---\ntimestamp: [\n---\ncorrupt"), 0o600); err != nil {
		t.Fatalf("write corrupt session: %v", err)
	}

	res := callTool(t, deps, registerGetMemoryState, learnerID, "get_memory_state", map[string]any{})
	if res.IsError {
		t.Fatalf("narrative parsing is optional to inventory: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["status"] != "degraded" || out["session_count"] != float64(1) {
		t.Fatalf("corruption must preserve inventory with an explicit degradation: %v", out)
	}
	if out["has_recent_narrative_signal"] != nil {
		t.Fatalf("corrupt narrative must not become a false signal: %v", out)
	}
	assertMemoryDegradedComponents(t, out, "narrative_context")
	if strings.Contains(resultText(res), path) {
		t.Fatalf("corruption response leaked a filesystem path: %q", resultText(res))
	}
}

func TestGetMemoryState_AbsentMemoryIsHealthy(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)

	res := callTool(t, deps, registerGetMemoryState, "L_owner", "get_memory_state", map[string]any{})
	if res.IsError {
		t.Fatalf("normal memory absence failed: %q", resultText(res))
	}
	out := decodeResult(t, res)
	for _, field := range []string{"memory_size_bytes", "pending_count", "session_count", "archive_count", "concept_count", "consolidation_lag_days"} {
		if out[field] != float64(0) {
			t.Fatalf("%s = %v, want healthy zero: %v", field, out[field], out)
		}
	}
	if out["has_recent_narrative_signal"] != false {
		t.Fatalf("empty memory should have no narrative signal: %v", out)
	}
	if _, degraded := out["degraded_components"]; degraded {
		t.Fatalf("normal absence must not be marked degraded: %v", out)
	}
}

func TestReadRawSession_FailureIsSafeAndAbsenceIsNormal(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)
	timestamp := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC).Format(time.RFC3339)

	absent := callTool(t, deps, registerReadRawSession, "L_owner", "read_raw_session", map[string]any{"timestamp": timestamp})
	if absent.IsError {
		t.Fatalf("normal session absence failed: %q", resultText(absent))
	}
	if out := decodeResult(t, absent); out["ok"] != true || out["session_payload"] != nil {
		t.Fatalf("unexpected absent-session response: %v", out)
	}

	original := readLearnerMemory
	t.Cleanup(func() { readLearnerMemory = original })
	secretContent := "private learner content"
	readLearnerMemory = func(string, memory.Scope, string) (string, error) {
		return "---\ntimestamp: [" + secretContent + "\n---\ncorrupt", nil
	}
	corrupt := callTool(t, deps, registerReadRawSession, "L_owner", "read_raw_session", map[string]any{"timestamp": timestamp})
	if !corrupt.IsError || resultText(corrupt) != "memory session is invalid" {
		t.Fatalf("corrupt session was not surfaced safely: error=%v text=%q", corrupt.IsError, resultText(corrupt))
	}
	if strings.Contains(resultText(corrupt), secretContent) {
		t.Fatalf("session parse failure leaked learner content: %q", resultText(corrupt))
	}

	secretPath := "/private/tenant/L_owner/sessions/secret.md"
	readLearnerMemory = func(string, memory.Scope, string) (string, error) {
		return "", &os.PathError{Op: "read", Path: secretPath, Err: os.ErrPermission}
	}
	failed := callTool(t, deps, registerReadRawSession, "L_owner", "read_raw_session", map[string]any{"timestamp": timestamp})
	if !failed.IsError || resultText(failed) != "memory session unavailable" {
		t.Fatalf("session read failure was not surfaced safely: error=%v text=%q", failed.IsError, resultText(failed))
	}
	if strings.Contains(resultText(failed), secretPath) {
		t.Fatalf("session read failure leaked a filesystem path: %q", resultText(failed))
	}
}

func assertMemoryDegradedComponents(t *testing.T, out map[string]any, want ...string) {
	t.Helper()
	raw, ok := out["degraded_components"].([]any)
	if !ok {
		t.Fatalf("degraded_components missing from %v", out)
	}
	got := make(map[string]bool, len(raw))
	for _, component := range raw {
		name, _ := component.(string)
		got[name] = true
	}
	for _, component := range want {
		if !got[component] {
			t.Fatalf("degraded component %q missing from %v", component, out)
		}
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
