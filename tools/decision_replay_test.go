// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestGetDecisionReplaySummary_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetDecisionReplaySummary, "", "get_decision_replay_summary", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
	if !strings.Contains(resultText(res), "authentication") {
		t.Fatalf("expected authentication required, got %q", resultText(res))
	}
}

func TestGetDecisionReplaySummary_ReturnsAuditSummary(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")

	interaction := &models.Interaction{
		LearnerID:    "L_owner",
		Concept:      "a",
		ActivityType: string(models.ActivityPractice),
		Success:      true,
		DomainID:     domain.ID,
	}
	if err := store.CreateInteraction(context.Background(), interaction); err != nil {
		t.Fatalf("create interaction: %v", err)
	}
	if err := store.CreatePedagogicalSnapshot(context.Background(), &models.PedagogicalSnapshot{
		InteractionID:   interaction.ID,
		LearnerID:       "L_owner",
		DomainID:        domain.ID,
		Concept:         "a",
		ActivityType:    string(models.ActivityPractice),
		BeforeJSON:      `{"p_mastery":0.2}`,
		ObservationJSON: `{"success":true}`,
		AfterJSON:       `{"p_mastery":0.9}`,
		DecisionJSON:    `{"source":"test"}`,
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	res := callTool(t, deps, registerGetDecisionReplaySummary, "L_owner", "get_decision_replay_summary", map[string]any{
		"domain_id": domain.ID,
		"concept":   "a",
	})
	if res.IsError {
		t.Fatalf("get_decision_replay_summary failed: %s", resultText(res))
	}

	out := decodeResult(t, res)
	summary, ok := out["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary = %T, want map", out["summary"])
	}
	if got := summary["total_snapshots"]; got != float64(1) {
		t.Fatalf("total_snapshots = %v, want 1", got)
	}
	if got := summary["suspicious_jump_count"]; got != float64(1) {
		t.Fatalf("suspicious_jump_count = %v, want 1", got)
	}
	if got := summary["missing_rubric_evidence_count"]; got != float64(1) {
		t.Fatalf("missing_rubric_evidence_count = %v, want 1", got)
	}
}

func TestGetDecisionReplaySummary_RejectsForeignDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	foreign := makeOwnerDomain(t, store, "L_attacker", "math")

	res := callTool(t, deps, registerGetDecisionReplaySummary, "L_owner", "get_decision_replay_summary", map[string]any{
		"domain_id": foreign.ID,
	})
	if !res.IsError {
		t.Fatalf("expected foreign domain rejection, got %q", resultText(res))
	}
}

func TestBoundedDecisionReplaySnapshotsLimit(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{-1, defaultDecisionReplaySnapshotsLimit},
		{0, defaultDecisionReplaySnapshotsLimit},
		{7, 7},
		{maxDecisionReplaySnapshotsLimit + 1, maxDecisionReplaySnapshotsLimit},
	}

	for _, tc := range cases {
		if got := boundedDecisionReplaySnapshotsLimit(tc.in); got != tc.want {
			t.Fatalf("boundedDecisionReplaySnapshotsLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
