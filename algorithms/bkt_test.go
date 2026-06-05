package algorithms

import (
	"math"
	"testing"
)

func TestBKTUpdateCorrect(t *testing.T) {
	state := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}
	updated := BKTUpdate(state, true)
	if !approxEqual(updated.PMastery, 0.8318, 0.01) {
		t.Errorf("PMastery after correct = %f, want ~0.8318", updated.PMastery)
	}
}

func TestBKTUpdateIncorrect(t *testing.T) {
	state := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}
	updated := BKTUpdate(state, false)
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
		state = BKTUpdate(state, true)
	}
	if state.PMastery < 0.85 {
		t.Errorf("after 10 correct, PMastery = %f, want >= 0.85", state.PMastery)
	}
}

func TestBKTUpdateClampsToOne(t *testing.T) {
	// Pre-clamp PMastery should remain in [0, 1].
	state := BKTState{PMastery: 0.99, PLearn: 0.99, PForget: 0.0, PSlip: 0.0, PGuess: 0.0}
	updated := BKTUpdate(state, true)
	if updated.PMastery < 0 || updated.PMastery > 1 {
		t.Errorf("PMastery out of [0,1]: %f", updated.PMastery)
	}
	if !approxEqual(updated.PMastery, 1.0, 1e-9) {
		t.Errorf("PMastery should clamp to 1, got %f", updated.PMastery)
	}
}

func TestBKTUpdateHeuristicSlipByErrorType(t *testing.T) {
	base := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}

	tests := []struct {
		name      string
		correct   bool
		errorType string
		// expectation relative to plain BKTUpdate(base, correct)
		// for incorrect: SYNTAX_ERROR softens penalty (higher mastery),
		// KNOWLEDGE_GAP hardens penalty (lower mastery).
		// "" or unknown == standard.
		comparator string // "equal", "greater", "less"
	}{
		{name: "correct ignores errorType", correct: true, errorType: "SYNTAX_ERROR", comparator: "equal"},
		{name: "empty errorType uses standard", correct: false, errorType: "", comparator: "equal"},
		{name: "logic error uses standard", correct: false, errorType: "LOGIC_ERROR", comparator: "equal"},
		{name: "unknown errorType uses standard", correct: false, errorType: "FOOBAR", comparator: "equal"},
		{name: "syntax error softens penalty", correct: false, errorType: "SYNTAX_ERROR", comparator: "greater"},
		{name: "knowledge gap hardens penalty", correct: false, errorType: "KNOWLEDGE_GAP", comparator: "less"},
	}

	standard := BKTUpdate(base, false).PMastery
	standardCorrect := BKTUpdate(base, true).PMastery

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next, _, _ := BKTUpdateHeuristicSlipByErrorType(base, tc.correct, tc.errorType)
			got := next.PMastery
			ref := standard
			if tc.correct {
				ref = standardCorrect
			}
			switch tc.comparator {
			case "equal":
				if !approxEqual(got, ref, 1e-9) {
					t.Errorf("got %f, want %f (equal to standard)", got, ref)
				}
			case "greater":
				if got <= ref {
					t.Errorf("got %f, want > %f (softer penalty)", got, ref)
				}
			case "less":
				if got >= ref {
					t.Errorf("got %f, want < %f (harder penalty)", got, ref)
				}
			}
			if got < 0 || got > 1 {
				t.Errorf("PMastery out of [0,1]: %f", got)
			}
		})
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
			got := BKTUpdate(tc.state, tc.correct)
			if math.IsNaN(got.PMastery) || math.IsInf(got.PMastery, 0) {
				t.Errorf("PMastery is NaN/Inf: %v", got.PMastery)
			}
			if got.PMastery < 0 || got.PMastery > 1 {
				t.Errorf("PMastery out of [0,1]: %v", got.PMastery)
			}
		})
	}
}

