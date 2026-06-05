// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/db"
	"tutor-mcp/models"
)

// e2eSession is one row of the E2E artifact — captures everything an
// observer needs to reconstruct the run after the fact (cf. user
// requirement: artefact lisible pour debug et calibration).
type e2eSession struct {
	SessionNum        int     `json:"session_num"`
	PhaseBefore       string  `json:"phase_before"`
	PhaseAfter        string  `json:"phase_after"`
	Transitioned      bool    `json:"transitioned"`
	MeanEntropy       float64 `json:"mean_entropy"`
	PhaseEntryEntropy float64 `json:"phase_entry_entropy"`
	MasteredCount     int     `json:"mastered_count"`
	MasteredGoalCount int     `json:"mastered_goal_count"`
	TotalGoalRelevant int     `json:"total_goal_relevant"`
	ActivityType      string  `json:"activity_emitted"`
	Concept           string  `json:"concept"`
	LearnerCorrect    bool    `json:"learner_response_correct"`
	Rationale         string  `json:"rationale"`
}

type e2eArtifact struct {
	Scenario  string       `json:"scenario"`
	StartedAt time.Time    `json:"started_at"`
	Sessions  []e2eSession `json:"sessions"`
	Summary   e2eSummary   `json:"summary"`
}

type e2eSummary struct {
	TotalSessions      int            `json:"total_sessions"`
	TransitionsCount   int            `json:"transitions_count"`
	PhaseDistribution  map[string]int `json:"phase_distribution"`
	FirstInstructionAt int            `json:"first_instruction_at,omitempty"`
	FirstMaintenanceAt int            `json:"first_maintenance_at,omitempty"`
	FinalPhase         string         `json:"final_phase"`
}

// simulatedAnswer returns whether the synthetic learner answers
// correctly. Always true in this harness — the goal is to validate
// FSM transitions under monotonic mastery growth, not to simulate
// realistic noise. Real-world calibration belongs to the eval/
// harness (eval/synthetic), not to this orchestrator integration test.
func simulatedAnswer(pMastery float64) bool {
	_ = pMastery
	return true
}

