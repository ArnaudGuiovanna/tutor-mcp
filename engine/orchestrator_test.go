// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"

	_ "modernc.org/sqlite"
)

// orchTestDBCounter avoids collisions across in-memory DSNs.
var orchTestDBCounter int

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
		cs := models.NewConceptState("L1", c)
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
	cs.Reps = 3
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
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

func TestOrchestrate_Diagnostic_NMaxReached_TransitionsToInstruction(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.PhaseDiagnostic)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0})

	// Inject 8 interactions to reach NDiagnosticMax
	now := time.Now().UTC()
	for i := range 8 {
		_, _ = recordSyntheticInteraction(t, store, "A", true, now.Add(time.Duration(i)*time.Second))
	}
	// Force phase_changed_at far in the past so all 8 count.
	if err := store.UpdateDomainPhase(context.Background(), domainID, models.PhaseDiagnostic, 0.469, now.Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := Orchestrate(context.Background(), store, defaultInput(domainID)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase != models.PhaseInstruction {
		t.Errorf("expected transition to INSTRUCTION via NMax, got phase=%q", d.Phase)
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

func TestOrchestrate_Maintenance_RetentionLow_TransitionsToInstruction(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.PhaseMaintenance)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0})
	// Set state with low retention (high elapsed, low stability).
	cs, _ := store.GetConceptState(context.Background(), "L1", "A")
	cs.PMastery = 0.95
	cs.CardState = "review"
	cs.Stability = 1
	cs.ElapsedDays = 30 // retention << 0.5
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	if _, err := Orchestrate(context.Background(), store, defaultInput(domainID)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, _ := store.GetDomainByID(context.Background(), domainID)
	if d.Phase != models.PhaseInstruction {
		t.Errorf("expected transition to INSTRUCTION on retention drop, got phase=%q", d.Phase)
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

// TestOrchestrateWithPhase_NoFringeFallback_ReturnedPhaseMatchesPersisted
// closes the gap left by the two FSM-path regressions above: the
// post-orchestrate phase must also match the persisted DB phase when
// the resolution comes from the *NoFringe fallback* branch (the retry
// loop in Orchestrate, not the FSM EvaluatePhase decision).
//
// Scenario forces the fallback branch:
//
//   - Phase = MAINTENANCE in DB.
//   - Goal_relevance covers A only ; A is unmastered (default mastery
//     0.1 from seedOrchDomain). FSM stays in MAINTENANCE because no
//     goal-relevant concept is "below retention" (the default state
//     CardState=="new" short-circuits the retention check in
//     buildObservables).
//   - In MAINTENANCE, selectMaintenance returns NoFringe (no concept
//     mastered → mastered pool empty). fsmTransitioned=false, so the
//     orchestrator enters the noFringeFallbackPhase retry, switches
//     currentPhase to INSTRUCTION, and persists via UpdateDomainPhase.
//   - In INSTRUCTION, A is in the external fringe (mastery 0.1 < BKT,
//     no prereqs) and eligible (rel=1.0) → activity produced.
//
// Both assertions then fire on the *fallback* exit path:
//  1. gotPhase == PhaseInstruction (the fallback target — vice-versa of
//     the docstring's INSTRUCTION→MAINTENANCE example, but exercises the
//     same noFringeFallbackPhase code path).
//  2. store.GetDomainByID(context.Background(), domainID).Phase == gotPhase (DB consistency
//     after the fallback's UpdateDomainPhase write).
func TestOrchestrateWithPhase_NoFringeFallback_ReturnedPhaseMatchesPersisted(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A"}, nil, models.PhaseMaintenance)
	setGoalRelevance(t, store, domainID, map[string]float64{"A": 1.0})
	// A stays at default mastery (0.1, CardState="new") — not mastered.
	// MAINTENANCE pipeline: no mastered concept → NoFringe.
	// FSM stays in MAINTENANCE (GoalRelevantBelowRetention=false because
	// the default state is "new", the retention check is skipped).
	// Fallback path → INSTRUCTION → A in fringe → activity produced.

	activity, gotPhase, err := OrchestrateWithPhase(context.Background(), store, defaultInput(domainID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Sanity: the fallback branch produced a real activity, not the
	// pipeline_exhausted REST escape — that would mean both phases
	// returned NoFringe and we'd be exercising the wrong path.
	if activity.Type == models.ActivityRest {
		t.Fatalf("expected fallback to produce a real activity, got REST escape (rationale=%q)", activity.Rationale)
	}
	if gotPhase != models.PhaseInstruction {
		t.Errorf("returned phase = %q, want INSTRUCTION (NoFringe fallback target)", gotPhase)
	}
	d, err := store.GetDomainByID(context.Background(), domainID)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if d.Phase != gotPhase {
		t.Errorf("returned phase %q does not match persisted phase %q (NoFringe fallback DB write missed)", gotPhase, d.Phase)
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// recordSyntheticInteraction inserts a minimal interaction row so the
// orchestrator's CountInteractionsSince and GetActionHistoryForConcept
// see something. Mimics what record_interaction would do, minus BKT/
// FSRS updates (those are tested separately in their own modules).
func recordSyntheticInteraction(t *testing.T, store *db.Store, concept string, success bool, when time.Time) (int, error) {
	t.Helper()
	successInt := 0
	if success {
		successInt = 1
	}
	conn := storeRawDB(store)
	_, err := conn.Exec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, error_type, notes, hints_requested, self_initiated, calibration_id, is_proactive_review, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, '', '', 0, 0, '', 0, ?)`,
		"L1", concept, "PRACTICE", successInt, 1000, 0.7, when,
	)
	return 0, err
}

// storeRawDB returns the underlying *sql.DB from the Store. Used by
// tests that need to insert with explicit timestamps.
func storeRawDB(store *db.Store) *sql.DB { return store.RawDB() }

// jsonOf is a tiny helper for diagnostic output in tests.
func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var _ = jsonOf // keep handy for debug
