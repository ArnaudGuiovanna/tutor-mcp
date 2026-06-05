// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestLoadContextLoadsRecentSessionsAndDetectsSignals(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	if err := Write(WriteRequest{LearnerID: "L1", Scope: ScopeMemoryPending, Operation: OpAppend, Content: "- new observation"}); err != nil {
		t.Fatalf("Write pending: %v", err)
	}
	if err := Write(WriteRequest{LearnerID: "L1", Scope: ScopeConcept, ConceptSlug: "bayes", Operation: OpReplaceSection, SectionKey: "Current state", Content: "Stable but slow."}); err != nil {
		t.Fatalf("Write concept: %v", err)
	}
	for i := 0; i < 4; i++ {
		ts := now.Add(time.Duration(-i) * time.Hour)
		content := `---
timestamp: ` + ts.Format(time.RFC3339) + `
duration_minutes: 20
novelty_flag: true
---

## Summary
Session body ` + ts.Format(time.RFC3339)
		if err := Write(WriteRequest{LearnerID: "L1", Scope: ScopeSession, Timestamp: ts, Operation: OpReplaceFile, Content: content}); err != nil {
			t.Fatalf("Write session %d: %v", i, err)
		}
	}

	ec, err := LoadContext("L1", "bayes", &OLMView{FocusConcept: "other", ConceptBucket: map[string]string{"bayes": "solid"}}, []models.Alert{
		{Type: models.AlertForgetting, Concept: "bayes", Urgency: models.UrgencyCritical, Retention: 0.22},
	})
	if err != nil {
		t.Fatalf("LoadContext: %v", err)
	}
	if len(ec.RecentSessions) != 3 {
		t.Fatalf("recent sessions = %d, want 3", len(ec.RecentSessions))
	}
	if !strings.Contains(ec.ConceptNotes, "Stable but slow.") {
		t.Fatalf("concept notes not loaded: %q", ec.ConceptNotes)
	}
	if !ec.HasRecentNarrativeSignal() {
		t.Fatal("expected recent narrative signal from pending memory or novelty flag")
	}
	if len(ec.OLMInconsistencies) == 0 || ec.OLMInconsistencies[0].Type != "solid_but_forgetting" {
		t.Fatalf("OLM inconsistencies = %#v", ec.OLMInconsistencies)
	}
}

func TestDetectOLMInconsistenciesMultipleCriticalUnsorted(t *testing.T) {
	got := DetectOLMInconsistencies(&OLMView{
		FocusConcept: "less_urgent",
	}, []models.Alert{
		{Type: models.AlertForgetting, Concept: "less_urgent", Urgency: models.UrgencyCritical, Retention: 0.25},
		{Type: models.AlertForgetting, Concept: "most_urgent", Urgency: models.UrgencyCritical, Retention: 0.10},
	})
	found := false
	for _, inc := range got {
		if inc.Type == "multiple_critical_unsorted" && inc.Concept == "most_urgent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing multiple_critical_unsorted in %#v", got)
	}
}

func TestContextBudgetKeepsStableMemoryAndDropsSessionsFirst(t *testing.T) {
	ec := &EpisodicContext{
		LearnerMemory:      strings.Repeat("m", 20),
		ConceptNotes:       strings.Repeat("c", 20),
		OLMInconsistencies: []OLMInconsistency{{Type: "solid_but_forgetting", Concept: "x", Description: "keep"}},
		RecentSessions: []SessionPayload{
			{Body: strings.Repeat("s", 100)},
			{Body: strings.Repeat("s", 100)},
		},
	}
	enforceContextBudget(ec, 80)
	if ec.LearnerMemory == "" || ec.ConceptNotes == "" || len(ec.OLMInconsistencies) == 0 {
		t.Fatalf("stable context was truncated: %#v", ec)
	}
	// Session content is shed before any stable memory: the older session is
	// dropped whole and the surviving one-session floor has its body cleared,
	// leaving no session payload but preserving the most-recent slot.
	if len(ec.RecentSessions) != 1 {
		t.Fatalf("one-session floor should be preserved: %d", len(ec.RecentSessions))
	}
	if ec.RecentSessions[0].Body != "" {
		t.Fatalf("surviving session body should be cleared, got %q", ec.RecentSessions[0].Body)
	}
	if contextSize(ec) > 80 {
		t.Fatalf("size %d exceeds budget 80", contextSize(ec))
	}
}