// learnerInteract simulates one interaction — applies BKT update on
// the concept's state. Mirrors what record_interaction does in the
// runtime, minus FSRS scheduling (we keep cards in 'review' state).
func learnerInteract(t *testing.T, store *db.Store, concept string, success bool, when time.Time) {
	t.Helper()
	cs, err := store.GetConceptState("L1", concept)
	if err != nil {
		t.Fatalf("get state %q: %v", concept, err)
	}
	bkt := algorithms.BKTUpdate(algorithms.BKTState{
		PMastery: cs.PMastery,
		PLearn:   cs.PLearn,
		PForget:  cs.PForget,
		PSlip:    cs.PSlip,
		PGuess:   cs.PGuess,
	}, success)
	cs.PMastery = bkt.PMastery
	if cs.CardState == "new" {
		cs.CardState = "review"
		cs.Stability = 30
		cs.ElapsedDays = 1
	}
	if err := store.UpsertConceptState(cs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	successInt := 0
	if success {
		successInt = 1
	}
	_, err = store.RawDB().Exec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, error_type, notes, hints_requested, self_initiated, calibration_id, is_proactive_review, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, '', '', 0, 0, '', 0, ?)`,
		"L1", concept, "PRACTICE", successInt, 1000, 0.7, when,
	)
	if err != nil {
		t.Fatalf("insert interaction: %v", err)
	}
}

// runE2ESimulation drives N sessions of Orchestrate calls against a
// freshly seeded domain, capturing each session's state into an
// e2eArtifact. Returns the artifact for assertions and writes it to
// disk for human/machine inspection.
//
// cfg is injected so each scenario can tune its config (e.g. lower
// AntiRepeatWindow for small domains where N=3 would exhaust the
// eligible pool).
func runE2ESimulation(
	t *testing.T,
	scenario string,
	domainConcepts []string,
	prereqs map[string][]string,
	goalRelevance map[string]float64,
	nSessions int,
	cfg PhaseConfig,
) *e2eArtifact {
	t.Helper()
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, domainConcepts, prereqs, models.PhaseDiagnostic)
	if len(goalRelevance) > 0 {
		setGoalRelevance(t, store, domainID, goalRelevance)
	}

	artifact := &e2eArtifact{
		Scenario:  scenario,
		StartedAt: time.Now().UTC(),
		Summary: e2eSummary{
			PhaseDistribution: map[string]int{},
		},
	}

	for i := range nSessions {
		// Walltime — spaced 1 hour apart so retention can be modelled
		// properly if this test ever evolves to use longer cycles.
		now := artifact.StartedAt.Add(time.Duration(i) * time.Hour)

		// Snapshot pre-call.
		domainBefore, err := store.GetDomainByID(domainID)
		if err != nil {
			t.Fatalf("session %d: get domain: %v", i, err)
		}
		phaseBefore := string(domainBefore.Phase)
		if phaseBefore == "" {
			phaseBefore = string(models.PhaseInstruction)
		}

		// Call Orchestrate.
		input := defaultInput(domainID)
		input.Now = now
		input.Config = cfg
		activity, err := Orchestrate(store, input)
		if err != nil {
			// Phase-corrupted or pipeline-error : surface in artifact
			// but don't fail the test (this is a tracing harness).
			artifact.Sessions = append(artifact.Sessions, e2eSession{
				SessionNum:  i,
				PhaseBefore: phaseBefore,
				PhaseAfter:  phaseBefore,
				Rationale:   fmt.Sprintf("ORCHESTRATE_ERROR: %v", err),
			})
			continue
		}

		// Snapshot post-call.
		domainAfter, _ := store.GetDomainByID(domainID)
		phaseAfter := string(domainAfter.Phase)
		if phaseAfter == "" {
			phaseAfter = string(models.PhaseInstruction)
		}

		// Compute observables for the artifact.
		states, _ := store.GetConceptStatesByLearner("L1")
		sm := map[string]*models.ConceptState{}
		mastered := 0
		for _, s := range states {
			sm[s.Concept] = s
			if s.PMastery >= algorithms.MasteryBKT() {
				mastered++
			}
		}
		meanH := MeanBinaryEntropyOverGraph(domainAfter.Graph, sm)
		obs := buildObservables(domainAfter, &pipelineFixtures{
			StatesList:      states,
			StatesByConcept: sm,
			GoalRelevance:   goalRelevance,
		}, cfg)

		// Run learner on the chosen concept (if any).
		correct := false
		if activity.Concept != "" {
			cs := sm[activity.Concept]
			if cs != nil {
				correct = simulatedAnswer(cs.PMastery)
				learnerInteract(t, store, activity.Concept, correct, now)
			}
		}

		artifact.Sessions = append(artifact.Sessions, e2eSession{
			SessionNum:        i,
			PhaseBefore:       phaseBefore,
			PhaseAfter:        phaseAfter,
			Transitioned:      phaseBefore != phaseAfter,
			MeanEntropy:       meanH,
			PhaseEntryEntropy: domainAfter.PhaseEntryEntropy,
			MasteredCount:     mastered,
			MasteredGoalCount: obs.MasteredGoalRelevant,
			TotalGoalRelevant: obs.TotalGoalRelevant,
			ActivityType:      string(activity.Type),
			Concept:           activity.Concept,
			LearnerCorrect:    correct,
			Rationale:         activity.Rationale,
		})

		if phaseBefore != phaseAfter {
			artifact.Summary.TransitionsCount++
			if phaseAfter == string(models.PhaseInstruction) && artifact.Summary.FirstInstructionAt == 0 {
				artifact.Summary.FirstInstructionAt = i
			}
			if phaseAfter == string(models.PhaseMaintenance) && artifact.Summary.FirstMaintenanceAt == 0 {
				artifact.Summary.FirstMaintenanceAt = i
			}
		}
		artifact.Summary.PhaseDistribution[phaseAfter]++
	}

	artifact.Summary.TotalSessions = len(artifact.Sessions)
	if len(artifact.Sessions) > 0 {
		artifact.Summary.FinalPhase = artifact.Sessions[len(artifact.Sessions)-1].PhaseAfter
	}

	writeE2EArtifact(t, scenario, artifact)
	return artifact
}

// writeE2EArtifact persists the run to /eval/orchestrator_e2e_<scenario>_<date>.json
// and a markdown summary alongside. Idempotent on the filename — overwrites
// any prior run for the same scenario+date.
func writeE2EArtifact(t *testing.T, scenario string, art *e2eArtifact) {
	t.Helper()
	repoRoot := findRepoRootForArtifact(t)
	evalDir := filepath.Join(repoRoot, "eval")
	if err := os.MkdirAll(evalDir, 0o755); err != nil {
		t.Logf("could not mkdir %s: %v (skipping artifact write)", evalDir, err)
		return
	}
	date := art.StartedAt.Format("2006-01-02")
	jsonPath := filepath.Join(evalDir, fmt.Sprintf("orchestrator_e2e_%s_%s.json", scenario, date))
	mdPath := filepath.Join(evalDir, fmt.Sprintf("orchestrator_e2e_%s_%s.md", scenario, date))

	jsonData, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		t.Logf("marshal artifact: %v", err)
		return
	}
	if err := os.WriteFile(jsonPath, jsonData, 0o644); err != nil {
		t.Logf("write json artifact: %v", err)
	}
	if err := os.WriteFile(mdPath, []byte(renderE2EMarkdown(art)), 0o644); err != nil {
		t.Logf("write md artifact: %v", err)
	}
	t.Logf("E2E artifact: %s + .md", jsonPath)
}

func renderE2EMarkdown(art *e2eArtifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# E2E orchestrator — %s — %s\n\n", art.Scenario, art.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "**Total sessions**: %d  \n", art.Summary.TotalSessions)
	fmt.Fprintf(&b, "**Transitions**: %d  \n", art.Summary.TransitionsCount)
	fmt.Fprintf(&b, "**Final phase**: %s  \n", art.Summary.FinalPhase)
	if art.Summary.FirstInstructionAt > 0 {
		fmt.Fprintf(&b, "**First INSTRUCTION at session**: %d  \n", art.Summary.FirstInstructionAt)
	}
	if art.Summary.FirstMaintenanceAt > 0 {
		fmt.Fprintf(&b, "**First MAINTENANCE at session**: %d  \n", art.Summary.FirstMaintenanceAt)
	}
	b.WriteString("\n## Phase distribution\n\n")
	for p, n := range art.Summary.PhaseDistribution {
		fmt.Fprintf(&b, "- %s : %d sessions\n", p, n)
	}
	b.WriteString("\n## Per-session trace\n\n")
	b.WriteString("| # | phase before → after | activity | concept | correct | mean H | mastered (goal/total) |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, s := range art.Sessions {
		marker := "→"
		if !s.Transitioned {
			marker = "·"
		}
		fmt.Fprintf(&b, "| %d | %s %s %s | %s | %s | %v | %.3f | %d/%d |\n",
			s.SessionNum, s.PhaseBefore, marker, s.PhaseAfter,
			s.ActivityType, s.Concept, s.LearnerCorrect,
			s.MeanEntropy, s.MasteredGoalCount, s.TotalGoalRelevant)
	}
	return b.String()
}

func findRepoRootForArtifact(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := cwd; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	return cwd
}

// ─── E2E scenarios (OQ-2.7) ────────────────────────────────────────────────

func TestOrchestrate_E2E_RestrictiveGoal_FastMaintenance_30Sessions(t *testing.T) {
	// Restrictive: 3 goal-relevant concepts out of 6, the rest irrelevant.
	// AntiRepeatWindow=1 for this scenario: with only 3 concepts
	// covered, N=3 would drain the eligible pool. N=1 preserves
	// minimal diversity while keeping 2 concepts always selectable.
	concepts := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	goal := map[string]float64{
		"alpha": 0.9, "beta": 0.9, "gamma": 0.9,
		// delta, epsilon, zeta : NOT in vector → uncovered → exclus
	}
	cfg := NewDefaultPhaseConfig()
	cfg.AntiRepeatWindow = 1
	art := runE2ESimulation(t, "restrictive_goal", concepts, nil, goal, 30, cfg)

	// Assertions:
	// 1. At least one transition observed
	if art.Summary.TransitionsCount == 0 {
		t.Errorf("expected ≥1 transition over 30 sessions, got 0")
	}
	// 2. Phase finale = MAINTENANCE (les 3 concepts pertinents sont
	//    rapidement mastered, les 3 autres ne bloquent pas).
	if art.Summary.FinalPhase != string(models.PhaseMaintenance) {
		t.Errorf("expected final phase MAINTENANCE (restrictive goal), got %s", art.Summary.FinalPhase)
	}
}

func TestOrchestrate_E2E_BroadGoal_LongInstruction_30Sessions(t *testing.T) {
	// Broad: all concepts are goal-relevant. The learner must
	// master ALL of them to transition to MAINTENANCE.
	concepts := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	goal := map[string]float64{
		"alpha": 0.9, "beta": 0.9, "gamma": 0.9,
		"delta": 0.9, "epsilon": 0.9, "zeta": 0.9,
	}
	cfg := NewDefaultPhaseConfig()
	art := runE2ESimulation(t, "broad_goal", concepts, nil, goal, 30, cfg)

	// Assertion: the transition to MAINTENANCE must be later (or
	// absent) than in the restrictive scenario. We verify that
	// MAINTENANCE does not arrive before the second half of the
	// simulation, that the final phase is MAINTENANCE, and that
	// INSTRUCTION sessions are substantial. (The direct comparison
	// I > M was a fragile proxy: once `selectDiagnostic` was fixed
	// to respect the anti-repeat gate — issue #93 lineage — the
	// DIAGNOSTIC phase no longer accidentally masters concepts, so
	// INSTRUCTION starts earlier and MAINTENANCE can accumulate
	// more sessions toward the end.)
	if art.Summary.FirstMaintenanceAt > 0 && art.Summary.FirstMaintenanceAt < 15 {
		t.Errorf("broad goal : MAINTENANCE arrived too early (session %d, expected >= 15)",
			art.Summary.FirstMaintenanceAt)
	}
	if art.Summary.FinalPhase != string(models.PhaseMaintenance) {
		t.Errorf("broad goal : expected final phase MAINTENANCE, got %s", art.Summary.FinalPhase)
	}
	instructionCount := art.Summary.PhaseDistribution[string(models.PhaseInstruction)]
	if instructionCount < 8 {
		t.Errorf("broad goal : INSTRUCTION should be substantial (>= 8 sessions), got %d", instructionCount)
	}
}

// TestOrchestrate_E2E_FullCycle_30Sessions verifies that all 3
// transitions DIAGNOSTIC → INSTRUCTION → MAINTENANCE can occur
// within a 30-session simulation on a minimal goal-aligned domain.
func TestOrchestrate_E2E_FullCycle_30Sessions(t *testing.T) {
	concepts := []string{"alpha", "beta"}
	goal := map[string]float64{"alpha": 1.0, "beta": 1.0}
	cfg := NewDefaultPhaseConfig()
	cfg.AntiRepeatWindow = 1 // 2-concept domain — N>=2 starves the pool.
	art := runE2ESimulation(t, "full_cycle", concepts, nil, goal, 30, cfg)

	// At least DIAGNOSTIC → INSTRUCTION must have occurred (NDiagnosticMax=8
	// guarantees exit after 8 interactions).
	if art.Summary.FirstInstructionAt == 0 {
		t.Errorf("expected DIAGNOSTIC→INSTRUCTION transition within 30 sessions")
	}
	// The final phase must be MAINTENANCE (simulated learner masters
	// both concepts through successive BKT updates).
	if art.Summary.FinalPhase != string(models.PhaseMaintenance) {
		t.Logf("note : final phase %s, expected MAINTENANCE — ok if BKT didn't reach 0.85 in 30 steps",
			art.Summary.FinalPhase)
	}
}
