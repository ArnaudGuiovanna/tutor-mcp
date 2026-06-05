package algorithms

import (
	"testing"
)

func TestIRTProbability(t *testing.T) {
	tests := []struct {
		theta, difficulty, discrimination, want float64
	}{
		{0, 0, 1, 0.5},
		{1, 0, 1, 0.7311},
		{0, 1, 1, 0.2689},
		{2, 0, 2, 0.9820},
	}
	for _, tt := range tests {
		got := IRTProbability(tt.theta, tt.difficulty, tt.discrimination)
		if !approxEqual(got, tt.want, 0.01) {
			t.Errorf("IRTProbability(%.1f, %.1f, %.1f) = %.4f, want ~%.4f", tt.theta, tt.difficulty, tt.discrimination, got, tt.want)
		}
	}
}

func TestIRTUpdateTheta(t *testing.T) {
	items := []IRTItem{{Difficulty: 0, Discrimination: 1}, {Difficulty: 0.5, Discrimination: 1}}
	newTheta := IRTUpdateTheta(0, items, []bool{true, true})
	if newTheta <= 0 {
		t.Errorf("theta should increase after all correct, got %f", newTheta)
	}
	newTheta = IRTUpdateTheta(0, items, []bool{false, false})
	if newTheta >= 0 {
		t.Errorf("theta should decrease after all incorrect, got %f", newTheta)
	}
}

func TestIRTIsInZPD(t *testing.T) {
	if !IRTIsInZPD(0.65) {
		t.Error("0.65 should be in ZPD")
	}
	if IRTIsInZPD(0.90) {
		t.Error("0.90 should NOT be in ZPD")
	}
	if IRTIsInZPD(0.40) {
		t.Error("0.40 should NOT be in ZPD")
	}
}

func TestFSRSDifficultyToIRT(t *testing.T) {
	tests := []struct {
		fsrs float64
		want float64
	}{
		{1.0, -3.0}, // easiest FSRS → lowest IRT
		{10.0, 3.0}, // hardest FSRS → highest IRT
		{5.5, 0.0},  // midpoint → zero
	}
	for _, tt := range tests {
		got := FSRSDifficultyToIRT(tt.fsrs)
		if !approxEqual(got, tt.want, 0.01) {
			t.Errorf("FSRSDifficultyToIRT(%.1f) = %.2f, want %.2f", tt.fsrs, got, tt.want)
		}
	}
}

// TestIRTUpdateThetaGuards verifies that IRTUpdateTheta returns the input theta
// unchanged when input slices are empty or mismatched in length — the early
// guard at the top of the function.
func TestIRTUpdateThetaGuards(t *testing.T) {
	tests := []struct {
		name      string
		theta     float64
		items     []IRTItem
		responses []bool
	}{
		{"empty items", 1.5, []IRTItem{}, []bool{}},
		{"nil items", 0.7, nil, nil},
		{"mismatched lengths (more responses)", -0.5,
			[]IRTItem{{Difficulty: 0, Discrimination: 1}},
			[]bool{true, false}},
		{"mismatched lengths (more items)", 2.0,
			[]IRTItem{{Difficulty: 0, Discrimination: 1}, {Difficulty: 1, Discrimination: 1}},
			[]bool{true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IRTUpdateTheta(tc.theta, tc.items, tc.responses)
			if !approxEqual(got, tc.theta, 1e-12) {
				t.Errorf("IRTUpdateTheta(%f, ...) = %f, want %f (unchanged)",
					tc.theta, got, tc.theta)
			}
		})
	}
}

// TestIRTUpdateThetaZeroDiscrimination triggers the d2L == 0 break: when every
// item has Discrimination == 0 the second derivative is exactly zero and the
// Newton step is undefined, so the loop must short-circuit and return the
// clamped input theta.
func TestIRTUpdateThetaZeroDiscrimination(t *testing.T) {
	items := []IRTItem{
		{Difficulty: 0, Discrimination: 0},
		{Difficulty: 1, Discrimination: 0},
	}
	got := IRTUpdateTheta(0.5, items, []bool{true, false})
	// With zero discrimination, theta should remain unchanged (after clamp).
	if !approxEqual(got, 0.5, 1e-12) {
		t.Errorf("IRTUpdateTheta with zero discrimination = %f, want 0.5 (unchanged)", got)
	}
}

// TestIRTUpdateThetaEarlyConvergence triggers the |step| < 0.001 break: when
// the input theta is already at (or very close to) the MLE, the Newton step
// rounds to ~0 and we should exit before the 5-iteration cap.
func TestIRTUpdateThetaEarlyConvergence(t *testing.T) {
	// Symmetric setup: at theta=0, one correct and one incorrect on identical
	// items at Difficulty 0 yields dL = 0 exactly, so step = 0 < 0.001.
	items := []IRTItem{
		{Difficulty: 0, Discrimination: 1},
		{Difficulty: 0, Discrimination: 1},
	}
	got := IRTUpdateTheta(0, items, []bool{true, false})
	if !approxEqual(got, 0, 1e-9) {
		t.Errorf("at MLE, theta should be unchanged, got %f", got)
	}
}

// TestIRTUpdateThetaClamps verifies that the returned theta is clamped into
// the [-4, 4] range even when the Newton step would push it far past the
// bounds (e.g. all-correct on very hard items pushes theta upward).
func TestIRTUpdateThetaClamps(t *testing.T) {
	hardItems := []IRTItem{
		{Difficulty: 5, Discrimination: 2},
		{Difficulty: 5, Discrimination: 2},
		{Difficulty: 5, Discrimination: 2},
	}
	got := IRTUpdateTheta(0, hardItems, []bool{true, true, true})
	if got < -4 || got > 4 {
		t.Errorf("theta should be clamped to [-4,4], got %f", got)
	}

	easyItems := []IRTItem{
		{Difficulty: -5, Discrimination: 2},
		{Difficulty: -5, Discrimination: 2},
		{Difficulty: -5, Discrimination: 2},
	}
	got = IRTUpdateTheta(0, easyItems, []bool{false, false, false})
	if got < -4 || got > 4 {
		t.Errorf("theta should be clamped to [-4,4], got %f", got)
	}
}

// TestIRTIsInZPDBoundaries pins the inclusive boundaries of the ZPD predicate.
func TestIRTIsInZPDBoundaries(t *testing.T) {
	tests := []struct {
		name string
		p    float64
		want bool
	}{
		{"exact lower bound", 0.55, true},
		{"just below lower", 0.5499999, false},
		{"exact upper bound", 0.80, true},
		{"just above upper", 0.8000001, false},
		{"midpoint", 0.675, true},
		{"zero", 0.0, false},
		{"one", 1.0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IRTIsInZPD(tc.p); got != tc.want {
				t.Errorf("IRTIsInZPD(%f) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

// TestIRTProbabilityBounds checks that IRTProbability stays in (0, 1) and
// is monotone increasing in theta for fixed difficulty/discrimination.
func TestIRTProbabilityBounds(t *testing.T) {
	thetas := []float64{-10, -3, -1, 0, 1, 3, 10}
	prev := -1.0
	for _, theta := range thetas {
		p := IRTProbability(theta, 0, 1)
		if p <= 0 || p >= 1 {
			t.Errorf("IRTProbability(%f, 0, 1) = %f, want in (0,1)", theta, p)
		}
		if p <= prev {
			t.Errorf("monotonicity violated: theta=%f gave p=%f, prev=%f", theta, p, prev)
		}
		prev = p
	}
}
