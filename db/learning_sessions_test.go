// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestLearningSession_OpenConcurrentAndCloseIdempotent(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	const contenders = 8
	start := make(chan struct{})
	results := make(chan *models.LearningSession, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			session, err := s.OpenLearningSession(ctx, "L1", "", "", now.Add(time.Duration(i)*time.Millisecond))
			results <- session
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent open: %v", err)
		}
	}
	var canonical string
	for session := range results {
		if session == nil {
			t.Fatal("concurrent open returned nil session")
		}
		if canonical == "" {
			canonical = session.ID
		}
		if session.ID != canonical {
			t.Fatalf("concurrent opens returned %q and %q", canonical, session.ID)
		}
	}

	closed, err := s.CloseLearningSession(ctx, "L1", canonical, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	closedAgain, err := s.CloseLearningSession(ctx, "L1", canonical, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	if closed.Status != models.LearningSessionStatusClosed || closed.ClosedAt == nil {
		t.Fatalf("unexpected closed session: %+v", closed)
	}
	if !closedAgain.ClosedAt.Equal(*closed.ClosedAt) {
		t.Fatalf("idempotent close changed closed_at: first=%v second=%v", closed.ClosedAt, closedAgain.ClosedAt)
	}

	next, err := s.OpenLearningSession(ctx, "L1", "", "", now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	if next.ID == canonical {
		t.Fatal("new episode reused the closed session ID")
	}
}

func TestLearningSession_OwnershipAndRequestedID(t *testing.T) {
	s := setupTestDB(t)
	seedLearner(t, s, "L2")
	ctx := context.Background()
	now := time.Now().UTC()
	domain, err := s.CreateDomain(ctx, "L1", "private", "goal", models.KnowledgeSpace{Concepts: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenLearningSession(ctx, "L2", domain.ID, "sess_foreign_domain", now); err == nil {
		t.Fatal("learner opened a session against another learner's domain")
	}

	session, err := s.OpenLearningSession(ctx, "L1", domain.ID, "sess_client_key", now)
	if err != nil {
		t.Fatalf("open requested session: %v", err)
	}
	if session.ID != "sess_client_key" {
		t.Fatalf("session id = %q, want requested id", session.ID)
	}
	repeated, err := s.OpenLearningSession(ctx, "L1", "", "sess_client_key", now.Add(time.Minute))
	if err != nil || repeated.ID != session.ID {
		t.Fatalf("idempotent requested open: session=%+v err=%v", repeated, err)
	}
	if _, err := s.GetLearningSession(ctx, "L2", session.ID); err == nil {
		t.Fatal("other learner read session")
	}
	if _, err := s.TouchLearningSession(ctx, "L2", session.ID, now); err == nil {
		t.Fatal("other learner touched session")
	}
	if _, err := s.CloseLearningSession(ctx, "L2", session.ID, now); err == nil {
		t.Fatal("other learner closed session")
	}
}

func TestSessionInteractions_ExplicitBoundaryDoesNotUseTwoHourWindow(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, err := s.OpenLearningSession(ctx, "L1", "", "sess_first", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInteraction(ctx, &models.Interaction{
		LearnerID: "L1", SessionID: first.ID, Concept: "c", ActivityType: "PRACTICE", Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseLearningSession(ctx, "L1", first.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	second, err := s.OpenLearningSession(ctx, "L1", "", "sess_second", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInteraction(ctx, &models.Interaction{
		LearnerID: "L1", SessionID: second.ID, Concept: "d", ActivityType: "PRACTICE", Success: true,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSessionInteractions(ctx, "L1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != second.ID || got[0].Concept != "d" {
		t.Fatalf("active session leaked recent closed interaction: %+v", got)
	}
}

func TestCountSessionsOnConcept_UsesExplicitIDsWithinSameDay(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, sessionID := range []string{"sess_count_1", "sess_count_2"} {
		session, err := s.OpenLearningSession(ctx, "L1", "", sessionID, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CreateInteraction(ctx, &models.Interaction{
			LearnerID: "L1", SessionID: session.ID, Concept: "c", ActivityType: "PRACTICE", Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseLearningSession(ctx, "L1", session.ID, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	count, err := s.CountSessionsOnConcept(ctx, "L1", "c")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("same-day explicit sessions counted as %d, want 2", count)
	}
}
