// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"math"
	"testing"
	"time"

	"tutor-mcp/models"
)

// chainGraph returns c0 <- c1 <- ... <- c(n-1): each concept requires the
// previous one.
func chainGraph(n int) models.KnowledgeSpace {
	graph := models.KnowledgeSpace{Prerequisites: map[string][]string{}}
	for i := range n {
		concept := string(rune('a' + i))
		graph.Concepts = append(graph.Concepts, concept)
		if i > 0 {
			graph.Prerequisites[concept] = []string{string(rune('a' + i - 1))}
		}
	}
	return graph
}

func TestPlacementSingleSuccessBeatsColdPrior(t *testing.T) {
	p := ReplayPlacement(graphFlat("a"), nil, []models.DiagnosticObservation{{Concept: "a", Success: true}}, 0)
	got := p.Concepts["a"].Estimate
	// 0.5 prior, slip 0.1, guess 0.2: 0.45 / (0.45 + 0.10).
	if math.Abs(got-0.45/0.55) > 1e-12 {
		t.Fatalf("estimate after one success = %v, want %v", got, 0.45/0.55)
	}
	if p.Concepts["a"].Classified() {
		t.Fatal("one success leaves the estimate in the ambiguous band: a second item is due")
	}
	p = ReplayPlacement(graphFlat("a"), nil, []models.DiagnosticObservation{{Concept: "a", Success: true}, {Concept: "a", Success: true}}, 0)
	if !p.Concepts["a"].Classified() || p.Concepts["a"].Estimate < PlacementUpperBound {
		t.Fatalf("two successes must classify as known, got %+v", p.Concepts["a"])
	}
}

func TestPlacementFailureClassifiesAfterOneItem(t *testing.T) {
	p := ReplayPlacement(graphFlat("a"), nil, []models.DiagnosticObservation{{Concept: "a"}}, 0)
	if e := p.Concepts["a"]; !e.Classified() || e.Estimate > PlacementLowerBound {
		t.Fatalf("one failure must classify as not known, got %+v", e)
	}
}

func TestPlacementSecondItemCapsDirectTesting(t *testing.T) {
	p := ReplayPlacement(graphFlat("a"), nil, []models.DiagnosticObservation{{Concept: "a", Success: true}, {Concept: "a"}}, 0)
	e := p.Concepts["a"]
	if e.Estimate <= PlacementLowerBound || e.Estimate >= PlacementUpperBound {
		t.Fatalf("fixture: mixed answers should stay ambiguous, got %v", e.Estimate)
	}
	if !e.Classified() {
		t.Fatal("a concept is never tested more than PlacementMaxItemsPerConcept times")
	}
}

func TestPlacementPropagatesSuccessToAncestorsAndFailureToDescendants(t *testing.T) {
	graph := chainGraph(5) // a <- b <- c <- d <- e
	success := ReplayPlacement(graph, nil, []models.DiagnosticObservation{{Concept: "c", Success: true}}, 0)
	for _, concept := range []string{"a", "b", "c"} {
		if e := success.Concepts[concept]; e.Estimate <= PlacementPrior || !e.Observed() {
			t.Fatalf("success on c must raise ancestor %s, got %+v", concept, e)
		}
	}
	for _, concept := range []string{"d", "e"} {
		if e := success.Concepts[concept]; e.Observed() || e.Estimate != PlacementPrior {
			t.Fatalf("success on c must not touch descendant %s, got %+v", concept, e)
		}
	}
	if success.Concepts["a"].DirectItems != 0 || success.Concepts["a"].PropagatedItems != 1 {
		t.Fatalf("propagated evidence must not count as a direct item: %+v", success.Concepts["a"])
	}

	failure := ReplayPlacement(graph, nil, []models.DiagnosticObservation{{Concept: "c"}}, 0)
	for _, concept := range []string{"c", "d", "e"} {
		if e := failure.Concepts[concept]; !e.Classified() || e.Estimate > PlacementLowerBound {
			t.Fatalf("failure on c must classify descendant %s as not known, got %+v", concept, e)
		}
	}
	for _, concept := range []string{"a", "b"} {
		if failure.Concepts[concept].Observed() {
			t.Fatalf("failure on c must not touch prerequisite %s", concept)
		}
	}
}

func TestPlacementIgnoresOutOfGraphObservations(t *testing.T) {
	p := ReplayPlacement(graphFlat("a"), nil, []models.DiagnosticObservation{
		{Concept: "elsewhere", Success: true},
	}, 0)
	if p.Items != 0 || p.Concepts["a"].Observed() {
		t.Fatalf("out-of-graph observations must not count, got %+v", p)
	}
}

