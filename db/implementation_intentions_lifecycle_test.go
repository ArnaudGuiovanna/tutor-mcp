// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestImplementationIntentionLifecycleOwnershipAndIdempotency(t *testing.T) {
	s := setupTestDB(t)
	seedLearner(t, s, "L2")
	ctx := context.Background()
	session, err := s.OpenLearningSession(ctx, "L1", "", "sess_intention", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.InsertImplementationIntentionForSession(
		ctx, "L1", "", session.ID, "after breakfast", "practice fractions", time.Now().UTC().Add(24*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert intention: %v", err)
	}
	pending, err := s.GetImplementationIntention(ctx, "L1", id)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != models.IntentionStatusPending || pending.SessionID != session.ID || pending.ResolvedAt != nil {
		t.Fatalf("unexpected pending intention: %+v", pending)
	}
	if _, err := s.UpdateImplementationIntentionStatus(ctx, "L2", id, models.IntentionStatusHonored, time.Now()); err == nil {
		t.Fatal("another learner resolved the intention")
	}

	resolvedAt := time.Now().UTC().Truncate(time.Millisecond)
	honored, err := s.UpdateImplementationIntentionStatus(ctx, "L1", id, models.IntentionStatusHonored, resolvedAt)
	if err != nil {
		t.Fatalf("honor intention: %v", err)
	}
	if honored.Status != models.IntentionStatusHonored || honored.Honored == nil || !*honored.Honored || honored.ResolvedAt == nil {
		t.Fatalf("unexpected honored intention: %+v", honored)
	}
	repeated, err := s.UpdateImplementationIntentionStatus(ctx, "L1", id, models.IntentionStatusHonored, resolvedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("repeat identical transition: %v", err)
	}
	if !repeated.ResolvedAt.Equal(*honored.ResolvedAt) {
		t.Fatal("idempotent transition changed resolved_at")
	}
	if _, err := s.UpdateImplementationIntentionStatus(ctx, "L1", id, models.IntentionStatusMissed, time.Now()); err == nil {
		t.Fatal("terminal intention was rewritten")
	}
}
