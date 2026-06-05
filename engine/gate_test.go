// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"errors"
	"math"
	"testing"

	"tutor-mcp/models"
)

// gateCS builds a ConceptState with the given mastery, suitable for
// prereq lookups (CardState=review so it's a "real" state).
func gateCS(concept string, mastery float64) *models.ConceptState {
	cs := models.NewConceptState("L1", concept)
	cs.PMastery = mastery
	cs.CardState = "review"
	return cs
}

// statesMap turns a list of states into the lookup map ApplyGate expects.
func statesMap(css ...*models.ConceptState) map[string]*models.ConceptState {
	m := map[string]*models.ConceptState{}
	for _, cs := range css {
		m[cs.Concept] = cs
	}
	return m
}

func graph(concepts []string, prereqs map[string][]string) models.KnowledgeSpace {
	if prereqs == nil {
		prereqs = map[string][]string{}
	}
	return models.KnowledgeSpace{Concepts: concepts, Prerequisites: prereqs}
}

// containsAll asserts every name in want appears in got.
func containsAll(t *testing.T, got, want []string) {
	t.Helper()
	set := map[string]bool{}
	for _, c := range got {
		set[c] = true
	}
	for _, c := range want {
		if !set[c] {
			t.Errorf("expected %q in pool, got %v", c, got)
		}
	}
}

// containsNone asserts no name in unwanted appears in got.
func containsNone(t *testing.T, got, unwanted []string) {
	t.Helper()
	set := map[string]bool{}
	for _, c := range got {
		set[c] = true
	}
	for _, c := range unwanted {
		if set[c] {
			t.Errorf("did not expect %q in pool, got %v", c, got)
		}
	}
}

// ─── OQ-3.1 : multi-mode invariant garde-fou ───────────────────────────────

func TestGateResult_Validate_RejectsMultiMode_EscapeAndNoCandidate(t *testing.T) {
	g := GateResult{
		EscapeAction: &EscapeAction{Type: models.ActivityCloseSession},
		NoCandidate:  true,
	}
	err := g.Validate()
	if err == nil {
		t.Fatalf("expected error for Escape+NoCandidate, got nil")
	}
	if !errors.Is(err, ErrInvalidGateResult) {
		t.Errorf("expected errors.Is(ErrInvalidGateResult), got %v", err)
	}
}

func TestGateResult_Validate_RejectsMultiMode_AllowAndNoCandidate(t *testing.T) {
	g := GateResult{
		AllowedConcepts: []string{"A"},
		NoCandidate:     true,
	}
	if err := g.Validate(); !errors.Is(err, ErrInvalidGateResult) {
		t.Errorf("expected ErrInvalidGateResult, got %v", err)
	}
}

func TestGateResult_Validate_RejectsMultiMode_EscapeAndAllow(t *testing.T) {
	g := GateResult{
		EscapeAction:    &EscapeAction{Type: models.ActivityCloseSession},
		AllowedConcepts: []string{"A"},
	}
	if err := g.Validate(); !errors.Is(err, ErrInvalidGateResult) {
		t.Errorf("expected ErrInvalidGateResult, got %v", err)
	}
}

func TestGateResult_Validate_RejectsMultiMode_AllThree(t *testing.T) {
	g := GateResult{
		EscapeAction:    &EscapeAction{Type: models.ActivityCloseSession},
		NoCandidate:     true,
		AllowedConcepts: []string{"A"},
	}
	if err := g.Validate(); !errors.Is(err, ErrInvalidGateResult) {
		t.Errorf("expected ErrInvalidGateResult, got %v", err)
	}
}

func TestGateResult_Validate_RejectsZeroMode(t *testing.T) {
	g := GateResult{}
	if err := g.Validate(); !errors.Is(err, ErrInvalidGateResult) {
		t.Errorf("expected ErrInvalidGateResult on zero modes, got %v", err)
	}
}

