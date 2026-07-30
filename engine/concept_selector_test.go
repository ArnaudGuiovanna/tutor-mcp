// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"errors"
	"math"
	"testing"
	"time"

	"tutor-mcp/models"
)

// reviewedCS builds a ConceptState past its first review with high
// retention by default (so the FSRS branch doesn't accidentally drive
// MAINTENANCE tests).
func reviewedCS(concept string, mastery float64) *models.ConceptState {
	cs := models.NewConceptState("L1", concept)
	cs.PMastery = mastery
	cs.CardState = "review"
	cs.Stability = 30
	cs.ElapsedDays = 1
	lastReview := time.Now().UTC().Add(-24 * time.Hour)
	cs.LastReview = &lastReview
	return cs
}

func setConceptCardAge(cs *models.ConceptState, days int) {
	cs.ElapsedDays = days
	lastReview := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	cs.LastReview = &lastReview
}

func graphLinear(concepts ...string) models.KnowledgeSpace {
	prereqs := map[string][]string{}
	for i := 1; i < len(concepts); i++ {
		prereqs[concepts[i]] = []string{concepts[i-1]}
	}
	return models.KnowledgeSpace{Concepts: concepts, Prerequisites: prereqs}
}

func graphFlat(concepts ...string) models.KnowledgeSpace {
	return models.KnowledgeSpace{Concepts: concepts, Prerequisites: map[string][]string{}}
}

// ─── Phase invalid (OQ-4.1 explicit error) ─────────────────────────────────

func TestSelectConcept_UnknownPhase_ReturnsExplicitError(t *testing.T) {
	_, err := SelectConcept(models.Phase("BOGUS"), nil, models.KnowledgeSpace{}, nil)
	if err == nil {
		t.Fatalf("expected error for unknown phase, got nil")
	}
	if !errors.Is(err, ErrUnknownPhase) {
		t.Errorf("expected errors.Is(err, ErrUnknownPhase), got %v", err)
	}
}

func TestSelectConcept_EmptyPhaseString_ReturnsExplicitError(t *testing.T) {
	_, err := SelectConcept(models.Phase(""), nil, models.KnowledgeSpace{}, nil)
	if !errors.Is(err, ErrUnknownPhase) {
		t.Errorf("expected ErrUnknownPhase for empty phase string, got %v", err)
	}
}

// ─── INSTRUCTION ───────────────────────────────────────────────────────────

func TestSelectConcept_Instruction_PicksHighestScore(t *testing.T) {
	graph := graphFlat("A", "B", "C")
	states := []*models.ConceptState{
		reviewedCS("A", 0.10), // (1-m)=0.90
		reviewedCS("B", 0.50), // (1-m)=0.50
		reviewedCS("C", 0.20), // (1-m)=0.80
	}
	gr := map[string]float64{"A": 0.5, "B": 0.9, "C": 1.0}
	// Scores: A=0.45, B=0.45, C=0.80 → C wins.
	sel, err := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Concept != "C" {
		t.Errorf("expected C, got %s (score=%.3f)", sel.Concept, sel.Score)
	}
	if sel.Phase != models.PhaseInstruction {
		t.Errorf("phase echo wrong: %s", sel.Phase)
	}
}

