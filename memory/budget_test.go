// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"strings"
	"testing"
)

// TestEnforceContextBudgetUnderBudgetUnchanged verifies that a context already
// within budget is left completely untouched.
func TestEnforceContextBudgetUnderBudgetUnchanged(t *testing.T) {
	ec := &EpisodicContext{
		LearnerMemory: strings.Repeat("m", 100),
		PendingMemory: strings.Repeat("p", 50),
		ConceptNotes:  strings.Repeat("c", 50),
		RecentSessions: []SessionPayload{
			{Body: strings.Repeat("s", 100), Frontmatter: map[string]any{"k": "v"}},
			{Body: strings.Repeat("s", 100), Frontmatter: map[string]any{"k": "v"}},
		},
		RecentArchives: []ArchivePayload{
			{Period: "2026-04", Body: strings.Repeat("a", 100)},
		},
		OLMInconsistencies: []OLMInconsistency{{Type: "t", Concept: "x", Description: "d"}},
	}
	before := contextSize(ec)
	enforceContextBudget(ec, contextBudgetBytes)

	if contextSize(ec) != before {
		t.Fatalf("size changed: before=%d after=%d", before, contextSize(ec))
	}
	if len(ec.RecentSessions) != 2 {
		t.Fatalf("sessions altered: %d", len(ec.RecentSessions))
	}
	if len(ec.RecentArchives) != 1 {
		t.Fatalf("archives altered: %d", len(ec.RecentArchives))
	}
	if len(ec.RecentSessions[0].Frontmatter) != 1 || ec.RecentSessions[0].Body == "" {
		t.Fatalf("session payload altered: %#v", ec.RecentSessions[0])
	}
}

// TestEnforceContextBudgetEvictionOrder walks the full progressive-eviction
// ladder: drop trailing sessions down to a one-session floor, then trailing
// archives, then frontmatter on the surviving session(s), then their bodies.
// The most recent session always survives (floor) and stable memory is never
// touched.
func TestEnforceContextBudgetEvictionOrder(t *testing.T) {
	// Stable, never-evicted payload: 200 bytes.
	stableMemory := strings.Repeat("m", 200)

	newEC := func() *EpisodicContext {
		return &EpisodicContext{
			LearnerMemory: stableMemory,
			RecentSessions: []SessionPayload{
				{Body: strings.Repeat("s", 100), Frontmatter: map[string]any{"key": strings.Repeat("v", 100)}},
				{Body: strings.Repeat("s", 100), Frontmatter: map[string]any{"key": strings.Repeat("v", 100)}},
				{Body: strings.Repeat("s", 100), Frontmatter: map[string]any{"key": strings.Repeat("v", 100)}},
			},
			RecentArchives: []ArchivePayload{
				{Period: "p1", Body: strings.Repeat("a", 100)},
				{Period: "p2", Body: strings.Repeat("a", 100)},
			},
		}
	}

	// Each session contributes 100 (body) + 3 (key) + 100 (value) = 203 bytes.
	// Each archive contributes 100 bytes. Stable memory is 200.
	// Total = 200 + 3*203 + 2*100 = 1009.

	// Stage 1: budget that only requires dropping a trailing session.
	// Drop one session (1009 -> 806). Budget 850 keeps 2 sessions, both archives,
	// all frontmatter and bodies intact.
	t.Run("drops trailing sessions first", func(t *testing.T) {
		ec := newEC()
		enforceContextBudget(ec, 850)
		if len(ec.RecentSessions) != 2 {
			t.Fatalf("expected 2 sessions, got %d", len(ec.RecentSessions))
		}
		if len(ec.RecentArchives) != 2 {
			t.Fatalf("archives should be untouched, got %d", len(ec.RecentArchives))
		}
		for i, s := range ec.RecentSessions {
			if len(s.Frontmatter) == 0 || s.Body == "" {
				t.Fatalf("session %d should keep frontmatter and body: %#v", i, s)
			}
		}
		if contextSize(ec) > 850 {
			t.Fatalf("size %d exceeds budget 850", contextSize(ec))
		}
	})

	// Stage 2: budget low enough to hit the one-session floor, then drop archives.
	// Sessions drop to 1 (1009 -> 806 -> 603), then archives drop one at a time
	// (603 -> 503). Budget 550: floor keeps 1 session, one archive remains, the
	// surviving session keeps frontmatter and body.
	t.Run("keeps a one-session floor then drops archives", func(t *testing.T) {
		ec := newEC()
		enforceContextBudget(ec, 550)
		if len(ec.RecentSessions) != 1 {
			t.Fatalf("floor should keep exactly 1 session, got %d", len(ec.RecentSessions))
		}
		if len(ec.RecentArchives) != 1 {
			t.Fatalf("expected 1 archive left, got %d", len(ec.RecentArchives))
		}
		if ec.RecentSessions[0].Body == "" || len(ec.RecentSessions[0].Frontmatter) == 0 {
			t.Fatalf("survivor should still hold frontmatter and body: %#v", ec.RecentSessions[0])
		}
		if ec.LearnerMemory != stableMemory {
			t.Fatal("stable memory must never be evicted")
		}
		if contextSize(ec) > 550 {
			t.Fatalf("size %d exceeds budget 550", contextSize(ec))
		}
	})

	// Stage 3: budget low enough that, after the floor + archive drops, the
	// surviving session's frontmatter must be trimmed (but not its body).
	// 1009 -> 603 (floor) -> 403 (archives gone) -> 300 (frontmatter cleared).
	t.Run("trims survivor frontmatter before its body", func(t *testing.T) {
		ec := newEC()
		enforceContextBudget(ec, 350)
		if len(ec.RecentSessions) != 1 {
			t.Fatalf("floor should keep exactly 1 session, got %d", len(ec.RecentSessions))
		}
		if len(ec.RecentArchives) != 0 {
			t.Fatalf("archives should be exhausted, got %d", len(ec.RecentArchives))
		}
		if len(ec.RecentSessions[0].Frontmatter) != 0 {
			t.Fatalf("survivor frontmatter should be cleared: %#v", ec.RecentSessions[0].Frontmatter)
		}
		if ec.RecentSessions[0].Body == "" {
			t.Fatal("survivor body should still be intact at this budget")
		}
		if contextSize(ec) > 350 {
			t.Fatalf("size %d exceeds budget 350", contextSize(ec))
		}
	})

	// Stage 4: budget low enough that even the survivor's body must be cleared.
	// 1009 -> 603 (floor) -> 403 (archives gone) -> 300 (frontmatter) -> 200 (body).
	t.Run("clears survivor body as the last graduated stage", func(t *testing.T) {
		ec := newEC()
		enforceContextBudget(ec, 250)
		if len(ec.RecentSessions) != 1 {
			t.Fatalf("floor should keep exactly 1 session, got %d", len(ec.RecentSessions))
		}
		if len(ec.RecentSessions[0].Frontmatter) != 0 || ec.RecentSessions[0].Body != "" {
			t.Fatalf("survivor should be fully trimmed: %#v", ec.RecentSessions[0])
		}
		if ec.LearnerMemory != stableMemory {
			t.Fatal("stable memory must never be evicted")
		}
		if contextSize(ec) > 250 {
			t.Fatalf("size %d exceeds budget 250", contextSize(ec))
		}
	})

	// Stage 5: stable memory alone exceeds the budget. The ladder trims the
	// survivor down to nothing (frontmatter + body cleared) but never drops it
	// below the floor and never evicts stable memory, so the final size stays
	// over budget. This is the intentional limit of the policy.
	t.Run("survivor floor and stable memory cannot be evicted below budget", func(t *testing.T) {
		ec := &EpisodicContext{
			LearnerMemory: strings.Repeat("m", 500), // exceeds budget alone
			RecentSessions: []SessionPayload{
				{Body: strings.Repeat("s", 100), Frontmatter: map[string]any{"key": strings.Repeat("v", 100)}},
				{Body: strings.Repeat("s", 100), Frontmatter: map[string]any{"key": strings.Repeat("v", 100)}},
			},
		}
		enforceContextBudget(ec, 300)
		if len(ec.RecentSessions) != 1 {
			t.Fatalf("the one-session floor must be preserved, got %d", len(ec.RecentSessions))
		}
		if len(ec.RecentSessions[0].Frontmatter) != 0 || ec.RecentSessions[0].Body != "" {
			t.Fatalf("survivor should be fully trimmed when over budget: %#v", ec.RecentSessions[0])
		}
		if len(ec.LearnerMemory) != 500 {
			t.Fatal("stable memory must never be evicted, even when it alone exceeds budget")
		}
		// Final size still exceeds budget: stable memory (500) + empty survivor.
		if contextSize(ec) <= 300 {
			t.Fatalf("expected residual over-budget size from un-evictable stable memory, got %d", contextSize(ec))
		}
	})
}

