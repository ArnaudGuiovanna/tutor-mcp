// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"math"
	"strings"
	"testing"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// reviewedConceptState builds a ConceptState that is past its first
// review (CardState != "new") so the FSRS retention override branch
// can fire when needed. Stability/ElapsedDays default to a configuration
// that yields high retention (so retention does not steal the test).
func reviewedConceptState(concept string, mastery float64) *models.ConceptState {
	cs := models.NewConceptState("L1", concept)
	cs.PMastery = mastery
	cs.CardState = "review"
	cs.Stability = 30 // generous: ensures retention >> 0.5 by default
	cs.ElapsedDays = 1
	lastReview := time.Now().UTC().Add(-24 * time.Hour)
	cs.LastReview = &lastReview
	cs.Theta = 0
	return cs
}

func setActionCardAge(cs *models.ConceptState, days int) {
	cs.ElapsedDays = days // historical value deliberately kept for fixture realism
	lastReview := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	cs.LastReview = &lastReview
}

// ─── Cascade tests ─────────────────────────────────────────────────────────

func TestSelectAction_Misconception_OverridesAll(t *testing.T) {
	cs := reviewedConceptState("Goroutines", 0.95)
	cs.Theta = 1.0
	mc := &models.MisconceptionGroup{
		Concept:           "Goroutines",
		MisconceptionType: "channel_blocking_assumption",
		Status:            "active",
	}
	a := SelectAction("Goroutines", cs, mc, ActionHistory{InteractionsAboveBKT: 5})
	if a.Type != models.ActivityDebugMisconception {
		t.Fatalf("expected DEBUG_MISCONCEPTION, got %s", a.Type)
	}
	if a.Format != "misconception_targeted" {
		t.Errorf("unexpected format: %s", a.Format)
	}
}

func TestSelectAction_MisconceptionBeatsLowRetention(t *testing.T) {
	// OQ-5.4 = A: misconception > retention. Concept has both an active
	// misconception AND retention below the recall-routing threshold.
	// Misconception must win.
	cs := reviewedConceptState("Channels", 0.5)
	cs.Stability = 1.0
	setActionCardAge(cs, 30) // pushes retention well below the recall-routing threshold
	mc := &models.MisconceptionGroup{
		Concept:           "Channels",
		MisconceptionType: "deadlock_unaware",
		Status:            "active",
	}
	a := SelectAction("Channels", cs, mc, ActionHistory{})
	if a.Type != models.ActivityDebugMisconception {
		t.Fatalf("OQ-5.4 violated: with mc+low retention, expected DEBUG_MISCONCEPTION, got %s", a.Type)
	}
}

func TestSelectAction_RetentionLow_TriggersRecall(t *testing.T) {
	cs := reviewedConceptState("Channels", 0.5)
	cs.Stability = 1.0
	setActionCardAge(cs, 30)
	a := SelectAction("Channels", cs, nil, ActionHistory{})
	if a.Type != models.ActivityRecall {
		t.Fatalf("expected RECALL_EXERCISE, got %s", a.Type)
	}
}

func TestSelectActionAt_IgnoresHistoricalElapsedDaysAfterRecentReview(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	recentReview := now.Add(-2 * time.Hour)
	cs := reviewedConceptState("Channels", 0.50)
	cs.Stability = 1
	cs.ElapsedDays = 90 // interval before the recent review, not current age
	cs.LastReview = &recentReview

	action := SelectActionAt("Channels", cs, nil, ActionHistory{}, now)
	if action.Type != models.ActivityPractice {
		t.Fatalf("recently reviewed card routed as %s, want PRACTICE (historical ElapsedDays must not force recall)", action.Type)
	}

	action = SelectActionAt("Channels", cs, nil, ActionHistory{}, now.Add(30*24*time.Hour))
	if action.Type != models.ActivityRecall {
		t.Fatalf("same card after 30 current days routed as %s, want RECALL_EXERCISE", action.Type)
	}
}

func TestSelectAction_RetentionRecallRoutingBoundary(t *testing.T) {
	cases := []struct {
		name      string
		retention float64
		want      models.ActivityType
	}{
		{
			name:      "just above routing threshold stays in mastery branch",
			retention: algorithms.RetentionRecallRoutingThreshold + 0.0001,
			want:      models.ActivityPractice,
		},
		{
			name:      "just below routing threshold routes recall",
			retention: algorithms.RetentionRecallRoutingThreshold - 0.0001,
			want:      models.ActivityRecall,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := reviewedConceptState("Channels", 0.50)
			cs.Stability = stabilityForRetention(t, tc.retention)
			setActionCardAge(cs, 1)

			a := SelectAction("Channels", cs, nil, ActionHistory{})
			if a.Type != tc.want {
				t.Fatalf("action type: got %s, want %s", a.Type, tc.want)
			}
		})
	}
}