func TestSelectConcept_Instruction_GoalRelevanceDominatesAtEqualMastery(t *testing.T) {
	graph := graphFlat("A", "B")
	states := []*models.ConceptState{reviewedCS("A", 0.3), reviewedCS("B", 0.3)}
	gr := map[string]float64{"A": 0.2, "B": 0.9}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "B" {
		t.Errorf("expected B (higher relevance), got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_LowMasteryWinsAtEqualRelevance(t *testing.T) {
	graph := graphFlat("A", "B")
	states := []*models.ConceptState{reviewedCS("A", 0.10), reviewedCS("B", 0.60)}
	gr := map[string]float64{"A": 0.7, "B": 0.7}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "A" {
		t.Errorf("expected A (lower mastery), got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_PrereqsBlockExclusion(t *testing.T) {
	graph := graphLinear("Basics", "Advanced")
	states := []*models.ConceptState{
		reviewedCS("Basics", 0.30),   // below KST threshold
		reviewedCS("Advanced", 0.10), // would be eligible if prereq met
	}
	gr := map[string]float64{"Basics": 1.0, "Advanced": 1.0}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "Basics" {
		t.Errorf("expected Basics (only one with no prereqs), got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_MasteredExcludedFromFringe(t *testing.T) {
	graph := graphFlat("A", "B")
	states := []*models.ConceptState{
		reviewedCS("A", 0.95), // already mastered
		reviewedCS("B", 0.10),
	}
	gr := map[string]float64{"A": 1.0, "B": 0.5}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "B" {
		t.Errorf("expected B (A excluded), got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_NoFringe_AllMastered(t *testing.T) {
	graph := graphFlat("A", "B")
	states := []*models.ConceptState{reviewedCS("A", 0.95), reviewedCS("B", 0.95)}
	gr := map[string]float64{"A": 1.0, "B": 1.0}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if !sel.NoFringe {
		t.Errorf("expected NoFringe when all mastered, got %+v", sel)
	}
}

func TestSelectConcept_Instruction_NoFringe_NoPrereqsSatisfied(t *testing.T) {
	graph := graphLinear("A", "B", "C")
	states := []*models.ConceptState{
		reviewedCS("A", 0.20), // can't unlock B, B can't unlock C
	}
	gr := map[string]float64{"A": 1.0, "B": 1.0, "C": 1.0}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "A" {
		t.Errorf("expected A (only no-prereq concept eligible), got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_NilGoalRelevance_UniformFallback(t *testing.T) {
	graph := graphFlat("A", "B", "C")
	states := []*models.ConceptState{
		reviewedCS("A", 0.10),
		reviewedCS("B", 0.50),
		reviewedCS("C", 0.20),
	}
	// nil → uniform 1.0 → score = 1 - mastery; A wins with 0.90.
	sel, err := SelectConcept(models.PhaseInstruction, states, graph, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Concept != "A" {
		t.Errorf("expected A under uniform fallback, got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_ConceptNotInStatesIsEligible(t *testing.T) {
	// "Strings" is in the graph but never been touched (no state).
	// Treated as mastery=0; eligible if prereqs OK.
	graph := graphFlat("Strings")
	states := []*models.ConceptState{}
	gr := map[string]float64{"Strings": 1.0}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "Strings" {
		t.Errorf("expected Strings (mastery=0 default), got %+v", sel)
	}
}

// ─── OQ-4.3 = B' : exclusion sur concept absent du vecteur ─────────────────

func TestSelectConcept_Instruction_UncoveredConceptExcluded(t *testing.T) {
	graph := graphFlat("A", "B")
	states := []*models.ConceptState{reviewedCS("A", 0.10), reviewedCS("B", 0.10)}
	// Vector covers A only; B is uncovered → must be excluded.
	gr := map[string]float64{"A": 0.3}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "A" {
		t.Errorf("OQ-4.3=B' violated: B should be excluded as uncovered, got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_AllUncovered_NoFringe(t *testing.T) {
	graph := graphFlat("A", "B")
	states := []*models.ConceptState{reviewedCS("A", 0.10), reviewedCS("B", 0.10)}
	gr := map[string]float64{"OtherConcept": 1.0} // none of A, B covered
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if !sel.NoFringe {
		t.Errorf("expected NoFringe when all candidates uncovered, got %+v", sel)
	}
}

// ─── OQ-4.4 : alphabetical tie-break (frozen contract) ────────────────────

func TestSelectConcept_Instruction_TieBreakAlphabetical(t *testing.T) {
	// alpha, beta, gamma — same mastery, same goal_relevance → same
	// score → alphabetical wins → alpha.
	graph := graphFlat("alpha", "beta", "gamma")
	states := []*models.ConceptState{
		reviewedCS("alpha", 0.30),
		reviewedCS("beta", 0.30),
		reviewedCS("gamma", 0.30),
	}
	gr := map[string]float64{"alpha": 0.5, "beta": 0.5, "gamma": 0.5}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "alpha" {
		t.Errorf("tie-break contract violated: expected alpha, got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_TieBreakAlphabetical_NonAlphaInsertOrder(t *testing.T) {
	// Same as above but graph and states declared in non-alpha order
	// to confirm sort happens inside the selector.
	graph := graphFlat("gamma", "alpha", "beta")
	states := []*models.ConceptState{
		reviewedCS("gamma", 0.30),
		reviewedCS("alpha", 0.30),
		reviewedCS("beta", 0.30),
	}
	gr := map[string]float64{"alpha": 0.5, "beta": 0.5, "gamma": 0.5}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "alpha" {
		t.Errorf("expected alpha regardless of insert order, got %s", sel.Concept)
	}
}

// ─── OQ-4.6 : three explicit degenerate cases of the INSTRUCTION formula ────

func TestSelectConcept_Instruction_DegenerateGoalRelevanceNearZero(t *testing.T) {
	// Concept with near-zero relevance never wins unless it is the only candidate.
	graph := graphFlat("Low", "High")
	states := []*models.ConceptState{reviewedCS("Low", 0.10), reviewedCS("High", 0.10)}
	gr := map[string]float64{"Low": 0.001, "High": 1.0}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "High" {
		t.Errorf("expected High to dominate Low at gr=0.001, got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_DegenerateMasteryNearBKT(t *testing.T) {
	// Concept with mastery just below the threshold → score = gr × ~0.15 (low).
	// A less-mastered concept at equal relevance wins.
	graph := graphFlat("Almost", "Fresh")
	states := []*models.ConceptState{
		reviewedCS("Almost", 0.84), // very close to BKT threshold
		reviewedCS("Fresh", 0.10),  // (1-m)=0.90
	}
	gr := map[string]float64{"Almost": 1.0, "Fresh": 1.0}
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "Fresh" {
		t.Errorf("expected Fresh (less mastered, equal relevance), got %s", sel.Concept)
	}
}

func TestSelectConcept_Instruction_NormalFringe_4Candidates(t *testing.T) {
	// Frange normale 4 candidats avec spread sur les deux axes.
	graph := graphFlat("A", "B", "C", "D")
	states := []*models.ConceptState{
		reviewedCS("A", 0.20),
		reviewedCS("B", 0.50),
		reviewedCS("C", 0.70),
		reviewedCS("D", 0.10),
	}
	gr := map[string]float64{"A": 0.7, "B": 1.0, "C": 0.3, "D": 0.5}
	// Scores: A=0.56, B=0.50, C=0.09, D=0.45 → A wins.
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "A" {
		t.Errorf("expected A (highest argmax), got %s (score=%.3f)", sel.Concept, sel.Score)
	}
}

// ─── MAINTENANCE ───────────────────────────────────────────────────────────

func TestSelectConcept_Maintenance_PicksLowestRetention(t *testing.T) {
	csA := reviewedCS("A", 0.95)
	csA.Stability = 30
	setConceptCardAge(csA, 1) // high retention
	csB := reviewedCS("B", 0.95)
	csB.Stability = 1
	setConceptCardAge(csB, 30) // low retention
	gr := map[string]float64{"A": 1.0, "B": 1.0}
	sel, _ := SelectConcept(models.PhaseMaintenance, []*models.ConceptState{csA, csB},
		models.KnowledgeSpace{}, gr)
	if sel.Concept != "B" {
		t.Errorf("expected B (lowest retention → highest urgency), got %s", sel.Concept)
	}
}

func TestSelectConceptAt_MaintenanceUsesCurrentAgeNotHistoricalElapsedDays(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	recentReview := now.Add(-2 * time.Hour)
	oldReview := now.Add(-30 * 24 * time.Hour)

	recent := reviewedCS("recent", 0.95)
	recent.Stability = 1
	recent.ElapsedDays = 90
	recent.LastReview = &recentReview

	stale := reviewedCS("stale", 0.95)
	stale.Stability = 1
	stale.ElapsedDays = 1
	stale.LastReview = &oldReview

	sel, err := SelectConceptAt(
		models.PhaseMaintenance,
		[]*models.ConceptState{recent, stale},
		graphFlat("recent", "stale"),
		map[string]float64{"recent": 1, "stale": 1},
		now,
	)
	if err != nil {
		t.Fatalf("SelectConceptAt: %v", err)
	}
	if sel.Concept != "stale" {
		t.Fatalf("selected %q, want stale; historical ElapsedDays must not invert FSRS urgency", sel.Concept)
	}
}

func TestSelectConcept_Maintenance_GoalRelevanceWeights(t *testing.T) {
	csA := reviewedCS("A", 0.95)
	csA.Stability = 1
	setConceptCardAge(csA, 30) // high urgency
	csB := reviewedCS("B", 0.95)
	csB.Stability = 1
	setConceptCardAge(csB, 30) // same urgency
	// Higher relevance on B should tip.
	gr := map[string]float64{"A": 0.1, "B": 0.9}
	sel, _ := SelectConcept(models.PhaseMaintenance, []*models.ConceptState{csA, csB},
		models.KnowledgeSpace{}, gr)
	if sel.Concept != "B" {
		t.Errorf("expected B (higher relevance breaks urgency tie), got %s", sel.Concept)
	}
}

func TestSelectConcept_Maintenance_NoFringe_NothingMastered(t *testing.T) {
	states := []*models.ConceptState{reviewedCS("A", 0.50), reviewedCS("B", 0.30)}
	sel, _ := SelectConcept(models.PhaseMaintenance, states, models.KnowledgeSpace{}, nil)
	if !sel.NoFringe {
		t.Errorf("expected NoFringe when nothing mastered, got %+v", sel)
	}
}

func TestSelectConcept_Maintenance_NewCardSkippedAsZeroUrgency(t *testing.T) {
	csA := reviewedCS("A", 0.95)
	csA.CardState = "new" // urgency=0
	csB := reviewedCS("B", 0.95)
	csB.Stability = 5
	setConceptCardAge(csB, 5) // urgency > 0
	gr := map[string]float64{"A": 1.0, "B": 1.0}
	sel, _ := SelectConcept(models.PhaseMaintenance, []*models.ConceptState{csA, csB},
		models.KnowledgeSpace{}, gr)
	if sel.Concept != "B" {
		t.Errorf("expected B (A has zero urgency on new card), got %s", sel.Concept)
	}
}

func TestSelectConcept_Maintenance_TieBreakAlphabetical(t *testing.T) {
	csB := reviewedCS("beta", 0.95)
	csB.Stability = 1
	setConceptCardAge(csB, 30)
	csA := reviewedCS("alpha", 0.95)
	csA.Stability = 1
	setConceptCardAge(csA, 30)
	csC := reviewedCS("gamma", 0.95)
	csC.Stability = 1
	setConceptCardAge(csC, 30)
	gr := map[string]float64{"alpha": 0.5, "beta": 0.5, "gamma": 0.5}
	sel, _ := SelectConcept(models.PhaseMaintenance,
		[]*models.ConceptState{csB, csA, csC}, models.KnowledgeSpace{}, gr)
	if sel.Concept != "alpha" {
		t.Errorf("MAINTENANCE tie-break violated: expected alpha, got %s", sel.Concept)
	}
}

func TestSelectConcept_Maintenance_FiltersByActiveDomainGraph(t *testing.T) {
	// Regression: selectMaintenance must restrict its mastered pool to
	// states whose concept is in domain.Graph.Concepts. pf.StatesList is
	// learner-wide (GetConceptStatesByLearner is not domain-scoped), so
	// without this filter a concept mastered in another domain leaks
	// into the active domain's MAINTENANCE selection — particularly
	// when goal_relevance is nil and urgency × 1.0 alone drives
	// selection. Issue #93.
	//
	// Setup: active domain D2 has only "a"; "x" lives in D1 but is in
	// the learner-wide states. Both are mastered (PMastery=0.95). "x"
	// has higher urgency (Stability=1, ElapsedDays=30) so without the
	// filter it would win over "a" (Stability=30, ElapsedDays=1).
	csA := reviewedCS("a", 0.95)
	csA.Stability = 30
	setConceptCardAge(csA, 1)
	csX := reviewedCS("x", 0.95)
	csX.Stability = 1
	setConceptCardAge(csX, 30)

	activeGraph := graphFlat("a")

	sel, err := SelectConcept(
		models.PhaseMaintenance,
		[]*models.ConceptState{csA, csX},
		activeGraph,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Concept == "x" {
		t.Fatalf("cross-domain leak: MAINTENANCE selected %q (not in graph), expected %q", sel.Concept, "a")
	}
	if sel.Concept != "a" {
		t.Errorf("expected concept=a, got %q", sel.Concept)
	}
}

func TestSelectConcept_Maintenance_AllMasteredFromOtherDomain_NoFringe(t *testing.T) {
	// Companion regression: when every mastered state belongs to
	// another domain, MAINTENANCE must return NoFringe rather than
	// silently returning a foreign concept. Symmetric to the
	// DIAGNOSTIC case above.
	csY := reviewedCS("y", 0.95)
	csY.Stability = 1
	setConceptCardAge(csY, 30)
	csZ := reviewedCS("z", 0.95)
	csZ.Stability = 1
	setConceptCardAge(csZ, 30)

	activeGraph := graphFlat("a")

	sel, err := SelectConcept(
		models.PhaseMaintenance,
		[]*models.ConceptState{csY, csZ},
		activeGraph,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sel.NoFringe {
		t.Errorf("expected NoFringe when all mastered are from other domains, got %+v", sel)
	}
	if sel.Concept != "" {
		t.Errorf("expected empty concept on NoFringe, got %q", sel.Concept)
	}
}

// ─── DIAGNOSTIC ────────────────────────────────────────────────────────────

func TestSelectConcept_Diagnostic_PicksMaxInfoGain(t *testing.T) {
	// All same noise; varying P(L). P(L)=0.5 has the highest info-gain.
	csA := reviewedCS("A", 0.5)
	csA.PSlip = 0.1
	csA.PGuess = 0.2
	csB := reviewedCS("B", 0.2)
	csB.PSlip = 0.1
	csB.PGuess = 0.2
	csC := reviewedCS("C", 0.8)
	csC.PSlip = 0.1
	csC.PGuess = 0.2
	sel, _ := SelectConcept(models.PhaseDiagnostic,
		[]*models.ConceptState{csA, csB, csC}, models.KnowledgeSpace{}, nil)
	if sel.Concept != "A" {
		t.Errorf("expected A (PL=0.5 max info-gain), got %s", sel.Concept)
	}
}

func TestSelectConcept_Diagnostic_NoFringe_AllSaturated(t *testing.T) {
	states := []*models.ConceptState{
		reviewedCS("A", 0.01),
		reviewedCS("B", 0.99),
		reviewedCS("C", 0.0),
	}
	sel, _ := SelectConcept(models.PhaseDiagnostic, states, models.KnowledgeSpace{}, nil)
	if !sel.NoFringe {
		t.Errorf("expected NoFringe when all saturated, got %+v", sel)
	}
}

func TestSelectConcept_Diagnostic_IgnoresGoalRelevance(t *testing.T) {
	// Confirms OQ-4.2 sub A1 v1 contract: diagnostic does NOT weight
	// by goal_relevance, even when the vector strongly biases.
	csA := reviewedCS("A", 0.5) // max info-gain
	csA.PSlip = 0.1
	csA.PGuess = 0.2
	csB := reviewedCS("B", 0.2)
	csB.PSlip = 0.1
	csB.PGuess = 0.2
	// Vector strongly biased toward B; A is irrelevant per the goal.
	gr := map[string]float64{"A": 0.001, "B": 1.0}
	sel, _ := SelectConcept(models.PhaseDiagnostic,
		[]*models.ConceptState{csA, csB}, models.KnowledgeSpace{}, gr)
	if sel.Concept != "A" {
		t.Errorf("v1 contract violated: DIAGNOSTIC must ignore goal_relevance; expected A, got %s", sel.Concept)
	}
}

func TestSelectConcept_Diagnostic_FiltersByActiveDomainGraph(t *testing.T) {
	// Regression: selectDiagnostic must restrict its candidate pool to
	// states whose concept is in domain.Graph.Concepts. pf.StatesList is
	// learner-wide (GetConceptStatesByLearner is not domain-scoped), so
	// without this filter a concept mastered in another domain (or simply
	// in another active domain's `concept_states`) leaks into the active
	// domain's DIAGNOSTIC selection. Mirrors the MAINTENANCE filter
	// applied via issue #93 / PR #102.
	//
	// Setup: active domain D2 has only "a"; "x" lives in D1 but is in
	// the learner-wide states. "x" is at PMastery=0.5 (peak BKT
	// info-gain), "a" at PMastery=0.2 (lower info-gain). Pre-fix the
	// selector picks "x" because info-gain wins; post-fix "x" is
	// excluded by the domain set and "a" is selected.
	csA := reviewedCS("a", 0.2)
	csA.PSlip = 0.1
	csA.PGuess = 0.2
	csX := reviewedCS("x", 0.5)
	csX.PSlip = 0.1
	csX.PGuess = 0.2

	activeGraph := graphFlat("a")

	sel, err := SelectConcept(
		models.PhaseDiagnostic,
		[]*models.ConceptState{csA, csX},
		activeGraph,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Concept == "x" {
		t.Fatalf("cross-domain leak: DIAGNOSTIC selected %q (not in graph), expected %q", sel.Concept, "a")
	}
	if sel.Concept != "a" {
		t.Errorf("expected concept=a, got %q", sel.Concept)
	}
}

func TestSelectConcept_Diagnostic_AllCandidatesFromOtherDomain_NoFringe(t *testing.T) {
	// Companion regression: when every non-saturated state belongs to
	// another domain, DIAGNOSTIC must return NoFringe rather than
	// silently returning a foreign concept. This ensures the orchestrator
	// can react (e.g., transition or surface a needs-setup signal)
	// instead of routing to a concept that the writer side will reject
	// at validateConceptInDomain.
	csY := reviewedCS("y", 0.4)
	csY.PSlip = 0.1
	csY.PGuess = 0.2
	csZ := reviewedCS("z", 0.6)
	csZ.PSlip = 0.1
	csZ.PGuess = 0.2

	activeGraph := graphFlat("a")

	sel, err := SelectConcept(
		models.PhaseDiagnostic,
		[]*models.ConceptState{csY, csZ},
		activeGraph,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sel.NoFringe {
		t.Errorf("expected NoFringe when all candidates are from other domains, got %+v", sel)
	}
	if sel.Concept != "" {
		t.Errorf("expected empty concept on NoFringe, got %q", sel.Concept)
	}
}

// ─── Cross-cutting degenerate cases ────────────────────────────────────────

func TestSelectConcept_NaN_PMastery_ExcludedFromAllPhases(t *testing.T) {
	csNaN := reviewedCS("Bad", math.NaN())
	csOK := reviewedCS("Good", 0.5)
	csOK.PSlip = 0.1
	csOK.PGuess = 0.2

	graph := graphFlat("Bad", "Good")
	gr := map[string]float64{"Bad": 1.0, "Good": 0.5}

	for _, phase := range []models.Phase{
		models.PhaseInstruction,
		models.PhaseDiagnostic,
		models.PhaseMaintenance,
	} {
		t.Run(string(phase), func(t *testing.T) {
			sel, err := SelectConcept(phase, []*models.ConceptState{csNaN, csOK}, graph, gr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sel.Concept == "Bad" {
				t.Errorf("NaN concept must never be selected, got %s in phase %s",
					sel.Concept, phase)
			}
		})
	}
}

func TestSelectConcept_EmptyStatesEmptyGraph(t *testing.T) {
	for _, phase := range []models.Phase{
		models.PhaseInstruction,
		models.PhaseDiagnostic,
		models.PhaseMaintenance,
	} {
		sel, err := SelectConcept(phase, nil, models.KnowledgeSpace{}, nil)
		if err != nil {
			t.Fatalf("phase %s unexpected error: %v", phase, err)
		}
		if !sel.NoFringe {
			t.Errorf("phase %s: expected NoFringe on empty inputs, got %+v", phase, sel)
		}
	}
}

// ─── MasteryBKT accessor (no literal threshold in code) ────────────────────

func TestSelectConcept_RespectsMasteryBKTAccessor(t *testing.T) {
	t.Setenv("REGULATION_THRESHOLD", "off")
	graph := graphFlat("A")
	states := []*models.ConceptState{reviewedCS("A", 0.84)}
	gr := map[string]float64{"A": 1.0}
	// Under both legacy and unified, MasteryBKT()=0.85, so 0.84 stays
	// in fringe.
	sel, _ := SelectConcept(models.PhaseInstruction, states, graph, gr)
	if sel.Concept != "A" {
		t.Errorf("expected A in fringe at 0.84 under legacy, got %+v", sel)
	}
}
