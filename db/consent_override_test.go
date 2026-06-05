// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

// TestApproveClientPersistsAndScopes covers the consent persistence path:
// an unseen (learner, client, redirect_uri) triple is not approved, becomes
// approved after ApproveClient, and approval is scoped to the exact
// redirect_uri (a different URI on the same client re-prompts).
func TestApproveClientPersistsAndScopes(t *testing.T) {
	store := setupTestDB(t)
	if err := store.CreateOAuthClient(context.Background(), "c1", "Client 1", `["https://a.example/cb"]`); err != nil {
		t.Fatalf("create client: %v", err)
	}

	// Not approved before any consent is recorded.
	ok, err := store.IsClientApproved(context.Background(), "L1", "c1", "https://a.example/cb")
	if err != nil {
		t.Fatalf("is approved (pre): %v", err)
	}
	if ok {
		t.Fatal("expected not approved before ApproveClient")
	}

	if err := store.ApproveClient(context.Background(), "L1", "c1", "https://a.example/cb"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	ok, err = store.IsClientApproved(context.Background(), "L1", "c1", "https://a.example/cb")
	if err != nil {
		t.Fatalf("is approved (post): %v", err)
	}
	if !ok {
		t.Fatal("expected approved after ApproveClient")
	}

	// Approval is scoped to redirect_uri: a different URI on the same
	// client is still unapproved.
	ok, err = store.IsClientApproved(context.Background(), "L1", "c1", "https://a.example/other")
	if err != nil {
		t.Fatalf("is approved (other uri): %v", err)
	}
	if ok {
		t.Fatal("expected different redirect_uri to remain unapproved")
	}
}

// TestApproveClientIdempotent exercises the ON CONFLICT DO NOTHING path:
// re-approving the same triple is a no-op and never errors, and leaves a
// single row on file.
func TestApproveClientIdempotent(t *testing.T) {
	store := setupTestDB(t)
	if err := store.CreateOAuthClient(context.Background(), "c1", "Client 1", `["https://a.example/cb"]`); err != nil {
		t.Fatalf("create client: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := store.ApproveClient(context.Background(), "L1", "c1", "https://a.example/cb"); err != nil {
			t.Fatalf("approve #%d: %v", i, err)
		}
	}

	var count int
	if err := store.root.QueryRow(
		`SELECT COUNT(*) FROM learner_approved_clients
		 WHERE learner_id = ? AND client_id = ? AND redirect_uri = ?`,
		"L1", "c1", "https://a.example/cb",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 approval row, got %d", count)
	}

	ok, err := store.IsClientApproved(context.Background(), "L1", "c1", "https://a.example/cb")
	if err != nil {
		t.Fatalf("is approved: %v", err)
	}
	if !ok {
		t.Fatal("expected approved after idempotent re-approve")
	}
}

// TestLearningNegotiationOverrideInsertConsume covers the happy path:
// insert a pending override, then consume it and recover the payload.
// A second consume finds nothing pending.
func TestLearningNegotiationOverrideInsertConsume(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	expires := now.Add(1 * time.Hour)

	id, err := store.InsertLearningNegotiationOverridePayload(context.Background(), "L1", "D1", "payload-1", expires, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero override id")
	}

	res, err := store.ConsumeLearningNegotiationOverridePayload(context.Background(), "L1", "D1", now)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if res.Status != models.LearningNegotiationOverrideStatusConsumed {
		t.Fatalf("status = %q, want consumed", res.Status)
	}
	if res.Payload != "payload-1" {
		t.Errorf("payload = %q, want payload-1", res.Payload)
	}
	if res.ID != id {
		t.Errorf("id = %d, want %d", res.ID, id)
	}

	// Second consume: nothing pending.
	res2, err := store.ConsumeLearningNegotiationOverridePayload(context.Background(), "L1", "D1", now)
	if err != nil {
		t.Fatalf("consume #2: %v", err)
	}
	if res2.Status != models.LearningNegotiationOverrideStatusNone {
		t.Errorf("status #2 = %q, want none", res2.Status)
	}
}

// TestLearningNegotiationOverrideSupersede verifies a new override
// supersedes an older pending one for the same learner/domain: consuming
// returns the latest payload, and the older row is no longer pending.
func TestLearningNegotiationOverrideSupersede(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	expires := now.Add(1 * time.Hour)

	if _, err := store.InsertLearningNegotiationOverridePayload(context.Background(), "L1", "D1", "old", expires, now); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if _, err := store.InsertLearningNegotiationOverridePayload(context.Background(), "L1", "D1", "new", expires, now.Add(time.Second)); err != nil {
		t.Fatalf("insert new: %v", err)
	}

	res, err := store.ConsumeLearningNegotiationOverridePayload(context.Background(), "L1", "D1", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if res.Status != models.LearningNegotiationOverrideStatusConsumed {
		t.Fatalf("status = %q, want consumed", res.Status)
	}
	if res.Payload != "new" {
		t.Errorf("payload = %q, want new (latest supersedes)", res.Payload)
	}

	// Nothing pending remains after consuming the superseding override.
	res2, err := store.ConsumeLearningNegotiationOverridePayload(context.Background(), "L1", "D1", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("consume #2: %v", err)
	}
	if res2.Status != models.LearningNegotiationOverrideStatusNone {
		t.Errorf("status #2 = %q, want none", res2.Status)
	}
}

// TestLearningNegotiationOverrideExpired verifies that an override whose
// expiry is in the past is reported expired (not consumed) and its payload
// is withheld.
func TestLearningNegotiationOverrideExpired(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	expires := now.Add(-1 * time.Minute) // already expired

	id, err := store.InsertLearningNegotiationOverridePayload(context.Background(), "L1", "D1", "stale", expires, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := store.ConsumeLearningNegotiationOverridePayload(context.Background(), "L1", "D1", now)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if res.Status != models.LearningNegotiationOverrideStatusExpired {
		t.Fatalf("status = %q, want expired", res.Status)
	}
	if res.ID != id {
		t.Errorf("id = %d, want %d", res.ID, id)
	}
	if res.Payload != "" {
		t.Errorf("expected empty payload for expired override, got %q", res.Payload)
	}

	// No longer pending afterwards.
	res2, err := store.ConsumeLearningNegotiationOverridePayload(context.Background(), "L1", "D1", now)
	if err != nil {
		t.Fatalf("consume #2: %v", err)
	}
	if res2.Status != models.LearningNegotiationOverrideStatusNone {
		t.Errorf("status #2 = %q, want none", res2.Status)
	}
}