func TestSelectAction_NewCardSkipsRetentionCheck(t *testing.T) {
	cs := models.NewConceptState("L1", "Slices") // CardState=new, PMastery=0.1
	a := SelectAction("Slices", cs, nil, ActionHistory{})
	if a.Type != models.ActivityNewConcept {
		t.Fatalf("expected NEW_CONCEPT for fresh card, got %s", a.Type)
	}
}

func TestSelectActionForPhaseAt_DiagnosticIsColdAcrossDomains(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	for _, concept := range []string{"fraction addition", "past tense in Spanish", "causes of the French Revolution", "concurrency in Go"} {
		concept := concept
		t.Run(concept, func(t *testing.T) {
			cs := models.NewConceptState("L1", concept)
			action := SelectActionForPhaseAt(models.PhaseDiagnostic, concept, cs, &models.MisconceptionGroup{
				Concept: concept, Status: "active", MisconceptionType: "pre-existing",
			}, ActionHistory{}, now)
			if action.Type != models.ActivityDiagnosticAssessment {
				t.Fatalf("type=%q, want DIAGNOSTIC_ASSESSMENT", action.Type)
			}
			if action.Format != "cold_assessment" {
				t.Fatalf("format=%q, want cold_assessment", action.Format)
			}
			if action.DifficultyTarget != 0.50 {
				t.Fatalf("difficulty=%v, want 0.50", action.DifficultyTarget)
			}
		})
	}
}

func TestSelectAction_MasteryUnder30_NewConcept(t *testing.T) {
	cs := reviewedConceptState("Maps", 0.20)
	a := SelectAction("Maps", cs, nil, ActionHistory{})
	if a.Type != models.ActivityNewConcept {
		t.Fatalf("expected NEW_CONCEPT, got %s", a.Type)
	}
}

func TestSelectAction_Mastery30To70_PracticeStandard(t *testing.T) {
	cs := reviewedConceptState("Maps", 0.50)
	a := SelectAction("Maps", cs, nil, ActionHistory{})
	if a.Type != models.ActivityPractice {
		t.Fatalf("expected PRACTICE, got %s", a.Type)
	}
	if a.Format != "practice_standard" {
		t.Errorf("expected practice_standard format, got %s", a.Format)
	}
	if a.DifficultyTarget != 0.55 {
		t.Errorf("expected DifficultyTarget=0.55, got %f", a.DifficultyTarget)
	}
}

func TestSelectAction_Mastery70To85_PracticeZPD(t *testing.T) {
	cs := reviewedConceptState("Maps", 0.78)
	cs.Theta = 0.847 // legacy estimate must not determine task difficulty
	a := SelectAction("Maps", cs, nil, ActionHistory{})
	if a.Type != models.ActivityPractice {
		t.Fatalf("expected PRACTICE, got %s", a.Type)
	}
	if a.Format != "practice_zpd" {
		t.Errorf("expected practice_zpd format, got %s", a.Format)
	}
	if a.DifficultyTarget != PracticeDifficultyTarget {
		t.Errorf("expected heuristic DifficultyTarget=%f, got %f", PracticeDifficultyTarget, a.DifficultyTarget)
	}
}

// ─── High-mastery rotation (OQ-5.2 = A: gated cascade) ────────────────────

func TestSelectAction_HighMastery_FirstIsMasteryChallenge(t *testing.T) {
	cs := reviewedConceptState("Goroutines", 0.90)
	a := SelectAction("Goroutines", cs, nil, ActionHistory{InteractionsAboveBKT: 3})
	if a.Type != models.ActivityMasteryChallenge {
		t.Fatalf("expected MASTERY_CHALLENGE on first high-mastery activity, got %s", a.Type)
	}
}

func TestSelectAction_HighMastery_RotatesToFeynman(t *testing.T) {
	cs := reviewedConceptState("Goroutines", 0.90)
	a := SelectAction("Goroutines", cs, nil, ActionHistory{
		InteractionsAboveBKT:  5,
		MasteryChallengeCount: 1,
	})
	if a.Type != models.ActivityFeynmanPrompt {
		t.Fatalf("expected FEYNMAN_PROMPT after 1 mastery challenge, got %s", a.Type)
	}
}