// TestBKTUpdateHeuristicSlipByErrorTypeNoNaNOrInf is the same guard at the
// renamed heuristic layer — KNOWLEDGE_GAP/SYNTAX_ERROR mutate slip/guess
// inputs before delegating to BKTUpdate, so we want to make sure the guard
// still holds end-to-end. The function now returns three values; only the
// state is checked here for finiteness.
func TestBKTUpdateHeuristicSlipByErrorTypeNoNaNOrInf(t *testing.T) {
	degenerate := BKTState{PMastery: 0, PLearn: 0.3, PForget: 0.05, PSlip: 0, PGuess: 0}
	for _, et := range []string{"", "LOGIC_ERROR", "SYNTAX_ERROR", "KNOWLEDGE_GAP", "UNKNOWN"} {
		t.Run("errorType="+et, func(t *testing.T) {
			got, _, _ := BKTUpdateHeuristicSlipByErrorType(degenerate, true, et)
			if math.IsNaN(got.PMastery) || math.IsInf(got.PMastery, 0) {
				t.Errorf("PMastery NaN/Inf for errorType=%q: %v", et, got.PMastery)
			}
		})
	}
}

func TestBKTUpdateHeuristicSlipByErrorTypeClampsParameters(t *testing.T) {
	// SYNTAX_ERROR: PSlip clamped to <= 0.5 even when input is high.
	highSlip := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.45, PGuess: 0.2}
	got, _, _ := BKTUpdateHeuristicSlipByErrorType(highSlip, false, "SYNTAX_ERROR")
	if got.PMastery < 0 || got.PMastery > 1 {
		t.Errorf("PMastery out of bounds: %f", got.PMastery)
	}
	// KNOWLEDGE_GAP: PGuess clamped to >= 0.05 even when input is low.
	lowGuess := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.06}
	got, _, _ = BKTUpdateHeuristicSlipByErrorType(lowGuess, false, "KNOWLEDGE_GAP")
	if got.PMastery < 0 || got.PMastery > 1 {
		t.Errorf("PMastery out of bounds: %f", got.PMastery)
	}
}

// TestBKTUpdateHeuristicSlipByErrorType_ReturnsSlipGuessUsed verifies the
// function reports the slip/guess values it actually fed into BKTUpdate so
// callers can log them to the audit trail (issue #51).
func TestBKTUpdateHeuristicSlipByErrorType_ReturnsSlipGuessUsed(t *testing.T) {
	base := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}

	// Correct answer: the heuristic does not adjust — we should see the
	// input slip/guess unchanged.
	_, slip, guess := BKTUpdateHeuristicSlipByErrorType(base, true, "SYNTAX_ERROR")
	if !approxEqual(slip, base.PSlip, 1e-9) || !approxEqual(guess, base.PGuess, 1e-9) {
		t.Errorf("correct answer should report base params, got slip=%f guess=%f", slip, guess)
	}

	// Empty errorType: standard BKT — base params reported.
	_, slip, guess = BKTUpdateHeuristicSlipByErrorType(base, false, "")
	if !approxEqual(slip, base.PSlip, 1e-9) || !approxEqual(guess, base.PGuess, 1e-9) {
		t.Errorf("empty errorType should report base params, got slip=%f guess=%f", slip, guess)
	}

	// SYNTAX_ERROR: slip ramps up by 0.15 (clamped).
	_, slip, guess = BKTUpdateHeuristicSlipByErrorType(base, false, "SYNTAX_ERROR")
	if !approxEqual(slip, 0.25, 1e-9) {
		t.Errorf("SYNTAX_ERROR should ramp slip to 0.25, got %f", slip)
	}
	if !approxEqual(guess, base.PGuess, 1e-9) {
		t.Errorf("SYNTAX_ERROR should leave guess at base, got %f", guess)
	}

	// KNOWLEDGE_GAP: guess ramps down by 0.10 (clamped).
	_, slip, guess = BKTUpdateHeuristicSlipByErrorType(base, false, "KNOWLEDGE_GAP")
	if !approxEqual(slip, base.PSlip, 1e-9) {
		t.Errorf("KNOWLEDGE_GAP should leave slip at base, got %f", slip)
	}
	if !approxEqual(guess, 0.10, 1e-9) {
		t.Errorf("KNOWLEDGE_GAP should ramp guess to 0.10, got %f", guess)
	}
}

// Compile-time guard: the public symbol exposed to call-sites must be the new
// "Heuristic" name. If someone reverts the rename this test file refuses to
// build — that is the point (issue #51).
var _ = BKTUpdateHeuristicSlipByErrorType