func TestGateResult_Validate_AcceptsEachValidMode(t *testing.T) {
	cases := []struct {
		name string
		g    GateResult
	}{
		{"escape", newEscapeResult(EscapeAction{Type: models.ActivityCloseSession}, "ok")},
		{"no-candidate", newNoCandidateResult("ok")},
		{"allow", newAllowResult([]string{"A"}, nil, "ok")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.g.Validate(); err != nil {
				t.Errorf("expected valid result %s, got %v", c.name, err)
			}
		})
	}
}

// TestApplyGate_AllOutputsPassValidate is a meta-test: every well-formed
// invocation of ApplyGate must produce a GateResult that passes
// Validate. This pins the constructor-only invariant against future
// refactors.
func TestApplyGate_AllOutputsPassValidate(t *testing.T) {
	cases := []GateInput{
		{Phase: models.PhaseInstruction},
		{Phase: models.PhaseInstruction, Alerts: []models.Alert{{Type: models.AlertOverload}}},
		{
			Phase:    models.PhaseInstruction,
			Concepts: []string{"A", "B"},
			States:   statesMap(gateCS("A", 0.5), gateCS("B", 0.5)),
			Graph:    graph([]string{"A", "B"}, nil),
		},
		{
			Phase:                models.PhaseInstruction,
			Concepts:             []string{"A"},
			States:               statesMap(gateCS("A", 0.5)),
			Graph:                graph([]string{"A"}, nil),
			ActiveMisconceptions: map[string]bool{"A": true},
		},
		{
			Phase:            models.PhaseInstruction,
			Concepts:         []string{"A"},
			States:           statesMap(gateCS("A", 0.5)),
			Graph:            graph([]string{"A"}, nil),
			RecentConcepts:   []string{"A"},
			AntiRepeatWindow: 1,
		},
	}
	for i, in := range cases {
		got, err := ApplyGate(in)
		if err != nil {
			t.Errorf("case %d: unexpected error %v", i, err)
			continue
		}
		if vErr := got.Validate(); vErr != nil {
			t.Errorf("case %d: ApplyGate produced invalid result: %v", i, vErr)
		}
	}
}

// ─── Rule 3 : OVERLOAD escape (priority 1) ─────────────────────────────────

func TestApplyGate_OverloadEscape_TakesPrecedence(t *testing.T) {
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: []string{"A"},
		States:   statesMap(gateCS("A", 0.5)),
		Graph:    graph([]string{"A"}, nil),
		Alerts:   []models.Alert{{Type: models.AlertOverload}},
	}
	r, err := ApplyGate(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.EscapeAction == nil {
		t.Fatalf("expected escape action, got %+v", r)
	}
	if r.EscapeAction.Type != models.ActivityCloseSession {
		t.Errorf("expected CLOSE_SESSION, got %s", r.EscapeAction.Type)
	}
}

func TestApplyGate_OverloadEscape_OverridesMisconception(t *testing.T) {
	in := GateInput{
		Phase:                models.PhaseInstruction,
		Concepts:             []string{"A"},
		States:               statesMap(gateCS("A", 0.5)),
		Graph:                graph([]string{"A"}, nil),
		ActiveMisconceptions: map[string]bool{"A": true},
		Alerts:               []models.Alert{{Type: models.AlertOverload}},
	}
	r, _ := ApplyGate(in)
	if r.EscapeAction == nil {
		t.Fatal("OQ-3.5 violated: OVERLOAD must override misconception")
	}
}

func TestApplyGate_OverloadEscape_OverridesForgetting(t *testing.T) {
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: []string{"A"},
		States:   statesMap(gateCS("A", 0.5)),
		Graph:    graph([]string{"A"}, nil),
		Alerts: []models.Alert{
			{Type: models.AlertOverload},
			{Type: models.AlertForgetting, Concept: "A"},
		},
	}
	r, _ := ApplyGate(in)
	if r.EscapeAction == nil {
		t.Fatal("OQ-3.5 violated: OVERLOAD must override FORGETTING")
	}
}

// ─── Rule 2 : prereq filter ────────────────────────────────────────────────

