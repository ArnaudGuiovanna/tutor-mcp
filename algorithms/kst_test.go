package algorithms

import "testing"

func TestComputeFrontier(t *testing.T) {
	graph := KSTGraph{
		Concepts:      []string{"A", "B", "C", "D"},
		Prerequisites: map[string][]string{"B": {"A"}, "C": {"A", "B"}, "D": {"C"}},
	}
	mastery := map[string]float64{"A": 0.8, "B": 0.1, "C": 0.0, "D": 0.0}
	frontier := ComputeFrontier(graph, mastery)
	if len(frontier) != 1 || frontier[0] != "B" {
		t.Errorf("frontier = %v, want [B]", frontier)
	}

	mastery["B"] = 0.75
	frontier = ComputeFrontier(graph, mastery)
	if len(frontier) != 1 || frontier[0] != "C" {
		t.Errorf("frontier = %v, want [C]", frontier)
	}

	mastery = map[string]float64{"A": 0.1, "B": 0.1, "C": 0.0, "D": 0.0}
	frontier = ComputeFrontier(graph, mastery)
	if len(frontier) != 1 || frontier[0] != "A" {
		t.Errorf("frontier = %v, want [A]", frontier)
	}
}

func TestConceptStatus(t *testing.T) {
	graph := KSTGraph{
		Concepts:      []string{"A", "B", "C"},
		Prerequisites: map[string][]string{"B": {"A"}, "C": {"B"}},
	}
	mastery := map[string]float64{"A": 0.85, "B": 0.5, "C": 0.0}
	if s := ConceptStatus(graph, mastery, "A"); s != "done" {
		t.Errorf("A = %s, want done", s)
	}
	if s := ConceptStatus(graph, mastery, "B"); s != "current" {
		t.Errorf("B = %s, want current", s)
	}
	if s := ConceptStatus(graph, mastery, "C"); s != "locked" {
		t.Errorf("C = %s, want locked", s)
	}
}