func TestSelectAction_HighMastery_RotatesToTransfer(t *testing.T) {
	cs := reviewedConceptState("Goroutines", 0.90)
	a := SelectAction("Goroutines", cs, nil, ActionHistory{
		InteractionsAboveBKT:  5,
		MasteryChallengeCount: 1,
		FeynmanCount:          1,
	})
	if a.Type != models.ActivityTransferProbe {
		t.Fatalf("expected TRANSFER_PROBE after MC+F, got %s", a.Type)
	}
}

func TestSelectAction_HighMastery_CycleRestartsAtMasteryChallenge(t *testing.T) {
	cs := reviewedConceptState("Goroutines", 0.90)
	a := SelectAction("Goroutines", cs, nil, ActionHistory{
		InteractionsAboveBKT:  10,
		MasteryChallengeCount: 1,
		FeynmanCount:          1,
		TransferCount:         1,
	})
	if a.Type != models.ActivityMasteryChallenge {
		t.Fatalf("expected MasteryChallenge on cycle restart (1,1,1), got %s", a.Type)
	}
}

// TestSelectAction_HighMastery_FullCycleOrder simulates 6 calls in a
// row and asserts the exact sequence MC→F→T→MC→F→T. The caller is
// responsible for incrementing history; we mimic that here.
func TestSelectAction_HighMastery_FullCycleOrder(t *testing.T) {
	cs := reviewedConceptState("Goroutines", 0.90)
	history := ActionHistory{InteractionsAboveBKT: 10}
	expected := []models.ActivityType{
		models.ActivityMasteryChallenge,
		models.ActivityFeynmanPrompt,
		models.ActivityTransferProbe,
		models.ActivityMasteryChallenge,
		models.ActivityFeynmanPrompt,
		models.ActivityTransferProbe,
	}
	for i, want := range expected {
		a := SelectAction("Goroutines", cs, nil, history)
		if a.Type != want {
			t.Fatalf("step %d: expected %s, got %s (history=%+v)", i, want, a.Type, history)
		}
		switch a.Type {
		case models.ActivityMasteryChallenge:
			history.MasteryChallengeCount++
		case models.ActivityFeynmanPrompt:
			history.FeynmanCount++
		case models.ActivityTransferProbe:
			history.TransferCount++
		}
	}
}

// ─── Stability window (OQ-5.5 = B) ─────────────────────────────────────────

func TestSelectAction_HighMastery_Unstable_StaysInPracticeZPD(t *testing.T) {
	// p ≥ 0.85 but stability window not yet met → must NOT emit MasteryChallenge.
	cs := reviewedConceptState("Goroutines", 0.86)
	cs.Theta = 1.0
	a := SelectAction("Goroutines", cs, nil, ActionHistory{InteractionsAboveBKT: 1})
	if a.Type != models.ActivityPractice {
		t.Fatalf("expected PRACTICE while unstable, got %s", a.Type)
	}
	if a.Format != "practice_zpd" {
		t.Errorf("expected practice_zpd format under stability gate, got %s", a.Format)
	}
}

// TestSelectAction_OscillationAroundThreshold reproduces the pathological
// ping-pong scenario described in OQ-5.5 design rationale: PMastery
// oscillates around 0.85, InteractionsAboveBKT resets on each dip.
// MasteryChallenge must never fire under such instability.
func TestSelectAction_OscillationAroundThreshold(t *testing.T) {
	scenarios := []struct {
		p     float64
		stab  int
		label string
	}{
		{0.86, 1, "first crossing"},
		{0.84, 0, "dip below"},
		{0.86, 1, "re-cross, reset"},
		{0.84, 0, "second dip"},
		{0.86, 2, "re-cross, partial recovery"},
	}
	for _, s := range scenarios {
		cs := reviewedConceptState("OscConcept", s.p)
		a := SelectAction("OscConcept", cs, nil, ActionHistory{InteractionsAboveBKT: s.stab})
		if a.Type == models.ActivityMasteryChallenge {
			t.Fatalf("OQ-5.5 violated at %q (p=%.2f stab=%d): MasteryChallenge fired", s.label, s.p, s.stab)
		}
	}
}

func TestSelectAction_HighMastery_StabilityExactlyAtWindow(t *testing.T) {
	cs := reviewedConceptState("Concept", 0.90)
	// Exactly N=3 → eligible.
	a := SelectAction("Concept", cs, nil, ActionHistory{InteractionsAboveBKT: HighMasteryStabilityWindow})
	if a.Type != models.ActivityMasteryChallenge {
		t.Fatalf("expected MasteryChallenge at exactly N=%d, got %s", HighMasteryStabilityWindow, a.Type)
	}
}

