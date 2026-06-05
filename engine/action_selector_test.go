// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"math"
	"testing"

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
	cs.Theta = 0
	return cs
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
	cs.ElapsedDays = 30 // pushes retention well below the recall-routing threshold
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
	cs.ElapsedDays = 30
	a := SelectAction("Channels", cs, nil, ActionHistory{})
	if a.Type != models.ActivityRecall {
		t.Fatalf("expected RECALL_EXERCISE, got %s", a.Type)
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
			cs.ElapsedDays = 1

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
	cs.Theta = 0.847 // → b=0 → sigmoid=0.5
	a := SelectAction("Maps", cs, nil, ActionHistory{})
	if a.Type != models.ActivityPractice {
		t.Fatalf("expected PRACTICE, got %s", a.Type)
	}
	if a.Format != "practice_zpd" {
		t.Errorf("expected practice_zpd format, got %s", a.Format)
	}
	if math.Abs(a.DifficultyTarget-0.50) > 0.01 {
		t.Errorf("expected DifficultyTarget≈0.50 at theta=0.847, got %f", a.DifficultyTarget)
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

func TestSelectAction_NaN_Theta_FallsBackToRest(t *testing.T) {
	before := NaNFallbackCount()
	cs := reviewedConceptState("Concept", 0.5)
	cs.Theta = math.NaN()
	a := SelectAction("Concept", cs, nil, ActionHistory{})
	if a.Type != models.ActivityRest {
		t.Fatalf("expected REST on NaN Theta, got %s", a.Type)
	}
	if NaNFallbackCount() != before+1 {
		t.Errorf("expected nanFallbackCount to increment by 1, got delta %d", NaNFallbackCount()-before)
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

// ─── ZPD formula (OQ-5.3) ──────────────────────────────────────────────────

func TestZPDDifficulty_TargetsPCorrect70_AtTheta0847(t *testing.T) {
	d := zpdDifficultyFromTheta(ZPDOffset)
	if math.Abs(d-0.50) > 0.005 {
		t.Errorf("at θ=ZPDOffset (%.3f), expected DifficultyTarget=0.50 (b=0, sigmoid(0)=0.5), got %f",
			ZPDOffset, d)
	}
}

func TestZPDDifficulty_ClampsLow(t *testing.T) {
	// θ=-4 → b=-4.847 → sigmoid≈0.0078 → clamp to 0.30
	d := zpdDifficultyFromTheta(-4)
	if d != 0.30 {
		t.Errorf("expected 0.30 (low clamp) at θ=-4, got %f", d)
	}
}

func TestZPDDifficulty_ClampsHigh(t *testing.T) {
	// θ=4 → b=3.153 → sigmoid≈0.959 → clamp to 0.85
	d := zpdDifficultyFromTheta(4)
	if d != 0.85 {
		t.Errorf("expected 0.85 (high clamp) at θ=4, got %f", d)
	}
}

func TestZPDDifficulty_BoundaryAtZeroTheta(t *testing.T) {
	// θ=0 → b=-0.847 → sigmoid(-0.847)≈0.300 (right at low clamp boundary)
	d := zpdDifficultyFromTheta(0)
	if math.Abs(d-0.30) > 0.005 {
		t.Errorf("at θ=0 expected ~0.30, got %f", d)
	}
}

func TestZPDDifficulty_NaNHandlingDoesNotPropagate(t *testing.T) {
	d := zpdDifficultyFromTheta(math.NaN())
	if math.IsNaN(d) {
		t.Errorf("NaN theta produced NaN difficulty (must be benign middle), got NaN")
	}
	if d < 0.30 || d > 0.85 {
		t.Errorf("NaN fallback difficulty out of envelope: %f", d)
	}
}

func TestZPDDifficulty_MonotonicInTheta(t *testing.T) {
	// Sanity: difficulty must be non-decreasing in theta within the
	// unclamped range, since sigmoid is monotone.
	thetas := []float64{-1, -0.5, 0, 0.5, 1, 1.5, 2}
	prev := zpdDifficultyFromTheta(thetas[0])
	for _, th := range thetas[1:] {
		d := zpdDifficultyFromTheta(th)
		if d < prev {
			t.Errorf("non-monotone: θ=%.2f → %f < previous %f", th, d, prev)
		}
		prev = d
	}
}