func TestApplyGate_PrereqFilter_Excludes(t *testing.T) {
	// Advanced needs Basics; Basics not mastered → Advanced excluded.
	g := graph([]string{"Basics", "Advanced"}, map[string][]string{
		"Advanced": {"Basics"},
	})
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: g.Concepts,
		States: statesMap(
			gateCS("Basics", 0.30),
			gateCS("Advanced", 0.10),
		),
		Graph: g,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"Basics"})
	containsNone(t, r.AllowedConcepts, []string{"Advanced"})
}

func TestApplyGate_PrereqFilter_BypassedInDiagnostic(t *testing.T) {
	g := graph([]string{"Basics", "Advanced"}, map[string][]string{
		"Advanced": {"Basics"},
	})
	in := GateInput{
		Phase:    models.PhaseDiagnostic,
		Concepts: g.Concepts,
		States: statesMap(
			gateCS("Basics", 0.30),
			gateCS("Advanced", 0.50),
		),
		Graph: g,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"Basics", "Advanced"})
}

// ─── Rule 4 : anti-rep ─────────────────────────────────────────────────────

func TestApplyGate_AntiRepeat_ExcludesRecent(t *testing.T) {
	in := GateInput{
		Phase:            models.PhaseInstruction,
		Concepts:         []string{"A", "B", "C", "D"},
		States:           statesMap(gateCS("A", 0.3), gateCS("B", 0.3), gateCS("C", 0.3), gateCS("D", 0.3)),
		Graph:            graph([]string{"A", "B", "C", "D"}, nil),
		RecentConcepts:   []string{"A", "B"},
		AntiRepeatWindow: 2,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"C", "D"})
	containsNone(t, r.AllowedConcepts, []string{"A", "B"})
}

func TestApplyGate_AntiRepeat_BypassedByForgetting(t *testing.T) {
	in := GateInput{
		Phase:            models.PhaseInstruction,
		Concepts:         []string{"A", "B", "C"},
		States:           statesMap(gateCS("A", 0.3), gateCS("B", 0.3), gateCS("C", 0.3)),
		Graph:            graph([]string{"A", "B", "C"}, nil),
		RecentConcepts:   []string{"A"},
		AntiRepeatWindow: 1,
		Alerts:           []models.Alert{{Type: models.AlertForgetting, Concept: "A"}},
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"A", "B", "C"})
}

func TestApplyGate_AntiRepeat_BypassedByMisconception(t *testing.T) {
	// OQ-3.5 sub-a : misconception bypasse anti-rep.
	in := GateInput{
		Phase:                models.PhaseInstruction,
		Concepts:             []string{"A", "B", "C"},
		States:               statesMap(gateCS("A", 0.3), gateCS("B", 0.3), gateCS("C", 0.3)),
		Graph:                graph([]string{"A", "B", "C"}, nil),
		ActiveMisconceptions: map[string]bool{"A": true},
		RecentConcepts:       []string{"A"},
		AntiRepeatWindow:     1,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"A", "B", "C"})
	if got, ok := r.ActionRestriction["A"]; !ok || len(got) != 1 || got[0] != models.ActivityDebugMisconception {
		t.Errorf("expected DEBUG_MISCONCEPTION restriction on A, got %+v", r.ActionRestriction)
	}
}

func TestApplyGate_AntiRepeat_RespectsWindowSize(t *testing.T) {
	// Only top-N from RecentConcepts is excluded.
	in := GateInput{
		Phase:            models.PhaseInstruction,
		Concepts:         []string{"A", "B", "C", "D"},
		States:           statesMap(gateCS("A", 0.3), gateCS("B", 0.3), gateCS("C", 0.3), gateCS("D", 0.3)),
		Graph:            graph([]string{"A", "B", "C", "D"}, nil),
		RecentConcepts:   []string{"A", "B", "C"}, // C is "older" than B which is older than A
		AntiRepeatWindow: 2,                       // exclude A, B only
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"C", "D"})
	containsNone(t, r.AllowedConcepts, []string{"A", "B"})
}

