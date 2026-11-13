// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import "testing"

// TestThresholdIntegration_KSTGating: a prerequisite at 0.75 stays below the
// unified KST threshold and keeps its successor locked.
func TestThresholdIntegration_KSTGating(t *testing.T) {
	graph := KSTGraph{
		Concepts:      []string{"A", "B"},
		Prerequisites: map[string][]string{"B": {"A"}},
	}
	if f := ComputeFrontier(graph, map[string]float64{"A": 0.75, "B": 0.0}); len(f) != 1 || f[0] != "A" {
		t.Errorf("A=0.75 must not unlock B: want [A], got %v", f)
	}
	if f := ComputeFrontier(graph, map[string]float64{"A": 0.85, "B": 0.0}); len(f) != 1 || f[0] != "B" {
		t.Errorf("A=0.85 must unlock B: want [B], got %v", f)
	}
}

// TestThresholdIntegration_BKTMastered checks the BKT threshold boundary.
func TestThresholdIntegration_BKTMastered(t *testing.T) {
	if !BKTIsMastered(BKTState{PMastery: 0.86}) {
		t.Error("0.86 must be mastered")
	}
	if BKTIsMastered(BKTState{PMastery: 0.84}) {
		t.Error("0.84 must not be mastered")
	}
}

// TestThresholdIntegration_MidCollapse: the intermediate threshold equals the
// BKT threshold, so no 0.80-0.85 zone counts as "mastered enough" for hint
// independence but not for a mastery challenge.
func TestThresholdIntegration_MidCollapse(t *testing.T) {
	if 0.83 >= MasteryMid() {
		t.Errorf("0.83 must be < MasteryMid (%v)", MasteryMid())
	}
}
