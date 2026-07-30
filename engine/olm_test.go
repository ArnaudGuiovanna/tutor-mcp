// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

var olmTestDSNCounter int64

// newOLMTestStore opens a fresh in-memory SQLite database with migrations applied
// and returns the wrapped Store + raw *sql.DB. Reused across olm_test.go.
func newOLMTestStore(t *testing.T) (*db.Store, *sql.DB) {
	t.Helper()
	n := atomic.AddInt64(&olmTestDSNCounter, 1)
	dsn := fmt.Sprintf("file:olm_%s_%d?mode=memory&cache=shared", t.Name(), n)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return db.NewStore(raw), raw
}

func TestBuildOLMSnapshot_NoDomain_ReturnsError(t *testing.T) {
	store, _ := newOLMTestStore(t)

	snap, err := BuildOLMSnapshot(context.Background(), store, "nonexistent_learner", "")
	if err == nil {
		t.Fatalf("expected error for learner with no active domain, got snap=%+v", snap)
	}
}

// seedDomain inserts a non-archived (or archived) domain with the given concepts.
func seedDomain(t *testing.T, raw *sql.DB, learnerID, name string, concepts []string, prereqs map[string][]string, archived bool) string {
	t.Helper()
	graphJSON, err := json.Marshal(map[string]any{
		"concepts":      concepts,
		"prerequisites": prereqs,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := learnerID + "_" + name
	archInt := 0
	if archived {
		archInt = 1
	}
	_, err = raw.Exec(
		`INSERT INTO domains (id, learner_id, name, graph_json, personal_goal, archived, value_framings_json, last_value_axis, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, '', '', ?)`,
		id, learnerID, name, string(graphJSON), "test goal", archInt, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedConceptState upserts a concept_state row for a concept.
func seedConceptState(t *testing.T, store *db.Store, learnerID, concept string, mastery float64, cardState string) {
	t.Helper()
	domains, err := store.GetDomainsByLearner(context.Background(), learnerID, true)
	if err != nil {
		t.Fatal(err)
	}
	domainID := ""
	for _, domain := range domains {
		for _, candidate := range domain.Graph.Concepts {
			if candidate == concept {
				domainID = domain.ID
				break
			}
		}
		if domainID != "" {
			break
		}
	}
	if domainID == "" {
		t.Fatalf("no seeded domain contains concept %q", concept)
	}
	cs := models.NewConceptStateInDomain(learnerID, domainID, concept)
	cs.PMastery = mastery
	cs.CardState = cardState
	if cardState != "new" {
		cs.Stability = 5.0
		cs.Reps = 1
		now := time.Now().UTC()
		cs.LastReview = &now
		cs.ScheduledDays = 7
	}
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
}

func seedLearner(t *testing.T, raw *sql.DB, learnerID string) {
	t.Helper()
	_, err := raw.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, 'h', 'obj', ?)`,
		learnerID, learnerID+"@t.com", time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuildOLMSnapshot_MasteryBuckets(t *testing.T) {
	store, raw := newOLMTestStore(t)
	seedLearner(t, raw, "L1")
	seedDomain(t, raw, "L1", "math",
		[]string{"a", "b", "c", "d", "e"},
		map[string][]string{"b": {"a"}, "c": {"b"}},
		false,
	)
	seedConceptState(t, store, "L1", "a", 0.85, "review") // Solid
	seedConceptState(t, store, "L1", "b", 0.50, "review") // InProgress
	seedConceptState(t, store, "L1", "c", 0.10, "review") // Fragile
	seedConceptState(t, store, "L1", "d", 0.0, "new")     // NotStarted
	// "e" has NO concept_state row → also NotStarted

	snap, err := BuildOLMSnapshot(context.Background(), store, "L1", "")
	if err != nil {
		t.Fatalf("BuildOLMSnapshot: %v", err)
	}
	if snap.Solid != 1 {
		t.Errorf("Solid=%d, want 1", snap.Solid)
	}
	if snap.InProgress != 1 {
		t.Errorf("InProgress=%d, want 1", snap.InProgress)
	}
	if snap.Fragile != 1 {
		t.Errorf("Fragile=%d, want 1", snap.Fragile)
	}
	if snap.NotStarted != 2 {
		t.Errorf("NotStarted=%d (incl. concept with no state), want 2", snap.NotStarted)
	}
}

func TestBuildOLMSnapshot_FocusForgettingCritical(t *testing.T) {
	store, raw := newOLMTestStore(t)
	seedLearner(t, raw, "L1")
	domainID := seedDomain(t, raw, "L1", "math",
		[]string{"a", "b"},
		map[string][]string{"b": {"a"}},
		false,
	)
	// 'a' is in deep forgetting (low retention).
	cs := models.NewConceptStateInDomain("L1", domainID, "a")
	cs.PMastery = 0.40
	cs.Stability = 1.0
	cs.ElapsedDays = 30
	cs.Reps = 5
	cs.CardState = "review"
	now := time.Now().UTC().Add(-30 * 24 * time.Hour)
	cs.LastReview = &now
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	snap, err := BuildOLMSnapshot(context.Background(), store, "L1", "")
	if err != nil {
		t.Fatalf("BuildOLMSnapshot: %v", err)
	}
	if snap.FocusConcept != "a" {
		t.Errorf("FocusConcept=%q, want a", snap.FocusConcept)
	}
	if snap.FocusUrgency != models.UrgencyCritical && snap.FocusUrgency != models.UrgencyWarning {
		t.Errorf("FocusUrgency=%q, want critical or warning (FORGETTING)", snap.FocusUrgency)
	}
	if snap.FocusReason == "" {
		t.Error("FocusReason should be non-empty (retention …%)")
	}
}

func TestBuildOLMSnapshot_FocusFallsBackToFrontier(t *testing.T) {
	store, raw := newOLMTestStore(t)
	seedLearner(t, raw, "L1")
	seedDomain(t, raw, "L1", "math",
		[]string{"a", "b", "c"},
		map[string][]string{"b": {"a"}, "c": {"b"}},
		false,
	)
	// 'a' mastered → 'b' is on the frontier. 'b' has no state yet.
	seedConceptState(t, store, "L1", "a", 0.90, "review")

	snap, err := BuildOLMSnapshot(context.Background(), store, "L1", "")
	if err != nil {
		t.Fatalf("BuildOLMSnapshot: %v", err)
	}
	if snap.FocusConcept != "b" {
		t.Errorf("FocusConcept=%q, want b (frontier)", snap.FocusConcept)
	}
	if snap.FocusUrgency != models.UrgencyInfo {
		t.Errorf("FocusUrgency=%q, want info (frontier fallback)", snap.FocusUrgency)
	}
	if snap.FocusReason != "next frontier" {
		t.Errorf("FocusReason=%q, want 'next frontier'", snap.FocusReason)
	}
}

func TestBuildOLMSnapshot_MetacognitiveSignals(t *testing.T) {
	store, raw := newOLMTestStore(t)
	seedLearner(t, raw, "L1")
	seedDomain(t, raw, "L1", "math", []string{"a"}, nil, false)
	seedConceptState(t, store, "L1", "a", 0.50, "review")

	// Affect history. UpsertAffectState orders by created_at desc; we want
	// the most recent to have AutonomyScore=0.3 and the oldest (3rd back)
	// 0.7. Insert oldest first so created_at increases monotonically.
	for _, score := range []float64{0.7, 0.5, 0.3} {
		af := &models.AffectState{
			LearnerID:     "L1",
			SessionID:     fmt.Sprintf("s%.0f", score*10),
			Energy:        3,
			Satisfaction:  2,
			AutonomyScore: score,
		}
		if err := store.UpsertAffectState(context.Background(), af); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // ensure distinct created_at
	}

	snap, err := BuildOLMSnapshot(context.Background(), store, "L1", "")
	if err != nil {
		t.Fatalf("BuildOLMSnapshot: %v", err)
	}
	// Most recent autonomy (0.3) < oldest of the 3 (0.7) by 0.40 → declining.
	if snap.AutonomyTrend != "declining" {
		t.Errorf("AutonomyTrend=%q, want declining", snap.AutonomyTrend)
	}
	if snap.AffectTrend != "stable" && snap.AffectTrend != "" {
		// All 3 satisfaction = 2 → no diff → stable. Empty also acceptable
		// if implementation returns "" when diff is below threshold.
		t.Errorf("AffectTrend=%q, want stable or empty", snap.AffectTrend)
	}
}

func TestBuildOLMSnapshot_KSTProgressAndActionable(t *testing.T) {
	store, raw := newOLMTestStore(t)
	seedLearner(t, raw, "L1")
	seedDomain(t, raw, "L1", "math",
		[]string{"a", "b", "c", "d"},
		map[string][]string{"b": {"a"}, "c": {"b"}, "d": {"c"}},
		false,
	)
	// 2 mastered, 2 not started → KSTProgress = 2/4 = 0.5
	seedConceptState(t, store, "L1", "a", 0.90, "review")
	seedConceptState(t, store, "L1", "b", 0.85, "review")

	snap, err := BuildOLMSnapshot(context.Background(), store, "L1", "")
	if err != nil {
		t.Fatalf("BuildOLMSnapshot: %v", err)
	}
	if snap.KSTProgress < 0.49 || snap.KSTProgress > 0.51 {
		t.Errorf("KSTProgress=%f, want ~0.5", snap.KSTProgress)
	}
	if !snap.HasActionable {
		t.Errorf("HasActionable=false, want true (frontier focus exists)")
	}
}

func TestBuildOLMSnapshot_NotActionable_AllSolid(t *testing.T) {
	store, raw := newOLMTestStore(t)
	seedLearner(t, raw, "L1")
	seedDomain(t, raw, "L1", "math",
		[]string{"a", "b"},
		map[string][]string{"b": {"a"}},
		false,
	)
	seedConceptState(t, store, "L1", "a", 0.90, "review")
	seedConceptState(t, store, "L1", "b", 0.90, "review")

	snap, err := BuildOLMSnapshot(context.Background(), store, "L1", "")
	if err != nil {
		t.Fatalf("BuildOLMSnapshot: %v", err)
	}
	if snap.HasActionable {
		t.Errorf("HasActionable=true with all concepts mastered and no metacog signal, want false")
	}
	if snap.FocusConcept != "" {
		t.Errorf("FocusConcept=%q with no actionable, want empty", snap.FocusConcept)
	}
}

func TestFormatOLMEmbed_FocusWarning(t *testing.T) {
	snap := &OLMSnapshot{
		DomainID:      "d1",
		DomainName:    "Python for bio-info",
		PersonalGoal:  "analyse your garden data",
		Solid:         3,
		InProgress:    2,
		Fragile:       1,
		NotStarted:    2,
		FocusConcept:  "loops",
		FocusReason:   "retention 50%",
		FocusUrgency:  models.UrgencyWarning,
		KSTProgress:   0.50,
		HasActionable: true,
	}
	embed := FormatOLMEmbed(snap)
	if embed.Title != "🧭 Current state" {
		t.Errorf("Title=%q", embed.Title)
	}
	if !strings.Contains(embed.Description, "Python for bio-info") {
		t.Errorf("Description missing domain name: %q", embed.Description)
	}
	if !strings.Contains(embed.Description, "loops") {
		t.Errorf("Description missing focus concept: %q", embed.Description)
	}
	if !strings.Contains(embed.Description, "Current focus") {
		t.Errorf("Description should use 'Current focus' for warning urgency: %q", embed.Description)
	}
	if !strings.Contains(embed.Description, "halfway there") {
		t.Errorf("Description missing goal progress phrase: %q", embed.Description)
	}
	if embed.Color != 0xF5A623 {
		t.Errorf("Color=%#x, want amber 0xF5A623", embed.Color)
	}
}

func TestFormatOLMEmbed_FocusCriticalRedTitleAndColor(t *testing.T) {
	snap := &OLMSnapshot{
		DomainName:    "X",
		Solid:         1,
		FocusConcept:  "loops",
		FocusReason:   "retention 25%",
		FocusUrgency:  models.UrgencyCritical,
		HasActionable: true,
	}
	embed := FormatOLMEmbed(snap)
	if embed.Title != "🚨 State — one concept needs attention now" {
		t.Errorf("Title=%q", embed.Title)
	}
	if embed.Color != 0xFF6B6B {
		t.Errorf("Color=%#x, want red 0xFF6B6B", embed.Color)
	}
}

func TestNodeClassify(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		cs   *models.ConceptState
		want NodeState
	}{
		{"nil_state", nil, NodeNotStarted},
		{"new_card", &models.ConceptState{CardState: "new", PMastery: 0.0}, NodeNotStarted},
		{"solid", &models.ConceptState{CardState: "review", PMastery: 0.85, Stability: 5.0, ElapsedDays: 99, LastReview: ptrTime(now.Add(-24 * time.Hour))}, NodeSolid},
		{"in_progress", &models.ConceptState{CardState: "review", PMastery: 0.50, Stability: 5.0, ElapsedDays: 99, LastReview: ptrTime(now.Add(-24 * time.Hour))}, NodeInProgress},
		{"fragile_low_mastery", &models.ConceptState{CardState: "review", PMastery: 0.20, Stability: 5.0, ElapsedDays: 0, LastReview: ptrTime(now.Add(-24 * time.Hour))}, NodeFragile},
		{"fragile_low_retention", &models.ConceptState{CardState: "review", PMastery: 0.50, Stability: 1.0, ElapsedDays: 0, LastReview: ptrTime(now.Add(-30 * 24 * time.Hour))}, NodeFragile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NodeClassifyAt(tc.cs, now)
			if got != tc.want {
				t.Errorf("NodeClassify(%+v) = %q, want %q", tc.cs, got, tc.want)
			}
		})
	}
}

func TestNodeClassifyAt_IgnoresHistoricalElapsedDaysAfterRecentReview(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	lastReview := now.Add(-time.Hour)
	cs := &models.ConceptState{
		CardState:   "review",
		PMastery:    0.50,
		Stability:   1,
		ElapsedDays: 120,
		LastReview:  &lastReview,
	}

	if got := NodeClassifyAt(cs, now); got != NodeInProgress {
		t.Fatalf("NodeClassifyAt() = %q, want %q for recently reviewed card", got, NodeInProgress)
	}
	if got := NodeClassifyAt(cs, now.Add(30*24*time.Hour)); got != NodeFragile {
		t.Fatalf("NodeClassifyAt() after 30 days = %q, want %q", got, NodeFragile)
	}
}

func TestMetacogLine_Exported(t *testing.T) {
	got := MetacogLine(&OLMSnapshot{CalibrationBias: 2.0})
	if got == "" || got[:3] != "You" {
		t.Errorf("MetacogLine(over-confident) = %q, want non-empty starting with 'You'", got)
	}
	got = MetacogLine(&OLMSnapshot{CalibrationBias: 0.0})
	if got != "" {
		t.Errorf("MetacogLine(no signal) = %q, want empty", got)
	}
}
