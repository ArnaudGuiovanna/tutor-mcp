// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"

	_ "modernc.org/sqlite"
)

var motivationCounter int32

func newMotivationStore(t *testing.T) (*sql.DB, *db.Store, string) {
	t.Helper()
	id := atomic.AddInt32(&motivationCounter, 1)
	dsn := fmt.Sprintf("file:eng_motiv_%s_%d?mode=memory&cache=shared", t.Name(), id)
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { rawDB.Close() })
	if err := db.Migrate(rawDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	learnerID := "L1"
	if _, err := rawDB.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, ?, ?, ?)`,
		learnerID, "t@t.com", "h", "obj", time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert learner: %v", err)
	}
	return rawDB, db.NewStore(rawDB), learnerID
}

// TestNewMotivationEngine ensures the constructor wires the store and is non-nil.
func TestNewMotivationEngine(t *testing.T) {
	_, store, _ := newMotivationStore(t)
	m := NewMotivationEngine(store)
	if m == nil {
		t.Fatal("NewMotivationEngine returned nil")
	}
	if m.store != store {
		t.Error("expected store to be stored")
	}
}

// TestBuild_NoDomain — when called with a nil domain and an unknown concept,
// Build should return a brief with empty kind (no triggers fired).
func TestBuild_NoDomain(t *testing.T) {
	_, store, learnerID := newMotivationStore(t)
	m := NewMotivationEngine(store)
	brief, err := m.Build(context.Background(), learnerID, nil, "", models.ActivityRest, false, 5)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if brief == nil {
		t.Fatal("expected non-nil brief, got nil")
	}
	if brief.Kind != "" {
		t.Errorf("expected silent brief (kind=''), got %q", brief.Kind)
	}
}

// TestBuild_CompetenceValueRotatesAxis — first session on a concept with a
// goal-having domain fires competence_value, and Domain.LastValueAxis is
// rotated as a side effect.
func TestBuild_CompetenceValueRotatesAxis(t *testing.T) {
	rawDB, store, learnerID := newMotivationStore(t)

	// Create domain via store helper.
	domain, err := store.CreateDomainWithValueFramings(context.Background(),
		learnerID, "Go", "Become SRE",
		models.KnowledgeSpace{Concepts: []string{"Goroutines"}},
		`{"financial":"salaire 80k+","employment":"jobs DevOps"}`,
	)
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}

	// Concept state with low mastery so milestone won't fire.
	if err := store.UpsertConceptState(context.Background(), &models.ConceptState{
		LearnerID: learnerID, Concept: "Goroutines", PMastery: 0.2, CardState: "learning",
	}); err != nil {
		t.Fatalf("upsert concept state: %v", err)
	}

	m := NewMotivationEngine(store)
	brief, err := m.Build(context.Background(), learnerID, domain, "Goroutines", models.ActivityNewConcept, false, 1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if brief.Kind != models.MotivationKindCompetenceValue {
		t.Errorf("expected competence_value, got %q", brief.Kind)
	}
	if brief.ValueFraming == nil || brief.ValueFraming.Axis == "" {
		t.Errorf("expected ValueFraming with non-empty axis, got %+v", brief.ValueFraming)
	}

	// Domain.LastValueAxis must have been persisted to one of the canonical axes.
	var stored sql.NullString
	if err := rawDB.QueryRow(`SELECT last_value_axis FROM domains WHERE id = ?`, domain.ID).Scan(&stored); err != nil {
		t.Fatalf("query last_value_axis: %v", err)
	}
	if !stored.Valid || stored.String == "" {
		t.Errorf("Domain.LastValueAxis was not persisted")
	}
}

// TestBuild_WithDomainAndConcept exercises the full happy path: store reads,
// SelectBrief, ComposeBrief, and a non-empty kind being returned.
func TestBuild_WithDomainAndConcept(t *testing.T) {
	rawDB, store, learnerID := newMotivationStore(t)

	domain, err := store.CreateDomain(context.Background(), learnerID, "Go", "Become SRE",
		models.KnowledgeSpace{Concepts: []string{"Channels"}})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if err := store.UpsertConceptState(context.Background(), &models.ConceptState{
		LearnerID: learnerID, Concept: "Channels", PMastery: 0.3, CardState: "learning",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Insert interactions on different days via raw SQL so CountSessionsOnConcept
	// (distinct-date heuristic) sees > 1 session.
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		if _, err := rawDB.Exec(
			`INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, notes, self_initiated, created_at)
			 VALUES (?, ?, 'RECALL_EXERCISE', 1, 60, 0.5, '', 1, ?)`,
			learnerID, "Channels", now.AddDate(0, 0, -i*2),
		); err != nil {
			t.Fatalf("insert interaction: %v", err)
		}
	}

	m := NewMotivationEngine(store)
	brief, err := m.Build(context.Background(), learnerID, domain, "Channels", models.ActivityRecall, false, 1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if brief == nil {
		t.Fatal("expected non-nil brief")
	}
	// On a 1st-of-session exercise with a goal, why_this_exercise is the
	// expected fallback (SessionsOnConcept > 1 disables the "1st session"
	// competence_value branch, no failure, no negative affect, no plateau).
	if brief.Kind != models.MotivationKindWhyThisExercise {
		t.Errorf("expected why_this_exercise, got %q", brief.Kind)
	}
	if brief.GoalLink != "Become SRE" {
		t.Errorf("expected goal link 'Become SRE', got %q", brief.GoalLink)
	}
}

// ─── ComposeBrief: exercise the remaining branches ──────────────────────────

func TestComposeBrief_Milestone(t *testing.T) {
	now := time.Now().UTC()
	in := BriefInput{
		Domain:       newDomainWithGoal("g"),
		Concept:      "X",
		ConceptState: &models.ConceptState{PMastery: 0.86},
		Now:          now,
	}
	b := ComposeBrief(in, models.MotivationKindMilestone, "")
	if b.ProgressDelta == nil {
		t.Fatal("expected ProgressDelta populated")
	}
	if b.ProgressDelta.Concept != "X" {
		t.Errorf("delta concept = %q, want X", b.ProgressDelta.Concept)
	}
	if b.ProgressDelta.Threshold == 0 {
		t.Errorf("expected non-zero threshold")
	}
	if b.Instruction == "" {
		t.Error("expected non-empty instruction for milestone")
	}
}

func TestComposeBrief_GrowthMindset(t *testing.T) {
	now := time.Now().UTC()
	in := BriefInput{
		Concept: "X",
		LastFailure: &models.Interaction{
			Concept: "X", Success: false,
			HintsRequested: 1, ErrorType: "LOGIC_ERROR",
			MisconceptionType: "confusion", CreatedAt: now.Add(-2 * time.Hour),
		},
		Now: now,
	}
	b := ComposeBrief(in, models.MotivationKindGrowthMindset, "")
	if b.FailureContext == nil {
		t.Fatal("expected FailureContext populated")
	}
	if b.FailureContext.Concept != "X" || b.FailureContext.ErrorType != "LOGIC_ERROR" {
		t.Errorf("failure context = %+v, want concept=X errortype=LOGIC_ERROR", b.FailureContext)
	}
	if b.FailureContext.HoursAgo < 1 {
		t.Errorf("expected ~2h ago, got %d", b.FailureContext.HoursAgo)
	}
	if b.Instruction == "" {
		t.Error("expected non-empty instruction")
	}
}

func TestComposeBrief_GrowthMindset_NowZero(t *testing.T) {
	// When in.Now is zero, ComposeBrief falls back to time.Since(failure.CreatedAt)
	// to compute HoursAgo. We don't assert the exact number — just that it's
	// non-negative and the branch is exercised.
	in := BriefInput{
		Concept: "X",
		LastFailure: &models.Interaction{
			Concept: "X", Success: false, CreatedAt: time.Now().Add(-3 * time.Hour),
		},
	}
	b := ComposeBrief(in, models.MotivationKindGrowthMindset, "")
	if b.FailureContext == nil {
		t.Fatal("FailureContext nil")
	}
	if b.FailureContext.HoursAgo < 0 {
		t.Errorf("HoursAgo should be non-negative, got %d", b.FailureContext.HoursAgo)
	}
}

func TestComposeBrief_AffectReframe_AllDimensions(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name    string
		affect  *models.AffectState
		wantDim string
	}{
		{"satisfaction", &models.AffectState{SessionID: "s", Satisfaction: 1, CreatedAt: now.Add(-1 * time.Hour)}, "satisfaction"},
		{"difficulty", &models.AffectState{SessionID: "s", PerceivedDifficulty: 4, CreatedAt: now.Add(-1 * time.Hour)}, "difficulty"},
		{"energy", &models.AffectState{SessionID: "s", Energy: 1, CreatedAt: now.Add(-1 * time.Hour)}, "energy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := BriefInput{LatestAffect: tc.affect, Now: now}
			b := ComposeBrief(in, models.MotivationKindAffectReframe, "")
			if b.AffectContext == nil {
				t.Fatal("expected AffectContext populated")
			}
			if b.AffectContext.Dimension != tc.wantDim {
				t.Errorf("dimension = %q, want %q", b.AffectContext.Dimension, tc.wantDim)
			}
			if b.AffectContext.SessionID != "s" {
				t.Errorf("session id = %q, want %q", b.AffectContext.SessionID, "s")
			}
		})
	}
}

func TestComposeBrief_AffectReframe_NowZero(t *testing.T) {
	in := BriefInput{
		LatestAffect: &models.AffectState{SessionID: "s", Satisfaction: 1, CreatedAt: time.Now().Add(-1 * time.Hour)},
	}
	b := ComposeBrief(in, models.MotivationKindAffectReframe, "")
	if b.AffectContext == nil {
		t.Fatal("AffectContext nil")
	}
	if b.AffectContext.HoursAgo < 0 {
		t.Errorf("HoursAgo should be non-negative, got %d", b.AffectContext.HoursAgo)
	}
}

func TestComposeBrief_PlateauRecontext(t *testing.T) {
	b := ComposeBrief(BriefInput{}, models.MotivationKindPlateauRecontext, "")
	if b.Kind != models.MotivationKindPlateauRecontext {
		t.Errorf("kind = %q, want plateau_recontext", b.Kind)
	}
	if b.Instruction == "" {
		t.Error("expected instruction for plateau")
	}
}

func TestComposeBrief_WhyThisExercise(t *testing.T) {
	in := BriefInput{Domain: newDomainWithGoal("become SRE")}
	b := ComposeBrief(in, models.MotivationKindWhyThisExercise, "")
	if b.GoalLink != "become SRE" {
		t.Errorf("GoalLink = %q, want 'become SRE'", b.GoalLink)
	}
	if b.Instruction == "" {
		t.Error("expected instruction")
	}
}

func TestComposeBrief_CompetenceValue_NoStatementAuthored(t *testing.T) {
	domain := newDomainWithGoal("g")
	domain.ValueFramingsJSON = "" // nothing authored
	in := BriefInput{Domain: domain, Concept: "X", ConceptState: &models.ConceptState{PMastery: 0.3}}
	b := ComposeBrief(in, models.MotivationKindCompetenceValue, "intellectual")
	if b.ValueFraming == nil {
		t.Fatal("ValueFraming nil")
	}
	if b.ValueFraming.Axis != "intellectual" {
		t.Errorf("axis = %q", b.ValueFraming.Axis)
	}
	if b.ValueFraming.Statement != "" {
		t.Errorf("expected empty Statement, got %q", b.ValueFraming.Statement)
	}
	if b.GoalLink != "g" {
		t.Errorf("GoalLink = %q, want g", b.GoalLink)
	}
}

func TestComposeBrief_CompetenceValue_BadJSON(t *testing.T) {
	domain := newDomainWithGoal("g")
	domain.ValueFramingsJSON = "this is not json"
	in := BriefInput{Domain: domain, Concept: "X"}
	b := ComposeBrief(in, models.MotivationKindCompetenceValue, "financial")
	if b.ValueFraming == nil {
		t.Fatal("ValueFraming nil")
	}
	// Bad JSON should not crash; statement just stays empty.
	if b.ValueFraming.Statement != "" {
		t.Errorf("expected empty statement on bad JSON, got %q", b.ValueFraming.Statement)
	}
}

// ─── affectIsNegative: explicit branches ────────────────────────────────────

func TestAffectIsNegative_AllBranches(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		a    *models.AffectState
		want bool
	}{
		{"nil", nil, false},
		{"satisfaction high", &models.AffectState{Satisfaction: 4, CreatedAt: now}, false},
		{"satisfaction low", &models.AffectState{Satisfaction: 1, CreatedAt: now}, true},
		{"difficulty 4", &models.AffectState{PerceivedDifficulty: 4, CreatedAt: now}, true},
		{"difficulty 3", &models.AffectState{PerceivedDifficulty: 3, CreatedAt: now}, false},
		{"energy low", &models.AffectState{Energy: 1, CreatedAt: now}, true},
		{"energy normal", &models.AffectState{Energy: 3, CreatedAt: now}, false},
		{"old satisfaction", &models.AffectState{Satisfaction: 1, CreatedAt: now.Add(-48 * time.Hour)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := affectIsNegative(tc.a, now); got != tc.want {
				t.Errorf("affectIsNegative = %v, want %v", got, tc.want)
			}
		})
	}
}

// affectIsNegative with now zero must internally fall back to time.Now().
func TestAffectIsNegative_NowZero(t *testing.T) {
	fresh := &models.AffectState{Satisfaction: 1, CreatedAt: time.Now().Add(-1 * time.Hour)}
	if !affectIsNegative(fresh, time.Time{}) {
		t.Errorf("expected fresh affect to be negative even with zero 'now'")
	}
}

// ─── Exported wrappers for test/debug use in other packages ────────────────

func TestGroupIntoSessionsExported(t *testing.T) {
	now := time.Now().UTC()
	xs := []*models.Interaction{
		{CreatedAt: now.Add(-5 * time.Hour)},
		{CreatedAt: now.Add(-30 * time.Minute)},
	}
	got := GroupIntoSessionsExported(xs, 1*time.Hour)
	if len(got) != 2 {
		t.Errorf("expected 2 sessions across the gap, got %d", len(got))
	}
}

func TestComputeAutonomyTrendExported(t *testing.T) {
	// scores are passed newest-first; "improving" means the recent slice is
	// higher than the previous slice.
	cases := []struct {
		scores []float64
		want   string
	}{
		{[]float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95}, "declining"},
		{[]float64{0.95, 0.9, 0.8, 0.7, 0.6, 0.5, 0.4, 0.3, 0.2, 0.1}, "improving"},
		{[]float64{0.5, 0.5}, "stable"},
	}
	for _, tc := range cases {
		got := ComputeAutonomyTrendExported(tc.scores)
		if got != tc.want {
			t.Errorf("ComputeAutonomyTrendExported(%v) = %q, want %q", tc.scores, got, tc.want)
		}
	}
}
