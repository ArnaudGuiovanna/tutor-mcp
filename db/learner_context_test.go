// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestLearnerContextOverviewIsNarrowScopedAndThreeQueries(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)

	active, err := s.CreateDomain(ctx, "L1", "active", "", models.KnowledgeSpace{
		Concepts:      []string{"shared", "active-only"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create active domain: %v", err)
	}
	archived, err := s.CreateDomain(ctx, "L1", "archived", "", models.KnowledgeSpace{
		Concepts:      []string{"archived-only"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create archived domain: %v", err)
	}
	if err := s.ArchiveDomain(ctx, archived.ID, "L1"); err != nil {
		t.Fatalf("archive domain: %v", err)
	}

	// A malformed integration secret proves this projection does not load or
	// decrypt webhook_url (GetLearnerByID intentionally would fail here).
	if _, err := s.root.ExecContext(ctx, rb(s, `UPDATE learners
		SET objective = ?, profile_json = ?, webhook_url = ? WHERE id = ?`),
		"focused objective", `{"language":"fr"}`, "not-valid-ciphertext", "L1"); err != nil {
		t.Fatalf("update learner projection fields: %v", err)
	}

	insertContextState(t, s, "L1", active.ID, "active-only", 0.6)
	insertContextState(t, s, "L1", archived.ID, "archived-only", 0.7)
	insertContextState(t, s, "L1", "", "legacy", 0.4)
	insertContextInteraction(t, s, "L1", active.ID, "active-only", true, now.Add(-time.Hour))
	insertContextInteraction(t, s, "L1", active.ID, "active-only", false, now.Add(-2*time.Hour))
	insertContextInteraction(t, s, "L1", archived.ID, "archived-only", true, now.Add(-3*time.Hour))
	insertContextInteraction(t, s, "L1", active.ID, "yesterday", true, now.Add(-25*time.Hour))

	counter := &learnerContextCountingExecutor{sqlExecutor: s.db}
	measured := &Store{
		db:            counter,
		root:          s.root,
		dialect:       s.dialect,
		secretKeyring: s.secretKeyring,
	}
	overview, err := measured.GetLearnerContextOverview(ctx, "L1", now)
	if err != nil {
		t.Fatalf("get overview: %v", err)
	}
	if counter.queries != 3 {
		t.Fatalf("overview SQL queries = %d, want exactly 3", counter.queries)
	}
	if overview.Learner.ID != "L1" || overview.Learner.Objective != "focused objective" || overview.Learner.ProfileJSON != `{"language":"fr"}` {
		t.Fatalf("learner projection = %+v", overview.Learner)
	}
	if len(overview.Domains) != 2 {
		t.Fatalf("domains = %+v, want active and archived only", overview.Domains)
	}
	byDomain := make(map[string]storeport.LearnerContextDomain)
	for _, domain := range overview.Domains {
		byDomain[domain.ID] = domain
	}
	if byDomain[active.ID].Archived || !byDomain[archived.ID].Archived {
		t.Fatalf("domain archive flags = %+v", byDomain)
	}
	if len(overview.ConceptStates) != 3 {
		t.Fatalf("concept-state projection count = %d, want 3", len(overview.ConceptStates))
	}
	counts := make(map[string]int)
	for _, count := range overview.TodayInteractions {
		counts[count.DomainID+"/"+count.Concept] = count.Count
	}
	if counts[active.ID+"/active-only"] != 2 || counts[archived.ID+"/archived-only"] != 1 {
		t.Fatalf("today grouped counts = %v", counts)
	}
	if _, ok := counts[active.ID+"/yesterday"]; ok {
		t.Fatalf("previous UTC day leaked into today's counts: %v", counts)
	}
}

func TestLearnerContextOverviewNotFoundUsesStoreSentinel(t *testing.T) {
	s := setupTestDB(t)
	_, err := s.GetLearnerContextOverview(context.Background(), "missing", time.Now())
	if !errors.Is(err, storeport.ErrNotFound) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error = %v, want store and SQL not-found sentinels", err)
	}
}

func TestLearnerContextNarrativeSignalsEquivalentAndOneQuery(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	domain, err := s.CreateDomain(ctx, "L1", "math", "", models.KnowledgeSpace{
		Concepts:      []string{"a", "b"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	otherDomain, err := s.CreateDomain(ctx, "L1", "other", "", models.KnowledgeSpace{
		Concepts:      []string{"a"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create other domain: %v", err)
	}

	// Before the 30-day trajectory window: a=2/3, b=0/1. These counts must
	// remain domain scoped even though the other domain reuses concept "a".
	for i, success := range []bool{true, true, false} {
		insertContextInteraction(t, s, "L1", domain.ID, "a", success, now.Add(time.Duration(-40-i)*24*time.Hour))
	}
	insertContextInteraction(t, s, "L1", domain.ID, "b", false, now.Add(-45*24*time.Hour))
	insertContextInteraction(t, s, "L1", otherDomain.ID, "a", false, now.Add(-50*24*time.Hour))
	// A two-day streak and one fresh milestone candidate.
	insertContextInteraction(t, s, "L1", domain.ID, "a", true, now.Add(-time.Hour))
	insertContextInteraction(t, s, "L1", domain.ID, "b", false, now.Add(-25*time.Hour))
	insertContextInteraction(t, s, "L1", domain.ID, "a", true, now.Add(-72*time.Hour)) // gap after yesterday

	for i, score := range []float64{0.2, 0.5, 0.8} {
		createdAt := now.Add(time.Duration(i-3) * time.Hour)
		if _, err := s.root.ExecContext(ctx, rb(s, `INSERT INTO affect_states
			(learner_id, session_id, autonomy_score, created_at)
			VALUES (?, ?, ?, ?)`), "L1", fmt.Sprintf("affect-%d", i), score, createdAt); err != nil {
			t.Fatalf("insert affect %d: %v", i, err)
		}
	}

	counter := &learnerContextCountingExecutor{sqlExecutor: s.db}
	measured := &Store{
		db:            counter,
		root:          s.root,
		dialect:       s.dialect,
		secretKeyring: s.secretKeyring,
	}
	signals, err := measured.GetLearnerContextNarrativeSignals(ctx, "L1", domain.ID, domain.Graph.Concepts, now)
	if err != nil {
		t.Fatalf("get narrative signals: %v", err)
	}
	if counter.queries != 1 {
		t.Fatalf("narrative SQL queries = %d, want exactly 1", counter.queries)
	}
	if signals.SessionStreak != 2 {
		t.Fatalf("session streak = %d, want 2", signals.SessionStreak)
	}
	history := make(map[string]storeport.LearnerContextConceptHistory)
	for _, item := range signals.ConceptHistory {
		history[item.Concept] = item
	}
	if got := history["a"]; got.TotalBefore != 3 || got.SuccessfulBefore != 2 || !got.RecentSuccess {
		t.Fatalf("history[a] = %+v, want 2/3 with recent success", got)
	}
	if got := history["b"]; got.TotalBefore != 1 || got.SuccessfulBefore != 0 || got.RecentSuccess {
		t.Fatalf("history[b] = %+v, want 0/1 without recent success", got)
	}
	wantScores := []float64{0.8, 0.5, 0.2}
	if len(signals.RecentAutonomyScores) != len(wantScores) {
		t.Fatalf("autonomy scores = %v", signals.RecentAutonomyScores)
	}
	for i := range wantScores {
		if signals.RecentAutonomyScores[i] != wantScores[i] {
			t.Fatalf("autonomy scores = %v, want newest-first %v", signals.RecentAutonomyScores, wantScores)
		}
	}
}

func TestLearnerContextReadModelHighCardinalityIsConstantQueryCount(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	concepts := make([]string, 200)
	for i := range concepts {
		concepts[i] = fmt.Sprintf("concept-%03d", i)
	}
	domain, err := s.CreateDomain(ctx, "L1", "large", "", models.KnowledgeSpace{
		Concepts:      concepts,
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create large domain: %v", err)
	}
	for i, concept := range concepts {
		insertContextState(t, s, "L1", domain.ID, concept, 0.3+float64(i%7)/10)
		insertContextInteraction(t, s, "L1", domain.ID, concept, i%2 == 0, now.Add(-time.Duration(i%12)*time.Hour))
		insertContextInteraction(t, s, "L1", domain.ID, concept, i%3 == 0, now.Add(-40*24*time.Hour))
	}

	counter := &learnerContextCountingExecutor{sqlExecutor: s.db}
	measured := &Store{db: counter, root: s.root, dialect: s.dialect, secretKeyring: s.secretKeyring}
	overview, err := measured.GetLearnerContextOverview(ctx, "L1", now)
	if err != nil {
		t.Fatalf("get high-cardinality overview: %v", err)
	}
	signals, err := measured.GetLearnerContextNarrativeSignals(ctx, "L1", domain.ID, concepts, now)
	if err != nil {
		t.Fatalf("get high-cardinality narrative: %v", err)
	}
	_, _ = measured.GetActiveLearningSession(ctx, "L1")
	if counter.queries != 5 {
		t.Fatalf("read-model SQL queries = %d, want 5 independent of 200 concepts", counter.queries)
	}
	if len(overview.ConceptStates) != 200 || len(overview.TodayInteractions) != 200 {
		t.Fatalf("overview cardinality states=%d interactions=%d", len(overview.ConceptStates), len(overview.TodayInteractions))
	}
	if len(signals.ConceptHistory) != 200 {
		t.Fatalf("narrative concept aggregates=%d, want 200", len(signals.ConceptHistory))
	}

	// Mirror the pre-read-model call graph to keep the before/after query count
	// executable rather than relying on a comment or benchmark label.
	counter.queries = 0
	if _, err := measured.GetLearnerByID(ctx, "L1"); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.GetDomainsByLearner(ctx, "L1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.GetDomainByLearner(ctx, "L1"); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.GetConceptStatesByLearner(ctx, "L1"); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.GetInteractionsSince(ctx, "L1", time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.GetDomainsByLearner(ctx, "L1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.ConceptMasteryDeltaInDomain(ctx, "L1", domain.ID, concepts, now.Add(-30*24*time.Hour), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.CountLearnerSessionStreak(ctx, "L1"); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.MilestonesInWindowInDomain(ctx, "L1", domain.ID, concepts, now.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.GetRecentAffectStates(ctx, "L1", 5); err != nil {
		t.Fatal(err)
	}
	_, _ = measured.GetActiveLearningSession(ctx, "L1")
	if counter.queries != 13 {
		t.Fatalf("legacy SQL queries = %d, want 13 at high cardinality", counter.queries)
	}
}

type learnerContextCountingExecutor struct {
	sqlExecutor
	queries int
}

func (e *learnerContextCountingExecutor) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	e.queries++
	return e.sqlExecutor.QueryContext(ctx, query, args...)
}

func (e *learnerContextCountingExecutor) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	e.queries++
	return e.sqlExecutor.QueryRowContext(ctx, query, args...)
}

func insertContextState(t *testing.T, s *Store, learnerID, domainID, concept string, mastery float64) {
	t.Helper()
	if _, err := s.root.ExecContext(context.Background(), rb(s, `INSERT INTO concept_states
		(learner_id, domain_id, concept, card_state, p_mastery)
		VALUES (?, ?, ?, 'review', ?)`), learnerID, domainID, concept, mastery); err != nil {
		t.Fatalf("insert context state %q: %v", concept, err)
	}
}

func insertContextInteraction(t *testing.T, s *Store, learnerID, domainID, concept string, success bool, createdAt time.Time) {
	t.Helper()
	successInt := 0
	if success {
		successInt = 1
	}
	if _, err := s.root.ExecContext(context.Background(), rb(s, `INSERT INTO interactions
		(learner_id, domain_id, concept, activity_type, success, created_at)
		VALUES (?, ?, ?, 'PRACTICE', ?, ?)`), learnerID, domainID, concept, successInt, createdAt.UTC()); err != nil {
		t.Fatalf("insert context interaction %q: %v", concept, err)
	}
}
