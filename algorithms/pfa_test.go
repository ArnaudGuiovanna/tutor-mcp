package algorithms

import "testing"

func TestPFAUpdate(t *testing.T) {
	state := PFAState{Successes: 0, Failures: 0}
	state = PFAUpdate(state, true)
	if !approxEqual(state.Successes, 1.0, 0.001) {
		t.Errorf("successes = %f, want 1", state.Successes)
	}
	score := PFAScore(state)
	if !approxEqual(score, 0.11, 0.01) {
		t.Errorf("score = %f, want ~0.11", score)
	}
	state = PFAUpdate(state, false)
	score = PFAScore(state)
	if !approxEqual(score, 0.0, 0.01) {
		t.Errorf("score = %f, want ~0.0", score)
	}
}

func TestPFAProbability(t *testing.T) {
	// Zero score → sigmoid midpoint 0.5
	state := PFAState{Successes: 0, Failures: 0}
	if !approxEqual(PFAProbability(state), 0.5, 0.001) {
		t.Errorf("PFAProbability at zero = %f, want 0.5", PFAProbability(state))
	}

	// Positive score → probability > 0.5
	state = PFAState{Successes: 5, Failures: 0}
	p := PFAProbability(state)
	if p <= 0.5 || p >= 1.0 {
		t.Errorf("PFAProbability for 5 successes = %f, want (0.5, 1.0)", p)
	}

	// Symmetric: equal successes/failures → 0.5
	state = PFAState{Successes: 10, Failures: 10}
	if !approxEqual(PFAProbability(state), 0.5, 0.001) {
		t.Errorf("PFAProbability symmetric = %f, want 0.5", PFAProbability(state))
	}
}

func TestPFADetectPlateau(t *testing.T) {
	if !PFADetectPlateau([]float64{0.55, 0.56, 0.555, 0.558}, 4) {
		t.Error("should detect plateau")
	}
	if PFADetectPlateau([]float64{0.3, 0.45, 0.6, 0.75}, 4) {
		t.Error("should NOT detect plateau")
	}
	if PFADetectPlateau([]float64{0.5, 0.51, 0.52}, 4) {
		t.Error("should NOT detect plateau with < 4")
	}
}

func TestPFADetectPlateauWithProbabilities(t *testing.T) {
	// Simulate a long streak of successes — sigmoid probabilities should plateau.
	state := PFAState{}
	var probs []float64
	for i := 0; i < 25; i++ {
		state = PFAUpdate(state, true)
		probs = append(probs, PFAProbability(state))
	}
	// After 25 consecutive successes, the sigmoid saturates.
	if !PFADetectPlateau(probs, 4) {
		t.Errorf("25 consecutive successes should plateau, last 4 probs: %v",
			probs[len(probs)-4:])
	}

	// Early interactions: probabilities are still changing fast — no plateau.
	if PFADetectPlateau(probs[:6], 4) {
		t.Error("first 6 interactions should NOT plateau")
	}
}

func TestPFARawScoresNeverPlateauedBefore(t *testing.T) {
	// Confirm that raw PFA scores (linear, not sigmoid) never produce plateau —
	// this was the original bug. Each delta is exactly 0.11 > 0.025.
	state := PFAState{}
	var rawScores []float64
	for i := 0; i < 20; i++ {
		state = PFAUpdate(state, true)
		rawScores = append(rawScores, PFAScore(state))
	}
	if PFADetectPlateau(rawScores, 4) {
		t.Error("raw PFA scores should NOT plateau (constant delta 0.11)")
	}
}
