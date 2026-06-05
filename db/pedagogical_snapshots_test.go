// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestPedagogicalSnapshots_CreateAndFilter(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	interaction := &models.Interaction{
		LearnerID:    "L1",
		Concept:      "fractions",
		ActivityType: string(models.ActivityPractice),
		Success:      true,
		DomainID:     "domain-1",
		CreatedAt:    now,
	}
	if err := store.CreateInteraction(context.Background(), interaction); err != nil {
		t.Fatalf("create interaction: %v", err)
	}

	snapshot := &models.PedagogicalSnapshot{
		InteractionID:       interaction.ID,
		LearnerID:           "L1",
		DomainID:            "domain-1",
		Concept:             "fractions",
		ActivityType:        string(models.ActivityPractice),
		BeforeJSON:          `{"p_mastery":0.4}`,
		ObservationJSON:     `{"success":true}`,
		AfterJSON:           `{"p_mastery":0.6}`,
		DecisionJSON:        `{"source":"test"}`,
		InterpretationBrief: "The learner likely confuses part-whole and ratio meanings.",
		CreatedAt:           now,
	}
	if err := store.CreatePedagogicalSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if snapshot.ID == 0 {
		t.Fatal("expected snapshot id to be populated")
	}

	got, err := store.GetPedagogicalSnapshots(context.Background(), "L1", "domain-1", "fractions", 10)
	if err != nil {
		t.Fatalf("get snapshots: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(got))
	}
	if got[0].InteractionID != interaction.ID || got[0].BeforeJSON != snapshot.BeforeJSON || got[0].InterpretationBrief != snapshot.InterpretationBrief {
		t.Fatalf("unexpected snapshot: %+v", got[0])
	}

	empty, err := store.GetPedagogicalSnapshots(context.Background(), "L1", "domain-1", "other", 10)
	if err != nil {
		t.Fatalf("get filtered snapshots: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("got %d snapshots for unrelated concept, want 0", len(empty))
	}
}