func TestPlacementObservationOrderDoesNotMatter(t *testing.T) {
	graph := chainGraph(4)
	obs := []models.DiagnosticObservation{{Concept: "c", Success: true}, {Concept: "b"}, {Concept: "d", Success: true}}
	reversed := []models.DiagnosticObservation{obs[2], obs[1], obs[0]}
	a, b := ReplayPlacement(graph, nil, obs, 0), ReplayPlacement(graph, nil, reversed, 0)
	for _, concept := range graph.Concepts {
		if math.Abs(a.Concepts[concept].Estimate-b.Concepts[concept].Estimate) > 1e-12 {
			t.Fatalf("replay depends on order for %s: %v vs %v", concept, a.Concepts[concept], b.Concepts[concept])
		}
	}
}

func TestPlacementNextConceptBisectsTheChain(t *testing.T) {
	graph := chainGraph(7) // depths 0..6
	p := ReplayPlacement(graph, nil, nil, 0)
	first, _, ok := p.NextConcept(nil)
	if !ok || first != "d" {
		t.Fatalf("first item must target the median depth (d), got %q", first)
	}

	// Learner knows d (two successes): a..d known, next median among e,f,g.
	p = ReplayPlacement(graph, nil, []models.DiagnosticObservation{{Concept: "d", Success: true}, {Concept: "d", Success: true}}, 0)
	for _, concept := range []string{"a", "b", "c", "d"} {
		if !p.Concepts[concept].Classified() {
			t.Fatalf("%s should be classified known after two successes on d: %+v", concept, p.Concepts[concept])
		}
	}
	if next, _, _ := p.NextConcept(nil); next != "f" {
		t.Fatalf("after placing d as known, expected f (median of e,f,g), got %q", next)
	}

	// Learner fails d: d..g not known, next median among a,b,c.
	p = ReplayPlacement(graph, nil, []models.DiagnosticObservation{{Concept: "d"}}, 0)
	if next, _, _ := p.NextConcept(nil); next != "b" {
		t.Fatalf("after failing d, expected b (median of a,b,c), got %q", next)
	}
}

func TestPlacementNextConceptPrefersWiderReachAtMedianDepth(t *testing.T) {
	// Two roots; "hub" has two descendants, "solo" none.
	graph := models.KnowledgeSpace{
		Concepts: []string{"solo", "hub", "x", "y"},
		Prerequisites: map[string][]string{
			"x": {"hub"},
			"y": {"hub"},
		},
	}
	p := ReplayPlacement(graph, nil, nil, 0)
	if next, _, _ := p.NextConcept(nil); next != "hub" {
		t.Fatalf("expected the root with the widest reach, got %q", next)
	}
}

func TestPlacementCompletesOnClassificationOrBudget(t *testing.T) {
	graph := chainGraph(3)
	p := ReplayPlacement(graph, nil, []models.DiagnosticObservation{{Concept: "a"}}, 0)
	if !p.Complete() || len(p.Unclassified()) != 0 {
		t.Fatalf("failing the root classifies the whole chain, got unclassified=%v", p.Unclassified())
	}
	p = ReplayPlacement(chainGraph(7), nil, []models.DiagnosticObservation{{Concept: "d", Success: true}}, 1)
	if !p.BudgetReached() || !p.Complete() {
		t.Fatal("budget of one item must end the placement")
	}
	if _, _, ok := p.NextConcept(nil); ok {
		t.Fatal("no next item once the budget is spent")
	}
}

func TestPlacementNextConceptRespectsAllowedPool(t *testing.T) {
	p := ReplayPlacement(chainGraph(7), nil, nil, 0)
	next, _, ok := p.NextConcept(map[string]bool{"a": true, "g": true})
	if !ok || (next != "a" && next != "g") {
		t.Fatalf("selection must stay inside the allowed pool, got %q", next)
	}
	if _, _, ok := p.NextConcept(map[string]bool{"zzz": true}); ok {
		t.Fatal("no candidate when the pool holds no unclassified concept")
	}
}

func TestPlacementToleratesCycles(t *testing.T) {
	graph := models.KnowledgeSpace{
		Concepts:      []string{"a", "b"},
		Prerequisites: map[string][]string{"a": {"b"}, "b": {"a"}},
	}
	p := ReplayPlacement(graph, nil, []models.DiagnosticObservation{{Concept: "a", Success: true}}, 0)
	if !p.Concepts["b"].Observed() {
		t.Fatal("b is an ancestor of a through the cycle")
	}
	if _, _, ok := p.NextConcept(nil); !ok {
		t.Fatal("cyclic graph must still yield a next concept")
	}
}

