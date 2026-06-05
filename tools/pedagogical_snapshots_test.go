// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestGetPedagogicalSnapshots_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetPedagogicalSnapshots, "", "get_pedagogical_snapshots", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
	if !strings.Contains(resultText(res), "authentication") {
		t.Fatalf("expected authentication required, got %q", resultText(res))
	}
}

func TestGetPedagogicalSnapshots_ReturnsLearnerSnapshots(t *testing.T) {
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
		InteractionID:       interaction.ID,
		LearnerID:           "L_owner",
		DomainID:            domain.ID,
		Concept:             "a",
		ActivityType:        string(models.ActivityPractice),
		BeforeJSON:          `{"p_mastery":0.1}`,
		ObservationJSON:     `{"success":true}`,
		AfterJSON:           `{"p_mastery":0.3}`,
		DecisionJSON:        `{"source":"test"}`,
		InterpretationBrief: "The learner is ready for a narrow recall cue.",
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	res := callTool(t, deps, registerGetPedagogicalSnapshots, "L_owner", "get_pedagogical_snapshots", map[string]any{
		"domain_id": domain.ID,
		"concept":   "a",
		"limit":     5,
	})
	if res.IsError {
		t.Fatalf("get_pedagogical_snapshots failed: %s", resultText(res))
	}

	out := decodeResult(t, res)
	rawSnapshots, ok := out["snapshots"].([]any)
	if !ok {
		t.Fatalf("snapshots = %T, want []any", out["snapshots"])
	}
	if len(rawSnapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(rawSnapshots))
	}
	first := rawSnapshots[0].(map[string]any)
	if first["concept"] != "a" {
		t.Fatalf("concept = %v, want a", first["concept"])
	}
	if first["interpretation_brief"] != "The learner is ready for a narrow recall cue." {
		t.Fatalf("interpretation_brief = %v", first["interpretation_brief"])
	}

	before := first["before"].(map[string]any)
	if before["p_mastery"] != 0.1 {
		t.Fatalf("before p_mastery = %v, want 0.1", before["p_mastery"])
	}
}

func TestGetPedagogicalSnapshots_RejectsForeignDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	foreign := makeOwnerDomain(t, store, "L_attacker", "math")

	res := callTool(t, deps, registerGetPedagogicalSnapshots, "L_owner", "get_pedagogical_snapshots", map[string]any{
		"domain_id": foreign.ID,
	})
	if !res.IsError {
		t.Fatalf("expected foreign domain rejection, got %q", resultText(res))
	}
}

func TestBoundedPedagogicalSnapshotsLimit(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{-1, defaultPedagogicalSnapshotsLimit},
		{0, defaultPedagogicalSnapshotsLimit},
		{7, 7},
		{maxPedagogicalSnapshotsLimit + 1, maxPedagogicalSnapshotsLimit},
	}

	for _, tc := range cases {
		if got := boundedPedagogicalSnapshotsLimit(tc.in); got != tc.want {
			t.Fatalf("boundedPedagogicalSnapshotsLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