// TestEnforceContextBudgetFinalSizeWithinBudget builds an oversized context and
// asserts the eviction reduces it within budget while preserving stable memory.
func TestEnforceContextBudgetFinalSizeWithinBudget(t *testing.T) {
	ec := &EpisodicContext{
		LearnerMemory: strings.Repeat("m", 10*1024),
		PendingMemory: strings.Repeat("p", 5*1024),
		ConceptNotes:  strings.Repeat("c", 5*1024),
		RecentSessions: []SessionPayload{
			{Body: strings.Repeat("s", 20*1024), Frontmatter: map[string]any{"k": strings.Repeat("v", 1024)}},
			{Body: strings.Repeat("s", 20*1024), Frontmatter: map[string]any{"k": strings.Repeat("v", 1024)}},
			{Body: strings.Repeat("s", 20*1024), Frontmatter: map[string]any{"k": strings.Repeat("v", 1024)}},
		},
		RecentArchives: []ArchivePayload{
			{Period: "p1", Body: strings.Repeat("a", 15*1024)},
			{Period: "p2", Body: strings.Repeat("a", 15*1024)},
		},
	}
	if contextSize(ec) <= contextBudgetBytes {
		t.Fatalf("test precondition: context should exceed budget, got %d", contextSize(ec))
	}

	enforceContextBudget(ec, contextBudgetBytes)

	if got := contextSize(ec); got > contextBudgetBytes {
		t.Fatalf("final size %d exceeds budget %d", got, contextBudgetBytes)
	}
	// Stable memory (20 KB) fits within the 40 KB budget and must survive.
	if len(ec.LearnerMemory) != 10*1024 || len(ec.PendingMemory) != 5*1024 || len(ec.ConceptNotes) != 5*1024 {
		t.Fatal("stable memory was evicted")
	}
}
