package algorithms

import (
	"math"
	"testing"
)

func TestBKTUpdateCorrect(t *testing.T) {
	state := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}
	updated := BKTTransition(BKTObserve(state, true))
	if !approxEqual(updated.PMastery, 0.8318, 0.01) {
		t.Errorf("PMastery after correct = %f, want ~0.8318", updated.PMastery)
	}
}

func TestBKTUpdateIncorrect(t *testing.T) {
	state := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}
	updated := BKTTransition(BKTObserve(state, false))
	if !approxEqual(updated.PMastery, 0.3722, 0.01) {
		t.Errorf("PMastery after incorrect = %f, want ~0.3722", updated.PMastery)
	}
}

func TestBKTMasteryThreshold(t *testing.T) {
	state := BKTState{PMastery: 0.86}
	if !BKTIsMastered(state) {
		t.Error("0.86 should be mastered")
	}
	state.PMastery = 0.84
	if BKTIsMastered(state) {
		t.Error("0.84 should NOT be mastered")
	}
}

func TestBKTConvergence(t *testing.T) {
	state := BKTState{PMastery: 0.1, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}
	for i := 0; i < 10; i++ {
		state = BKTTransition(BKTObserve(state, true))
	}
	if state.PMastery < 0.85 {
		t.Errorf("after 10 correct, PMastery = %f, want >= 0.85", state.PMastery)
	}
}

func TestBKTUpdateClampsToOne(t *testing.T) {
	// Pre-clamp PMastery should remain in [0, 1].
	state := BKTState{PMastery: 0.99, PLearn: 0.99, PForget: 0.0, PSlip: 0.0, PGuess: 0.0}
	updated := BKTTransition(BKTObserve(state, true))
	if updated.PMastery < 0 || updated.PMastery > 1 {
		t.Errorf("PMastery out of [0,1]: %f", updated.PMastery)
	}
	if !approxEqual(updated.PMastery, 1.0, 1e-9) {
		t.Errorf("PMastery should clamp to 1, got %f", updated.PMastery)
	}
}

// TestBKTUpdateNoNaNOrInfOnDegenerateInputs verifies BKTUpdate never returns
// NaN or Inf when the input parameters make the marginal observation
// probability collapse to zero (e.g. PMastery=0 with PGuess=0 on a "correct"
// observation). Without a guard the Bayesian division 0/0 yields NaN, which
// then poisons every downstream consumer.
func TestBKTUpdateNoNaNOrInfOnDegenerateInputs(t *testing.T) {
	tests := []struct {
		name    string
		state   BKTState
		correct bool
	}{
		{
			// pCorrect = (1-0)*0 + 0*1 = 0  → 0/0 = NaN before the guard.
			name:    "correct with PMastery=0 and PGuess=0",
			state:   BKTState{PMastery: 0, PLearn: 0.3, PForget: 0.05, PSlip: 0, PGuess: 0},
			correct: true,
		},
		{
			// pCorrect = 0*1 + 1*0 = 0  → 0/0 = NaN before the guard.
			name:    "correct with PMastery=1 and PSlip=1, PGuess=0",
			state:   BKTState{PMastery: 1, PLearn: 0.3, PForget: 0.05, PSlip: 1, PGuess: 0},
			correct: true,
		},
		{
			// pIncorrect = 0*0 + 0*1 = 0  → 0/0 = NaN before the guard.
			name:    "incorrect with PMastery=0, PSlip=0, PGuess=1",
			state:   BKTState{PMastery: 0, PLearn: 0.3, PForget: 0.05, PSlip: 0, PGuess: 1},
			correct: false,
		},
		{
			// pIncorrect = 0*1 + 0*0 = 0  → 0/0 = NaN before the guard.
			name:    "incorrect with PMastery=1, PSlip=0, PGuess=1",
			state:   BKTState{PMastery: 1, PLearn: 0.3, PForget: 0.05, PSlip: 0, PGuess: 1},
			correct: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BKTTransition(BKTObserve(tc.state, tc.correct))
			if math.IsNaN(got.PMastery) || math.IsInf(got.PMastery, 0) {
				t.Errorf("PMastery is NaN/Inf: %v", got.PMastery)
			}
			if got.PMastery < 0 || got.PMastery > 1 {
				t.Errorf("PMastery out of [0,1]: %v", got.PMastery)
			}
		})
	}
}