func TestApplyGate_AntiRepeat_WindowZeroDisabled(t *testing.T) {
	// OQ-3.4: N=0 disables anti-rep.
	in := GateInput{
		Phase:            models.PhaseInstruction,
		Concepts:         []string{"A", "B"},
		States:           statesMap(gateCS("A", 0.3), gateCS("B", 0.3)),
		Graph:            graph([]string{"A", "B"}, nil),
		RecentConcepts:   []string{"A", "B"},
		AntiRepeatWindow: 0,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"A", "B"})
}

func TestApplyGate_AntiRepeat_EffectiveWindowProtection(t *testing.T) {
	// OQ-3.4 garde-fou: small domain (2 concepts) with N=3.
	// effective_N capped at len(Concepts)-1 = 1, so only 1 concept
	// excluded — at least 1 always stays.
	in := GateInput{
		Phase:            models.PhaseInstruction,
		Concepts:         []string{"A", "B"},
		States:           statesMap(gateCS("A", 0.3), gateCS("B", 0.3)),
		Graph:            graph([]string{"A", "B"}, nil),
		RecentConcepts:   []string{"A", "B"},
		AntiRepeatWindow: 3,
	}
	r, _ := ApplyGate(in)
	if r.NoCandidate {
		t.Fatalf("garde-fou violated: small domain produced NoCandidate, got %+v", r)
	}
	if len(r.AllowedConcepts) == 0 {
		t.Fatalf("expected at least 1 candidate after effective_N protection, got 0")
	}
}

func TestApplyGate_AntiRepeat_NegativeWindowTreatedAsZero(t *testing.T) {
	in := GateInput{
		Phase:            models.PhaseInstruction,
		Concepts:         []string{"A"},
		States:           statesMap(gateCS("A", 0.3)),
		Graph:            graph([]string{"A"}, nil),
		RecentConcepts:   []string{"A"},
		AntiRepeatWindow: -1,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"A"})
}

// ─── Rule 1 : misconception action restriction ─────────────────────────────

func TestApplyGate_Misconception_RestrictsActions(t *testing.T) {
	in := GateInput{
		Phase:                models.PhaseInstruction,
		Concepts:             []string{"A", "B"},
		States:               statesMap(gateCS("A", 0.3), gateCS("B", 0.3)),
		Graph:                graph([]string{"A", "B"}, nil),
		ActiveMisconceptions: map[string]bool{"A": true},
	}
	r, _ := ApplyGate(in)
	if got, ok := r.ActionRestriction["A"]; !ok {
		t.Fatalf("expected restriction on A, got %+v", r.ActionRestriction)
	} else if len(got) != 1 || got[0] != models.ActivityDebugMisconception {
		t.Errorf("expected only DEBUG_MISCONCEPTION on A, got %v", got)
	}
	if _, ok := r.ActionRestriction["B"]; ok {
		t.Errorf("expected NO restriction on B, got %v", r.ActionRestriction["B"])
	}
}

func TestApplyGate_Misconception_DoesNotFilterConcept(t *testing.T) {
	in := GateInput{
		Phase:                models.PhaseInstruction,
		Concepts:             []string{"A"},
		States:               statesMap(gateCS("A", 0.3)),
		Graph:                graph([]string{"A"}, nil),
		ActiveMisconceptions: map[string]bool{"A": true},
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"A"})
}

// TestApplyGate_Misconception_DoesNotBypassPrereq confirms OQ-3.5 sub-b:
// even with an active misconception, a concept whose prereqs are
// unsatisfied stays excluded. (Pathological signal — logged at INFO.)
func TestApplyGate_Misconception_DoesNotBypassPrereq(t *testing.T) {
	g := graph([]string{"Basics", "Advanced"}, map[string][]string{
		"Advanced": {"Basics"},
	})
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: g.Concepts,
		States: statesMap(
			gateCS("Basics", 0.30), // prereq fails
			gateCS("Advanced", 0.50),
		),
		Graph:                g,
		ActiveMisconceptions: map[string]bool{"Advanced": true},
	}
	r, _ := ApplyGate(in)
	containsNone(t, r.AllowedConcepts, []string{"Advanced"})
	if _, ok := r.ActionRestriction["Advanced"]; ok {
		t.Errorf("expected NO restriction on prereq-blocked concept, got %v",
			r.ActionRestriction["Advanced"])
	}
}