func TestSelectAction_HighMastery_StabilityJustUnder(t *testing.T) {
	cs := reviewedConceptState("Concept", 0.90)
	a := SelectAction("Concept", cs, nil, ActionHistory{InteractionsAboveBKT: HighMasteryStabilityWindow - 1})
	if a.Type != models.ActivityPractice {
		t.Fatalf("expected PRACTICE at N=%d (one below), got %s", HighMasteryStabilityWindow-1, a.Type)
	}
}

// ─── NaN / nil guard (OQ-5.6 = B) ──────────────────────────────────────────

func TestSelectAction_NaN_PMastery_FallsBackToRest(t *testing.T) {
	before := NaNFallbackCount()
	cs := reviewedConceptState("Concept", math.NaN())
	a := SelectAction("Concept", cs, nil, ActionHistory{})
	if a.Type != models.ActivityRest {
		t.Fatalf("expected REST on NaN PMastery, got %s", a.Type)
	}
	if NaNFallbackCount() != before+1 {
		t.Errorf("expected nanFallbackCount to increment by 1, got delta %d", NaNFallbackCount()-before)
	}
}

func TestSelectAction_NaN_LegacyThetaDoesNotBlockPractice(t *testing.T) {
	before := NaNFallbackCount()
	cs := reviewedConceptState("Concept", 0.5)
	cs.Theta = math.NaN()
	a := SelectAction("Concept", cs, nil, ActionHistory{})
	if a.Type != models.ActivityPractice {
		t.Fatalf("unused legacy theta must not block PRACTICE, got %s", a.Type)
	}
	if NaNFallbackCount() != before {
		t.Errorf("unused legacy theta triggered a fallback")
	}
}

func TestSelectAction_NilState_FallsBackToRest(t *testing.T) {
	before := NaNFallbackCount()
	a := SelectAction("Concept", nil, nil, ActionHistory{})
	if a.Type != models.ActivityRest {
		t.Fatalf("expected REST on nil state, got %s", a.Type)
	}
	if NaNFallbackCount() != before+1 {
		t.Errorf("expected nanFallbackCount to increment by 1, got delta %d", NaNFallbackCount()-before)
	}
}

// ─── MasteryBKT accessor (no literal 0.85 in code) ─────────────────────────

func TestSelectAction_RespectsMasteryBKTAccessor(t *testing.T) {
	t.Setenv("REGULATION_THRESHOLD", "off")
	// MasteryBKT() resolves to the unified 0.85 threshold; this test asserts
	// the accessor is invoked, not the literal. p=0.84 sits in the
	// practice_zpd branch.
	cs := reviewedConceptState("Concept", 0.84)
	cs.Theta = 1.0
	a := SelectAction("Concept", cs, nil, ActionHistory{InteractionsAboveBKT: 5})
	if a.Type != models.ActivityPractice || a.Format != "practice_zpd" {
		t.Errorf("expected practice_zpd at p=0.84, got %s/%s", a.Type, a.Format)
	}
}

func TestPracticeDecisionIndependentOfLegacyThetaAndFSRSDifficulty(t *testing.T) {
	for _, mastery := range []float64{0.5, 0.75, 0.90} {
		cs := reviewedConceptState("Concept", mastery)
		want := SelectAction("Concept", cs, nil, ActionHistory{})
		for _, theta := range []float64{-4, 0, 4, math.NaN(), math.Inf(1)} {
			for _, difficulty := range []float64{1, 5, 10} {
				cs.Theta, cs.Difficulty = theta, difficulty
				got := SelectAction("Concept", cs, nil, ActionHistory{})
				if got != want {
					t.Errorf("unvalidated estimates changed practice: mastery=%f theta=%f memoryDifficulty=%f got=%+v want=%+v", mastery, theta, difficulty, got, want)
				}
			}
		}
	}
}

func TestPracticeTargetIsExplicitlyHeuristic(t *testing.T) {
	cs := reviewedConceptState("Concept", 0.75)
	got := SelectAction("Concept", cs, nil, ActionHistory{})
	if got.DifficultyTarget != PracticeDifficultyTarget || !strings.Contains(got.Rationale, "heuristic") {
		t.Errorf("practice must expose a heuristic generation target, got %+v", got)
	}
	if strings.Contains(got.Rationale, "IRT") || strings.Contains(got.Rationale, "70%") {
		t.Errorf("practice must not claim calibrated IRT success probability, got %+v", got)
	}
}