func TestPlacementPersistableEstimatesSkipForeignEvidence(t *testing.T) {
	observations := []models.DiagnosticObservation{{Concept: "c", Success: true}}
	p := ReplayPlacement(chainGraph(4), nil, observations, 0)
	got := p.PersistableEstimates(map[string]bool{"a": true})
	if _, ok := got["a"]; ok {
		t.Fatal("a has evidence outside the placement and keeps its own estimate")
	}
	if got["b"] != p.Concepts["b"].Estimate || got["c"] != p.Concepts["c"].Estimate {
		t.Fatalf("placement-only concepts must be persisted, got %v", got)
	}
	if _, ok := got["d"]; ok {
		t.Fatal("an unobserved concept must not be written")
	}
}

// Answers beyond PlacementMaxItemsPerConcept on one concept are ignored:
// they neither spend the budget nor propagate.
func TestPlacementRepeatedItemsCountOncePerCap(t *testing.T) {
	var observations []models.DiagnosticObservation
	for range 8 {
		observations = append(observations, models.DiagnosticObservation{Concept: "c", Success: true})
	}
	p := ReplayPlacement(chainGraph(5), nil, observations, 4)
	if p.Items != PlacementMaxItemsPerConcept || p.BudgetReached() {
		t.Fatalf("eight repetitions spent %d items (budget reached=%v), want %d", p.Items, p.BudgetReached(), PlacementMaxItemsPerConcept)
	}
	capped := ReplayPlacement(chainGraph(5), nil, observations[:PlacementMaxItemsPerConcept], 4)
	for concept, estimate := range p.Concepts {
		if estimate != capped.Concepts[concept] {
			t.Fatalf("%s: surplus answers changed the placement: %+v vs %+v", concept, estimate, capped.Concepts[concept])
		}
	}
	if len(p.Unclassified()) == 0 {
		t.Fatal("descendants of c must stay to be placed")
	}
}

// A concept whose stored estimate is not finite is never served and never
// keeps the placement open, so selection and completion agree.
func TestPlacementNonFiniteEstimateIsUnplaceable(t *testing.T) {
	graph := graphFlat("a", "b")
	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		states := map[string]*models.ConceptState{"b": {Concept: "b", PMastery: bad}}
		p := ReplayPlacement(graph, states, []models.DiagnosticObservation{{Concept: "a"}}, 0)
		if !p.Complete() || len(p.Unclassified()) != 0 {
			t.Fatalf("P(L)=%v: placement must complete, unclassified=%v", bad, p.Unclassified())
		}
		if next, _, ok := p.NextConcept(nil); ok {
			t.Fatalf("P(L)=%v: unplaceable concept served: %q", bad, next)
		}
		sel := selectDiagnostic([]*models.ConceptState{states["b"]}, graph, graph, nil)
		if sel.Concept != "a" {
			t.Fatalf("P(L)=%v: fresh selection must skip b, got %+v", bad, sel)
		}
	}
}

func TestSelectDiagnosticUsesPlacementAndDomainGraph(t *testing.T) {
	graph := chainGraph(7)
	now := time.Now().UTC()
	sel, err := SelectConceptAtWithContext(models.PhaseDiagnostic, nil, graph, map[string]float64{"a": 1}, now, SelectionContext{})
	if err != nil || sel.Concept != "d" {
		t.Fatalf("fresh placement must bisect the graph and ignore goal relevance, got %+v err=%v", sel, err)
	}
	placement := ReplayPlacement(graph, nil, []models.DiagnosticObservation{{Concept: "d"}}, 0)
	sel, _ = SelectConceptAtWithContext(models.PhaseDiagnostic, nil, graphFlat("a", "b", "c", "x"), nil, now, SelectionContext{
		Placement:   &placement,
		DomainGraph: &graph,
	})
	if sel.Concept != "b" {
		t.Fatalf("expected the median of the unclassified prerequisites, got %+v", sel)
	}
	sel, _ = SelectConceptAtWithContext(models.PhaseDiagnostic, nil, graphFlat("x"), nil, now, SelectionContext{Placement: &placement})
	if !sel.NoFringe {
		t.Fatalf("a pool outside the domain graph has no placement candidate, got %+v", sel)
	}
}