// ─── Phase-specific (OQ-3.6) ───────────────────────────────────────────────

func TestApplyGate_Diagnostic_BypassesPrereqs(t *testing.T) {
	g := graph([]string{"Basics", "Advanced"}, map[string][]string{
		"Advanced": {"Basics"},
	})
	in := GateInput{
		Phase:    models.PhaseDiagnostic,
		Concepts: g.Concepts,
		States:   statesMap(gateCS("Basics", 0.30), gateCS("Advanced", 0.50)),
		Graph:    g,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"Basics", "Advanced"})
}

func TestApplyGate_Diagnostic_StillRespectsOverload(t *testing.T) {
	in := GateInput{
		Phase:    models.PhaseDiagnostic,
		Concepts: []string{"A"},
		States:   statesMap(gateCS("A", 0.5)),
		Graph:    graph([]string{"A"}, nil),
		Alerts:   []models.Alert{{Type: models.AlertOverload}},
	}
	r, _ := ApplyGate(in)
	if r.EscapeAction == nil {
		t.Errorf("OQ-3.6: DIAGNOSTIC must still emit OVERLOAD escape, got %+v", r)
	}
}

// TestApplyGate_Diagnostic_StillRestrictsMisconception confirms
// misconception remains an active veto in DIAGNOSTIC (UX surprise but
// pedagogically warranted — see comment in ApplyGate).
func TestApplyGate_Diagnostic_StillRestrictsMisconception(t *testing.T) {
	in := GateInput{
		Phase:                models.PhaseDiagnostic,
		Concepts:             []string{"A"},
		States:               statesMap(gateCS("A", 0.5)),
		Graph:                graph([]string{"A"}, nil),
		ActiveMisconceptions: map[string]bool{"A": true},
	}
	r, _ := ApplyGate(in)
	if got, ok := r.ActionRestriction["A"]; !ok || got[0] != models.ActivityDebugMisconception {
		t.Errorf("OQ-3.6: misconception must restrict in DIAGNOSTIC, got %+v", r.ActionRestriction)
	}
}

func TestApplyGate_Diagnostic_StillRespectsAntiRep(t *testing.T) {
	in := GateInput{
		Phase:            models.PhaseDiagnostic,
		Concepts:         []string{"A", "B", "C"},
		States:           statesMap(gateCS("A", 0.5), gateCS("B", 0.5), gateCS("C", 0.5)),
		Graph:            graph([]string{"A", "B", "C"}, nil),
		RecentConcepts:   []string{"A"},
		AntiRepeatWindow: 1,
	}
	r, _ := ApplyGate(in)
	containsNone(t, r.AllowedConcepts, []string{"A"})
	containsAll(t, r.AllowedConcepts, []string{"B", "C"})
}

// ─── Composition matrix (OQ-3.5) ───────────────────────────────────────────

func TestApplyGate_AllRulesCombined(t *testing.T) {
	g := graph([]string{"Pre", "Mid", "Adv"}, map[string][]string{
		"Mid": {"Pre"},
		"Adv": {"Mid"},
	})
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: g.Concepts,
		States: statesMap(
			gateCS("Pre", 0.95), // mastered
			gateCS("Mid", 0.50), // prereq OK, eligible
			gateCS("Adv", 0.10), // prereq fails
		),
		Graph:                g,
		ActiveMisconceptions: map[string]bool{"Mid": true},
		RecentConcepts:       []string{"Mid"},
		AntiRepeatWindow:     1,
		Alerts:               []models.Alert{}, // no overload
	}
	r, err := ApplyGate(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Pre is in fringe (Pre has no prereqs, mastered=ok per Gate's
	// scope — Gate doesn't filter by mastery, that's [4]'s job).
	// Mid: prereq OK, in recent, but misconception bypasses anti-rep.
	// Adv: prereq fails.
	containsAll(t, r.AllowedConcepts, []string{"Pre", "Mid"})
	containsNone(t, r.AllowedConcepts, []string{"Adv"})
	if got := r.ActionRestriction["Mid"]; len(got) != 1 || got[0] != models.ActivityDebugMisconception {
		t.Errorf("expected DEBUG_MISCONCEPTION on Mid, got %v", got)
	}
}

func TestApplyGate_NoCandidate_AllPrereqFiltered(t *testing.T) {
	g := graph([]string{"A", "B"}, map[string][]string{
		"A": {"X"},
		"B": {"X"},
	})
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: g.Concepts,
		States:   statesMap(),
		Graph:    g,
	}
	r, _ := ApplyGate(in)
	if !r.NoCandidate {
		t.Fatalf("expected NoCandidate when all prereqs miss, got %+v", r)
	}
}

