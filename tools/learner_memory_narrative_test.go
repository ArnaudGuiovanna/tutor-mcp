// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"encoding/base64"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/memory"
)

// The rest of the memory suite runs on the file backend, but --local and every
// server profile configure the database narrative store. These tests cover the
// backend that actually runs in production.
func withNarrativeStore(t *testing.T, store *db.Store) {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := db.NewIntegrationSecretKeyring("narrative:"+key, "narrative")
	if err != nil {
		t.Fatalf("build narrative keyring: %v", err)
	}
	store.SetIntegrationSecretKeyring(keyring)
	memory.ConfigureNarrativeStore(store)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
}

func sessionSummaryContent(sessionID, domainID string) string {
	return `---
session_id: ` + sessionID + `
domain_id: ` + domainID + `
timestamp: 2026-09-18T12:10:42Z
duration_minutes: 16
affect_start: energized
affect_end: satisfied
energy_level: medium
concepts_touched: ["error_handling"]
session_type: first_pass
novelty_flag: true
---

## Summary
First session in a new domain.`
}

// memory/prompts.go tells the client to pass domain_id on a session summary.
// The file backend ignores it; the narrative store used to reject the whole
// write, so every session summary failed on a real deployment.
func TestUpdateLearnerMemorySessionAcceptsDomainIDOnNarrativeStore(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	withNarrativeStore(t, store)

	domain := makeOwnerDomain(t, store, "L_owner", "memory-domain")
	session := openTestLearningSession(t, store, "L_owner", "sess_narrative", domain.ID)
	ts := time.Date(2026, 9, 18, 12, 10, 42, 0, time.UTC)

	res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":      "session",
		"session_id": session.ID,
		"domain_id":  domain.ID,
		"timestamp":  ts.Format(time.RFC3339),
		"operation":  "replace_file",
		"content":    sessionSummaryContent(session.ID, domain.ID),
	})
	if res.IsError {
		t.Fatalf("session summary rejected on the narrative store: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["ok"] != true {
		t.Fatalf("unexpected update response: %v", out)
	}
}

// A session summary is learner-scoped in both backends: passing a domain must
// not change where it lands.
func TestSessionNarrativeKeyIgnoresDomainID(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	store, deps := setupToolsTest(t)
	withNarrativeStore(t, store)

	domain := makeOwnerDomain(t, store, "L_owner", "memory-domain")
	session := openTestLearningSession(t, store, "L_owner", "sess_key", domain.ID)
	ts := time.Date(2026, 9, 18, 12, 10, 42, 0, time.UTC)
	args := map[string]any{
		"scope":      "session",
		"session_id": session.ID,
		"domain_id":  domain.ID,
		"timestamp":  ts.Format(time.RFC3339),
		"operation":  "replace_file",
		"content":    sessionSummaryContent(session.ID, domain.ID),
	}
	if res := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", args); res.IsError {
		t.Fatalf("first write rejected: %q", resultText(res))
	}

	// Written with domain_id, read back without it.
	read := callTool(t, deps, registerReadRawSession, "L_owner", "read_raw_session", map[string]any{
		"timestamp": ts.Format(time.RFC3339),
	})
	if read.IsError {
		t.Fatalf("session summary not readable without domain_id: %q", resultText(read))
	}
}
