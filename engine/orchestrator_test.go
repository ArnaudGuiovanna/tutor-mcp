// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	_ "modernc.org/sqlite"
)

// orchTestDBCounter avoids collisions across in-memory DSNs.
var orchTestDBCounter int

type failActionHistoryStore struct{ storeport.Store }

func (s *failActionHistoryStore) GetActionHistoryForConceptInDomain(context.Context, string, string, string, int) (models.ActionHistoryCounts, error) {
	return models.ActionHistoryCounts{}, errors.New("injected action-history read failure")
}

// setupOrchStore returns a freshly migrated in-memory Store with a
// learner already inserted. The orchestrator tests reuse this helper.
func setupOrchStore(t *testing.T) *db.Store {
	t.Helper()
	orchTestDBCounter++
	dsn := fmt.Sprintf("file:orch_%s_%d?mode=memory&cache=shared", t.Name(), orchTestDBCounter)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, ?, ?, ?)`,
		"L1", "test@test.com", "hash", "test", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	return db.NewStore(conn)
}

// seedOrchDomain creates a domain and ConceptStates for each concept,
// optionally setting the phase. Returns the domain ID. Named with
// "Orch" suffix to avoid collision with engine/olm_test.go's seedDomain.
func seedOrchDomain(t *testing.T, store *db.Store, concepts []string, prereqs map[string][]string, phase models.Phase) string {
	t.Helper()
	domain, err := store.CreateDomain(context.Background(), "L1", "TestDomain", "personal goal", models.KnowledgeSpace{
		Concepts: concepts, Prerequisites: prereqs,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range concepts {
		cs := models.NewConceptStateInDomain("L1", domain.ID, c)
		if err := store.InsertConceptStateIfNotExists(context.Background(), cs); err != nil {
			t.Fatal(err)
		}
	}
	if phase != "" {
		// Snapshot current entropy ONLY for DIAGNOSTIC entries.
		entry := 0.0
		if phase == models.PhaseDiagnostic {
			states, _ := store.GetConceptStatesByLearner(context.Background(), "L1")
			sm := map[string]*models.ConceptState{}
			for _, s := range states {
				sm[s.Concept] = s
			}
			entry = MeanBinaryEntropyOverGraph(domain.Graph, sm)
		}
		if err := store.UpdateDomainPhase(context.Background(), domain.ID, phase, entry, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	return domain.ID
}

func setMastery(t *testing.T, store *db.Store, concept string, p float64) {
	t.Helper()
	cs, err := store.GetConceptState(context.Background(), "L1", concept)
	if err != nil {
		t.Fatal(err)
	}
	cs.PMastery = p
	cs.CardState = "review"
	cs.Stability = 30
	cs.ElapsedDays = 1
	lastReview := time.Now().UTC().Add(-24 * time.Hour)
	cs.LastReview = &lastReview
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
}

func setGoalRelevance(t *testing.T, store *db.Store, domainID string, rel map[string]float64) {
	t.Helper()
	if _, err := store.MergeDomainGoalRelevance(context.Background(), domainID, rel); err != nil {
		t.Fatal(err)
	}
}

func defaultInput(domainID string) OrchestratorInput {
	return OrchestratorInput{
		LearnerID: "L1",
		DomainID:  domainID,
		Now:       time.Now().UTC(),
		Config:    NewDefaultPhaseConfig(),
	}
}

func setReviewState(t *testing.T, store *db.Store, concept string, p, stability float64, elapsedDays int) {
	t.Helper()
	cs, err := store.GetConceptState(context.Background(), "L1", concept)
	if err != nil {
		t.Fatal(err)
	}
	cs.PMastery = p
	cs.CardState = "review"
	cs.Stability = stability
	cs.ElapsedDays = elapsedDays
	lastReview := time.Now().UTC().Add(-time.Duration(elapsedDays) * 24 * time.Hour)
	cs.LastReview = &lastReview
	cs.Reps = 3
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
}

func TestBuildObservablesUsesCurrentFSRSAge(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	lastReview := now.Add(-2 * time.Hour)
	cs := &models.ConceptState{
		Concept:     "A",
		PMastery:    0.95,
		CardState:   "review",
		Stability:   1,
		ElapsedDays: 90,
		LastReview:  &lastReview,
	}
	domain := &models.Domain{Graph: models.KnowledgeSpace{Concepts: []string{"A"}}}
	fixtures := &pipelineFixtures{
		StatesList:      []*models.ConceptState{cs},
		StatesByConcept: map[string]*models.ConceptState{"A": cs},
		GoalRelevance:   map[string]float64{"A": 1},
	}
	cfg := NewDefaultPhaseConfig()

	got := buildObservables(domain, fixtures, cfg, now)
	if got.GoalRelevantBelowRetention {
		t.Fatal("recent review was treated as 90 days old from historical ElapsedDays")
	}

	got = buildObservables(domain, fixtures, cfg, now.Add(30*24*time.Hour))
	if !got.GoalRelevantBelowRetention {
		t.Fatal("30 days of current age should trigger the retention observable")
	}
}

// ─── Phase NULL → INSTRUCTION fallback (legacy) ────────────────────────────

func TestOrchestrate_DomainPhaseNull_DefaultsToInstruction(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, "") // empty phase = NULL
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 0.9, "B": 0.5})

	activity, err := Orchestrate(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activity.Type == "" {
		t.Errorf("expected an Activity, got empty Type")
	}
	// The phase remains NULL in DB (orchestrator only read, did not
	// write — no transition because already in INSTRUCTION).
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase != "" {
		t.Errorf("expected phase to remain NULL on legacy domain, got %q", d.Phase)
	}
}

func TestOrchestrate_ForgettingCriticalBypassesInstructionPrereqAndArgmax(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store,
		[]string{"prereq", "forgotten", "fresh"},
		map[string][]string{"forgotten": {"prereq"}},
		models.PhaseInstruction,
	)
	setGoalRelevance(t, store, domainID, map[string]float64{
		"prereq":    1.0,
		"forgotten": 1.0,
		"fresh":     1.0,
	})
	setReviewState(t, store, "prereq", 0.10, 30, 1)
	setReviewState(t, store, "forgotten", 0.95, 1, 80)
	setReviewState(t, store, "fresh", 0.10, 30, 1)

	activity, err := Orchestrate(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activity.Concept != "forgotten" {
		t.Fatalf("concept = %q, want forgotten critical-retention concept; activity=%+v", activity.Concept, activity)
	}
	if activity.Type != models.ActivityRecall {
		t.Fatalf("activity type = %q, want RECALL_EXERCISE; activity=%+v", activity.Type, activity)
	}
	if !strings.Contains(activity.Rationale, "INSTRUCTION+bypass_forgetting") {
		t.Fatalf("rationale should expose bypass phase, got %q", activity.Rationale)
	}
	if !strings.Contains(activity.Rationale, "retention FSRS basse") {
		t.Fatalf("rationale should keep action-selector retention reason, got %q", activity.Rationale)
	}
}

func TestOrchestrate_UnknownDomain_ReturnsError(t *testing.T) {
	store := setupOrchStore(t)
	_, err := Orchestrate(context.Background(), store, defaultInput("nonexistent"))
	if !errors.Is(err, ErrUnknownDomain) {
		t.Errorf("expected ErrUnknownDomain, got %v", err)
	}
}

// ─── DIAGNOSTIC → INSTRUCTION ──────────────────────────────────────────────

func TestOrchestrate_DiagnosticEmitsColdAssessment(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"Spanish past tense"}, nil, models.PhaseDiagnostic)
	setGoalRelevance(t, store, domainID, map[string]float64{"Spanish past tense": 1.0})

	input := defaultInput(domainID)
	input.Now = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	input.SessionStart = input.Now
	activity, err := Orchestrate(context.Background(), store, input)
	if err != nil {
		t.Fatalf("Orchestrate: %v", err)
	}
	if activity.Type != models.ActivityDiagnosticAssessment || activity.Format != "cold_assessment" {
		t.Fatalf("diagnostic activity=%+v, want cold DIAGNOSTIC_ASSESSMENT", activity)
	}
	if !strings.Contains(activity.PromptForLLM, "before giving any explanation") ||
		!strings.Contains(activity.PromptForLLM, "Do not teach") {
		t.Fatalf("diagnostic prompt permits teaching before measurement: %q", activity.PromptForLLM)
	}
}

func TestOrchestrate_Diagnostic_NMaxReached_TransitionsToInstruction(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.PhaseDiagnostic)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0})

	// Unrelated practice events cannot force the diagnostic cap.
	now := time.Now().UTC()
	eventStart := now.Add(-30 * time.Minute)
	for i := range 8 {
		_, _ = recordSyntheticInteraction(t, store, domainID, "A", string(models.ActivityPractice), true, eventStart.Add(time.Duration(i)*time.Second))
	}
	// Force phase_changed_at far in the past so all 8 count.
	if err := store.UpdateDomainPhase(context.Background(), domainID, models.PhaseDiagnostic, 0.469, now.Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := Orchestrate(context.Background(), store, defaultInput(domainID)); err != nil {
		t.Fatalf("unexpected error before diagnostic evidence: %v", err)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase != models.PhaseDiagnostic {
		t.Fatalf("practice interactions forced diagnostic exit: phase=%q", d.Phase)
	}

	// Eight actual cold assessments reach NDiagnosticMax.
	for i := range 8 {
		_, _ = recordSyntheticInteraction(t, store, domainID, "A", string(models.ActivityDiagnosticAssessment), true, eventStart.Add(time.Duration(20+i)*time.Second))
	}

	if _, err := Orchestrate(context.Background(), store, defaultInput(domainID)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, _ = store.GetDomainByID(context.Background(), domainID)
	if d.Phase != models.PhaseInstruction {
		t.Errorf("expected transition to INSTRUCTION via NMax, got phase=%q", d.Phase)
	}
}

func TestOrchestrate_DiagnosticRequiresAttemptLinkedHintFreeDistinctCoverage(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B", "C"}, nil, models.PhaseDiagnostic)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1, "B": 1, "C": 1})
	now := time.Now().UTC()
	if err := store.UpdateDomainPhase(context.Background(), domainID, models.PhaseDiagnostic, 0.469, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Eight public diagnostic-shaped observations without an assessment
	// envelope are routing data only and contribute zero qualified coverage.
	for i := range 8 {
		if err := store.CreateInteraction(context.Background(), &models.Interaction{
			LearnerID: "L1", DomainID: domainID, Concept: "A",
			ActivityType: string(models.ActivityDiagnosticAssessment), Success: true,
			CreatedAt: now.Add(-50*time.Minute + time.Duration(i)*time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// An otherwise valid evaluated diagnostic that requested a hint is also
	// excluded from cold-diagnostic coverage.
	hintedAt := now.Add(-40 * time.Minute)
	hintedAttempt := insertQualifiedDiagnosticEnvelope(t, store, "L1", domainID, "B", true, hintedAt)
	if err := store.CreateInteraction(context.Background(), &models.Interaction{
		LearnerID: "L1", DomainID: domainID, AssessmentAttemptID: hintedAttempt,
		Concept: "B", ActivityType: string(models.ActivityDiagnosticAssessment),
		Success: true, HintsRequested: 1, CreatedAt: hintedAt,
	}); err != nil {
		t.Fatal(err)
	}

	// Repeating one qualified concept cannot impersonate coverage of the
	// three-concept curriculum.
	for i := range 8 {
		_, err := recordSyntheticInteraction(t, store, domainID, "A", string(models.ActivityDiagnosticAssessment), true, now.Add(-30*time.Minute+time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	// Qualified rows for concepts no longer present in the active curriculum
	// are preserved for audit but cannot satisfy current-domain coverage.
	for index, stale := range []string{"RETIRED-1", "RETIRED-2"} {
		if _, err := recordSyntheticInteraction(t, store, domainID, stale, string(models.ActivityDiagnosticAssessment), true, now.Add(-20*time.Minute+time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	input := defaultInput(domainID)
	input.Now = now
	input.SessionStart = now
	if _, err := Orchestrate(context.Background(), store, input); err != nil {
		t.Fatal(err)
	}
	domain, _ := store.GetDomainByID(context.Background(), domainID)
	if domain.Phase != models.PhaseDiagnostic {
		t.Fatalf("unlinked, hinted, or repeated diagnostics bypassed coverage: phase=%q", domain.Phase)
	}

	for index, concept := range []string{"B", "C"} {
		if _, err := recordSyntheticInteraction(t, store, domainID, concept, string(models.ActivityDiagnosticAssessment), true, now.Add(-10*time.Minute+time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Orchestrate(context.Background(), store, input); err != nil {
		t.Fatal(err)
	}
	domain, _ = store.GetDomainByID(context.Background(), domainID)
	if domain.Phase != models.PhaseInstruction {
		t.Fatalf("all three qualified distinct concepts should complete diagnostic, phase=%q", domain.Phase)
	}
}

// ─── INSTRUCTION → MAINTENANCE ─────────────────────────────────────────────

func TestOrchestrate_Instruction_AllGoalMastered_TransitionsToMaintenance(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0, "B": 0.8})
	setMastery(t, store, "A", 0.95)
	setMastery(t, store, "B", 0.95)

	if _, err := Orchestrate(context.Background(), store, defaultInput(domainID)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase != models.PhaseMaintenance {
		t.Errorf("expected transition to MAINTENANCE, got phase=%q", d.Phase)
	}
}

func TestOrchestrate_UsesInjectedLoggerLevelForFSM(t *testing.T) {
	for _, tc := range []struct {
		name    string
		level   slog.Level
		wantLog bool
	}{
		{name: "info enabled", level: slog.LevelInfo, wantLog: true},
		{name: "info suppressed", level: slog.LevelWarn, wantLog: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := setupOrchStore(t)
			domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
			setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0, "B": 0.8})
			setMastery(t, store, "A", 0.95)
			setMastery(t, store, "B", 0.95)

			var buf bytes.Buffer
			input := defaultInput(domainID)
			input.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level}))

			if _, err := Orchestrate(context.Background(), store, input); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotLog := strings.Contains(buf.String(), "phase transition (FSM)")
			if gotLog != tc.wantLog {
				t.Fatalf("phase transition log present=%v, want %v; logs=%q", gotLog, tc.wantLog, buf.String())
			}
		})
	}
}

// ─── MAINTENANCE → INSTRUCTION ─────────────────────────────────────────────

func TestOrchestrate_Maintenance_RetentionLowRecallsWithoutPhaseChange(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.PhaseMaintenance)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0})
	// Set state with low retention (high elapsed, low stability).
	cs, _ := store.GetConceptState(context.Background(), "L1", "A")
	cs.PMastery = 0.95
	cs.CardState = "review"
	cs.Stability = 1
	cs.ElapsedDays = 30 // retention << 0.5
	lastReview := time.Now().UTC().Add(-30 * 24 * time.Hour)
	cs.LastReview = &lastReview
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	activity, err := Orchestrate(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activity.Type != models.ActivityRecall || activity.Concept != "A" {
		t.Fatalf("forgotten concept needs recall, got %+v", activity)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase != models.PhaseMaintenance {
		t.Errorf("recall need should stay in MAINTENANCE, got phase=%q", d.Phase)
	}
}

func TestOrchestrate_AntiRepeatNeverStarvesAcquisition(t *testing.T) {
	for _, chain := range []bool{true, false} {
		t.Run(fmt.Sprintf("prerequisite_chain=%t", chain), func(t *testing.T) {
			store := setupOrchStore(t)
			var prereqs map[string][]string
			if chain {
				prereqs = map[string][]string{"B": {"A"}, "C": {"B"}}
			}
			domainID := seedOrchDomain(t, store, []string{"A", "B", "C"}, prereqs, models.PhaseInstruction)
			if !chain {
				// The gate has other candidates, but they no longer belong to
				// the acquisition fringe. Diversity must relax after selection.
				setMastery(t, store, "B", 0.95)
				setMastery(t, store, "C", 0.95)
			}
			input := defaultInput(domainID)
			if _, err := recordSyntheticInteraction(t, store, domainID, "A", "PRACTICE", false, input.Now.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			for call := 0; call < 3; call++ {
				activity, phase, err := OrchestrateWithPhase(context.Background(), store, input)
				if err != nil {
					t.Fatal(err)
				}
				if activity.Concept != "A" || activity.Type == models.ActivityRest || phase != models.PhaseInstruction {
					t.Fatalf("call %d: accessible acquisition must remain available, phase=%s activity=%+v", call, phase, activity)
				}
			}
		})
	}
}

func TestOrchestrate_AntiRepeatZeroRemainsDisabled(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1, "B": 0.1})
	input := defaultInput(domainID)
	if _, err := recordSyntheticInteraction(t, store, domainID, "A", "PRACTICE", false, input.Now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	input.Config.AntiRepeatWindow = 0
	activity, err := Orchestrate(context.Background(), store, input)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Concept != "A" {
		t.Fatalf("zero window should allow most relevant recent concept A, got %+v", activity)
	}
}

func TestOrchestrate_RecallNeedIndependentOfAcquisitionPhase(t *testing.T) {
	for _, withAcquisitionGap := range []bool{false, true} {
		t.Run(fmt.Sprintf("acquisition_gap=%t", withAcquisitionGap), func(t *testing.T) {
			store := setupOrchStore(t)
			concepts := []string{"A"}
			wantPhase := models.PhaseMaintenance
			if withAcquisitionGap {
				concepts = append(concepts, "B")
				wantPhase = models.PhaseInstruction
			}
			domainID := seedOrchDomain(t, store, concepts, nil, models.PhaseMaintenance)
			// R ~= .388: recall is due, but the critical-forgetting bypass
			// does not apply. High acquisition mastery must not hide this need.
			setReviewState(t, store, "A", 0.95, 1, 24)
			input := defaultInput(domainID)
			for call := 0; call < 3; call++ {
				activity, phase, err := OrchestrateWithPhase(context.Background(), store, input)
				if err != nil {
					t.Fatal(err)
				}
				if activity.Type != models.ActivityRecall || activity.Concept != "A" || phase != wantPhase {
					t.Fatalf("call %d: recall must remain available without phase oscillation, phase=%s activity=%+v", call, phase, activity)
				}
			}
		})
	}
}

// ─── OQ-2.7 : Goal-relevant cutoff (uncovered exclusion) ───────────────────

func TestOrchestrate_GoalRelevant_RestrictiveGoal_FastMaintenance(t *testing.T) {
	// Restrictive goal: 1 goal-relevant concept out of 5 → MAINTENANCE
	// as soon as that concept is mastered, regardless of the others.
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B", "C", "D", "E"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 0.9}) // B-E uncovered
	setMastery(t, store, "A", 0.95)                                    // only A mastered
	// B-E remain at mastery=0.1 default — not goal-relevant

	if _, err := Orchestrate(context.Background(), store, defaultInput(domainID)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase != models.PhaseMaintenance {
		t.Errorf("expected MAINTENANCE (only goal-relevant mastered), got %q", d.Phase)
	}
}

func TestOrchestrate_GoalRelevant_BroadGoal_StaysInstruction(t *testing.T) {
	// Broad goal: 5 goal-relevant concepts, only 1 mastered → stays INSTRUCTION.
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B", "C", "D", "E"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{
		"A": 0.9, "B": 0.9, "C": 0.9, "D": 0.9, "E": 0.9,
	})
	setMastery(t, store, "A", 0.95) // only A mastered
	// B-E at 0.1 default

	if _, err := Orchestrate(context.Background(), store, defaultInput(domainID)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase == models.PhaseMaintenance {
		t.Errorf("expected stay INSTRUCTION (4/5 not mastered), got %q", d.Phase)
	}
}

// ─── Phase invalide en DB → INSTRUCTION fallback ──────────────────────────

func TestOrchestrate_PhaseCorruptedInDB_FallsBackGracefully(t *testing.T) {
	// If the DB contains an unrecognised phase, the orchestrator must
	// not crash. The FSM EvaluatePhase ignores it (no transition).
	// The pipeline runs with the DB value (Gate will refuse with an
	// error, but we capture the graceful fallback).
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.Phase("BOGUS"))
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0})

	_, err := Orchestrate(context.Background(), store, defaultInput(domainID))
	// Gate returns ErrGateUnknownPhase → propagated as a pipeline
	// error. This is the expected behaviour (consistent with
	// OQ-4.1/OQ-2.5 explicit-error).
	if err == nil {
		t.Fatalf("expected error on corrupted phase, got nil")
	}
}

// ─── No-transition cases ───────────────────────────────────────────────────

func TestOrchestrate_NoTransition_PhasePersists(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 0.9, "B": 0.9})
	setMastery(t, store, "A", 0.5) // not mastered → no transition
	setMastery(t, store, "B", 0.5)

	if _, err := Orchestrate(context.Background(), store, defaultInput(domainID)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase != models.PhaseInstruction {
		t.Errorf("expected phase to remain INSTRUCTION, got %q", d.Phase)
	}
}

// ─── OrchestrateWithPhase contract (perf #91) ──────────────────────────────

// TestOrchestrateWithPhase_ReturnedPhaseMatchesPersisted is the
// regression test for the perf #91 change: the post-orchestrate phase
// reported by OrchestrateWithPhase must match the phase the
// orchestrator just persisted to the DB. Drives the FSM from
// INSTRUCTION → MAINTENANCE so a transition actually happens, then
// asserts (returned phase) == (DB phase).
func TestOrchestrateWithPhase_ReturnedPhaseMatchesPersisted(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0, "B": 0.8})
	setMastery(t, store, "A", 0.95)
	setMastery(t, store, "B", 0.95)

	_, gotPhase, err := OrchestrateWithPhase(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPhase != models.PhaseMaintenance {
		t.Errorf("returned phase = %q, want MAINTENANCE", gotPhase)
	}
	d, err := store.GetDomainByID(context.Background(), domainID)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if d.Phase != gotPhase {
		t.Errorf("returned phase %q does not match persisted phase %q", gotPhase, d.Phase)
	}
}

func TestOrchestrateWithPhase_PreviewProjectsWithoutPersisting(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0, "B": 0.8})
	setMastery(t, store, "A", 0.95)
	setMastery(t, store, "B", 0.95)
	input := defaultInput(domainID)
	input.PreviewOnly = true

	_, projected, err := OrchestrateWithPhase(context.Background(), store, input)
	if err != nil {
		t.Fatalf("unexpected preview error: %v", err)
	}
	if projected != models.PhaseMaintenance {
		t.Fatalf("projected phase = %q, want MAINTENANCE", projected)
	}
	stored, err := store.GetDomainByID(context.Background(), domainID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != models.PhaseInstruction {
		t.Fatalf("preview persisted phase %q; want original INSTRUCTION", stored.Phase)
	}
}

func TestOrchestrateWithPhase_PipelineFailureDoesNotPersistProjectedPhase(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0, "B": 0.8})
	setMastery(t, store, "A", 0.95)
	setMastery(t, store, "B", 0.95)

	_, _, err := OrchestrateWithPhase(context.Background(), &failActionHistoryStore{Store: store}, defaultInput(domainID))
	if err == nil {
		t.Fatal("expected injected pipeline read failure")
	}
	stored, getErr := store.GetDomainByID(context.Background(), domainID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Phase != models.PhaseInstruction {
		t.Fatalf("failed pipeline persisted projected phase %q; want INSTRUCTION", stored.Phase)
	}
}

// TestOrchestrateWithPhase_NoTransition_ReturnsCurrentPhase asserts
// the no-transition case: when the FSM does not move, the returned
// phase is the (unchanged) current phase, still matching the DB.
func TestOrchestrateWithPhase_NoTransition_ReturnsCurrentPhase(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, nil, models.PhaseInstruction)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 0.9, "B": 0.9})
	setMastery(t, store, "A", 0.5)
	setMastery(t, store, "B", 0.5)

	_, gotPhase, err := OrchestrateWithPhase(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPhase != models.PhaseInstruction {
		t.Errorf("returned phase = %q, want INSTRUCTION", gotPhase)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	// DB phase may be empty (no transition was persisted) but the
	// effective phase reported is the resolved INSTRUCTION default.
	if d.Phase != "" && d.Phase != gotPhase {
		t.Errorf("returned phase %q does not match persisted phase %q", gotPhase, d.Phase)
	}
}

// A previously maintained domain can acquire a new gap. The FSM returns to
// instruction and the phase reported with the activity matches its CAS write.
func TestOrchestrateWithPhase_AcquisitionGap_ReturnedPhaseMatchesPersisted(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.PhaseMaintenance)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0})
	// A's initial estimate represents an acquisition gap, without any recall
	// history. That is sufficient to return the domain to instruction.

	activity, gotPhase, err := OrchestrateWithPhase(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activity.Type == models.ActivityRest {
		t.Fatalf("expected acquisition activity, got REST escape (rationale=%q)", activity.Rationale)
	}
	if gotPhase != models.PhaseInstruction {
		t.Errorf("returned phase = %q, want INSTRUCTION for acquisition gap", gotPhase)
	}
	d, err := store.GetDomainByID(context.Background(), domainID)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if d.Phase != gotPhase {
		t.Errorf("returned phase %q does not match persisted phase %q", gotPhase, d.Phase)
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// recordSyntheticInteraction inserts a minimal interaction row so the
// orchestrator's CountInteractionsSince and GetActionHistoryForConcept
// see something. Mimics what record_interaction would do, minus BKT/
// FSRS updates (those are tested separately in their own modules).
func recordSyntheticInteraction(t *testing.T, store *db.Store, domainID, concept, activityType string, success bool, when time.Time) (int, error) {
	t.Helper()
	successInt := 0
	if success {
		successInt = 1
	}
	conn := storeRawDB(store)
	attemptID := ""
	if activityType == string(models.ActivityDiagnosticAssessment) {
		attemptID = insertQualifiedDiagnosticEnvelope(t, store, "L1", domainID, concept, success, when)
	}
	_, err := conn.Exec(
		`INSERT INTO interactions (learner_id, domain_id, assessment_attempt_id, concept, activity_type, success, response_time, confidence, error_type, notes, hints_requested, self_initiated, calibration_id, is_proactive_review, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '', 0, 0, '', 0, ?)`,
		"L1", domainID, nullStringForTest(attemptID), concept, activityType, successInt, 1000, 0.7, when,
	)
	return 0, err
}

func insertQualifiedDiagnosticEnvelope(t *testing.T, store *db.Store, learnerID, domainID, concept string, success bool, when time.Time) string {
	t.Helper()
	attemptID := fmt.Sprintf("diag-%s-%d", strings.ReplaceAll(concept, " ", "-"), when.UnixNano())
	passed := 0
	if success {
		passed = 1
	}
	createdAt := when.Add(-2 * time.Minute)
	submittedAt := when.Add(-time.Minute)
	_, err := store.RawDB().Exec(`INSERT INTO assessment_attempts
		(id, learner_id, domain_id, concept_id, activity_id, activity_version,
		 activity_type, observable, task_text, task_content_hash, rubric_json, passing_score,
		 status, response_text, rubric_score_json, score, passed, evaluator_id,
		 evaluation_method, trusted_evaluation, created_at, submitted_at, evaluated_at)
		VALUES (?, ?, ?, ?, ?, 1, 'DIAGNOSTIC_ASSESSMENT', 'cold response',
		 'diagnostic task', '', '{"criteria":["correct"]}', 0.6, 'evaluated',
		 'learner response', '{"correct":1}', ?, ?, 'test-host', 'host_llm', 0,
		 ?, ?, ?)`,
		attemptID, learnerID, domainID, concept, attemptID+"-activity",
		map[bool]float64{true: 1, false: 0}[success], passed,
		createdAt, submittedAt, when)
	if err != nil {
		t.Fatalf("insert qualified diagnostic envelope: %v", err)
	}
	return attemptID
}

func nullStringForTest(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// storeRawDB returns the underlying *sql.DB from the Store. Used by
// tests that need to insert with explicit timestamps.
func storeRawDB(store *db.Store) *sql.DB { return store.RawDB() }