func TestApplyGate_EmptyCandidates_NoCandidate(t *testing.T) {
	in := GateInput{Phase: models.PhaseInstruction}
	r, _ := ApplyGate(in)
	if !r.NoCandidate {
		t.Errorf("expected NoCandidate on empty input, got %+v", r)
	}
}

// ─── Phase invalid ─────────────────────────────────────────────────────────

func TestApplyGate_PhaseInvalid_ReturnsError(t *testing.T) {
	in := GateInput{Phase: models.Phase("BOGUS"), Concepts: []string{"A"}}
	_, err := ApplyGate(in)
	if !errors.Is(err, ErrGateUnknownPhase) {
		t.Errorf("expected ErrGateUnknownPhase, got %v", err)
	}
}

func TestApplyGate_EmptyPhase_ReturnsError(t *testing.T) {
	in := GateInput{Phase: models.Phase(""), Concepts: []string{"A"}}
	_, err := ApplyGate(in)
	if !errors.Is(err, ErrGateUnknownPhase) {
		t.Errorf("expected ErrGateUnknownPhase on empty phase, got %v", err)
	}
}

// ─── Degenerate cases ──────────────────────────────────────────────────────

func TestApplyGate_NilAlerts(t *testing.T) {
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: []string{"A"},
		States:   statesMap(gateCS("A", 0.5)),
		Graph:    graph([]string{"A"}, nil),
		Alerts:   nil,
	}
	r, err := ApplyGate(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.EscapeAction != nil || r.NoCandidate {
		t.Errorf("expected normal allow, got %+v", r)
	}
}

func TestApplyGate_NilRecentConcepts(t *testing.T) {
	in := GateInput{
		Phase:            models.PhaseInstruction,
		Concepts:         []string{"A"},
		States:           statesMap(gateCS("A", 0.5)),
		Graph:            graph([]string{"A"}, nil),
		RecentConcepts:   nil,
		AntiRepeatWindow: 3,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"A"})
}

func TestApplyGate_NaN_PMastery_OnPrereq_Excludes(t *testing.T) {
	// Defensive: NaN mastery on prereq must NOT be treated as
	// satisfied (NaN < kstThreshold is false in raw Go but
	// math.IsNaN guard rejects it).
	g := graph([]string{"Basics", "Adv"}, map[string][]string{"Adv": {"Basics"}})
	csB := gateCS("Basics", math.NaN())
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: g.Concepts,
		States:   statesMap(csB, gateCS("Adv", 0.5)),
		Graph:    g,
	}
	r, _ := ApplyGate(in)
	containsNone(t, r.AllowedConcepts, []string{"Adv"})
}

// ─── MasteryKST accessor ───────────────────────────────────────────────────

func TestApplyGate_RespectsMasteryKSTAccessor_Legacy(t *testing.T) {
	// Under legacy threshold (0.70), a prereq at 0.75 satisfies; at 0.65 it doesn't.
	t.Setenv("REGULATION_THRESHOLD", "off")
	g := graph([]string{"Basics", "Adv"}, map[string][]string{"Adv": {"Basics"}})
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: g.Concepts,
		States:   statesMap(gateCS("Basics", 0.75), gateCS("Adv", 0.10)),
		Graph:    g,
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"Basics", "Adv"})
}

// ─── Issue #16 — FORGETTING-Critical priority filter ───────────────────────
//
// When at least one AlertForgetting with Urgency=Critical references a
// concept that survives prereq+anti-rep, the gate restricts AllowedConcepts
// to those critical-forgetting concepts only. [4]/[5] keep their normal
// contracts; [5]'s retention branch naturally produces RECALL_EXERCISE.

func TestApplyGate_ForgettingCritical_RestrictsPoolToForgettingConcepts(t *testing.T) {
	// Reproduces issue #16 scenario: phase=INSTRUCTION, concept X has
	// retention<0.30 and goal_relevance high but mastery=0.55 (lower
	// (1-mastery) score than newer concept A). Without the fix, [4]
	// argmax(rel × (1-mastery)) picks A. With the fix, the gate
	// restricts the pool to {X} so [4] is forced to pick it.
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: []string{"A", "X"},
		States:   statesMap(gateCS("A", 0.10), gateCS("X", 0.55)),
		Graph:    graph([]string{"A", "X"}, nil),
		Alerts: []models.Alert{
			{Type: models.AlertForgetting, Concept: "X", Urgency: models.UrgencyCritical, Retention: 0.20},
		},
	}
	r, err := ApplyGate(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.EscapeAction != nil {
		t.Fatalf("expected allow result, got escape %+v", r.EscapeAction)
	}
	if r.NoCandidate {
		t.Fatalf("expected allow result, got NoCandidate")
	}
	containsAll(t, r.AllowedConcepts, []string{"X"})
	containsNone(t, r.AllowedConcepts, []string{"A"})
}

func TestApplyGate_ForgettingWarning_DoesNotRestrictPool(t *testing.T) {
	// Only Critical urgency triggers the restriction; Warning leaves
	// the normal pool semantics in place.
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: []string{"A", "X"},
		States:   statesMap(gateCS("A", 0.10), gateCS("X", 0.55)),
		Graph:    graph([]string{"A", "X"}, nil),
		Alerts: []models.Alert{
			{Type: models.AlertForgetting, Concept: "X", Urgency: models.UrgencyWarning, Retention: 0.40},
		},
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"A", "X"})
}

func TestApplyGate_ForgettingCritical_FilteredByPrereq_NoRestriction(t *testing.T) {
	// X has unsatisfied prereq → filtered out by rule 2. No critical
	// concept survives, so the restriction does not kick in and A
	// remains in the pool.
	g := graph([]string{"A", "Basics", "X"}, map[string][]string{"X": {"Basics"}})
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: g.Concepts,
		States:   statesMap(gateCS("A", 0.10), gateCS("Basics", 0.10), gateCS("X", 0.55)),
		Graph:    g,
		Alerts: []models.Alert{
			{Type: models.AlertForgetting, Concept: "X", Urgency: models.UrgencyCritical, Retention: 0.20},
		},
	}
	r, _ := ApplyGate(in)
	containsAll(t, r.AllowedConcepts, []string{"A", "Basics"})
	containsNone(t, r.AllowedConcepts, []string{"X"})
}

func TestApplyGate_ForgettingCritical_OverloadStillWins(t *testing.T) {
	// OVERLOAD escape priority is preserved (rule 3 stays at priority 1).
	in := GateInput{
		Phase:    models.PhaseInstruction,
		Concepts: []string{"A", "X"},
		States:   statesMap(gateCS("A", 0.10), gateCS("X", 0.55)),
		Graph:    graph([]string{"A", "X"}, nil),
		Alerts: []models.Alert{
			{Type: models.AlertOverload},
			{Type: models.AlertForgetting, Concept: "X", Urgency: models.UrgencyCritical, Retention: 0.20},
		},
	}
	r, _ := ApplyGate(in)
	if r.EscapeAction == nil {
		t.Fatalf("expected OVERLOAD escape, got allow %+v", r.AllowedConcepts)
	}
}
