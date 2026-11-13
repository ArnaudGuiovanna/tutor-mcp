package algorithms

import (
	"math"
	"testing"
)

func TestBKTObserveIndividualizedEmptyProfileUsesErrorTypeObservationParams(t *testing.T) {
	base := BKTState{PMastery: 0.5, PLearn: 0.3, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}

	tests := []struct {
		name      string
		correct   bool
		errorType string
		wantSlip  float64
		wantGuess float64
	}{
		{name: "correct", correct: true, errorType: "SYNTAX_ERROR", wantSlip: 0.1, wantGuess: 0.2},
		{name: "incorrect", correct: false, errorType: "", wantSlip: 0.1, wantGuess: 0.2},
		{name: "syntax error", correct: false, errorType: "SYNTAX_ERROR", wantSlip: 0.25, wantGuess: 0.2},
		{name: "knowledge gap", correct: false, errorType: "KNOWLEDGE_GAP", wantSlip: 0.1, wantGuess: 0.1},
		{name: "logic error", correct: false, errorType: "LOGIC_ERROR", wantSlip: 0.1, wantGuess: 0.2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BKTObserveIndividualized(base, IndividualBKTProfile{}, tc.correct, tc.errorType)
			if !approxEqual(got.Params.PSlip, tc.wantSlip, 1e-9) || !approxEqual(got.Params.PGuess, tc.wantGuess, 1e-9) {
				t.Fatalf("params = %+v, want slip=%f guess=%f", got.Params, tc.wantSlip, tc.wantGuess)
			}
			adjusted := base
			adjusted.PSlip, adjusted.PGuess = tc.wantSlip, tc.wantGuess
			want := BKTObserve(adjusted, tc.correct).PMastery
			if !approxEqual(got.State.PMastery, want, 1e-9) {
				t.Fatalf("PMastery = %f, want %f", got.State.PMastery, want)
			}
			if !approxEqual(got.State.PLearn, base.PLearn, 1e-9) || got.TransitionApplied {
				t.Fatalf("empty profile changed learning parameters or applied a transition: %+v", got)
			}
		})
	}
}

func TestBKTObserveIndividualizedContrastingProfiles(t *testing.T) {
	base := BKTState{PMastery: 0.45, PLearn: 0.25, PForget: 0.05, PSlip: 0.12, PGuess: 0.2}
	strong := IndividualBKTProfile{
		Observations:       50,
		SuccessRate:        0.92,
		ErrorRate:          0.04,
		AvgConfidence:      0.85,
		HintsRate:          0.02,
		OverconfidenceRate: 0.01,
		Stability:          0.9,
	}
	fragile := IndividualBKTProfile{
		Observations:       50,
		SuccessRate:        0.35,
		ErrorRate:          0.65,
		AvgConfidence:      0.9,
		HintsRate:          0.55,
		OverconfidenceRate: 0.6,
		Stability:          0.1,
	}

	gotStrong := BKTObserveIndividualized(base, strong, true, "")
	gotFragile := BKTObserveIndividualized(base, fragile, true, "")

	if gotStrong.Params.PSlip >= base.PSlip {
		t.Errorf("strong stable profile should lower slip: got %f base %f", gotStrong.Params.PSlip, base.PSlip)
	}
	if gotStrong.State.PLearn <= base.PLearn {
		t.Errorf("strong stable profile should raise learn cautiously: got %f base %f", gotStrong.State.PLearn, base.PLearn)
	}
	if gotStrong.Params.PGuess >= base.PGuess {
		t.Errorf("strong stable profile should lower guess: got %f base %f", gotStrong.Params.PGuess, base.PGuess)
	}
	if gotFragile.Params.PSlip <= base.PSlip {
		t.Errorf("fragile profile should raise slip: got %f base %f", gotFragile.Params.PSlip, base.PSlip)
	}
	if gotFragile.State.PLearn >= base.PLearn {
		t.Errorf("fragile profile should lower learn: got %f base %f", gotFragile.State.PLearn, base.PLearn)
	}
	if gotFragile.Params.PGuess <= base.PGuess {
		t.Errorf("fragile profile should raise guess: got %f base %f", gotFragile.Params.PGuess, base.PGuess)
	}
	if gotStrong.State.PMastery <= gotFragile.State.PMastery {
		t.Errorf("strong profile should produce higher mastery on same correct answer: strong=%f fragile=%f",
			gotStrong.State.PMastery, gotFragile.State.PMastery)
	}
}

func TestBKTObserveIndividualizedClampsAndAvoidsNaN(t *testing.T) {
	state := BKTState{
		PMastery: math.NaN(),
		PLearn:   math.Inf(1),
		PForget:  math.Inf(-1),
		PSlip:    2,
		PGuess:   -1,
	}
	profile := IndividualBKTProfile{
		Observations:        1000,
		SuccessRate:         math.Inf(1),
		ErrorRate:           math.NaN(),
		AvgConfidence:       math.Inf(-1),
		HintsRate:           2,
		OverconfidenceRate:  2,
		Stability:           2,
		AvgResponseTimeSecs: math.NaN(),
	}

	got := BKTObserveIndividualized(state, profile, false, "KNOWLEDGE_GAP")

	assertFiniteBKTState(t, got.State)
	assertFiniteParams(t, got.Params)
	if got.State.PLearn < 0 || got.State.PLearn > 1 {
		t.Errorf("PLearn out of bounds: %f", got.State.PLearn)
	}
	if got.Params.PSlip < 0 || got.Params.PSlip > individualBKTMaxSlip {
		t.Errorf("PSlip out of bounds: %f", got.Params.PSlip)
	}
	if got.Params.PGuess < 0.05 || got.Params.PGuess > individualBKTMaxGuess {
		t.Errorf("PGuess out of bounds after KNOWLEDGE_GAP: %f", got.Params.PGuess)
	}
}

func TestBKTObserveIndividualizedDeterministic(t *testing.T) {
	state := BKTState{PMastery: 0.42, PLearn: 0.28, PForget: 0.04, PSlip: 0.11, PGuess: 0.21}
	profile := IndividualBKTProfile{
		Observations:       17,
		SuccessRate:        0.68,
		ErrorRate:          0.22,
		AvgConfidence:      0.74,
		HintsRate:          0.18,
		OverconfidenceRate: 0.08,
		Stability:          0.61,
	}

	first := BKTObserveIndividualized(state, profile, false, "LOGIC_ERROR")
	second := BKTObserveIndividualized(state, profile, false, "LOGIC_ERROR")

	if first != second {
		t.Fatalf("individualized BKT update is not deterministic: first=%+v second=%+v", first, second)
	}
}

func TestBKTObserveIndividualizedNoNaNOnDegenerateBKTInputs(t *testing.T) {
	tests := []struct {
		name    string
		state   BKTState
		correct bool
	}{
		{
			name:    "zero correct marginal",
			state:   BKTState{PMastery: 0, PLearn: 0.3, PForget: 0.05, PSlip: 0, PGuess: 0},
			correct: true,
		},
		{
			name:    "zero incorrect marginal",
			state:   BKTState{PMastery: 1, PLearn: 0.3, PForget: 0.05, PSlip: 0, PGuess: 1},
			correct: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BKTObserveIndividualized(tc.state, IndividualBKTProfile{}, tc.correct, "")
			assertFiniteBKTState(t, got.State)
			assertFiniteParams(t, got.Params)
		})
	}
}

func assertFiniteBKTState(t *testing.T, state BKTState) {
	t.Helper()
	if !isFinite(state.PMastery) || !isFinite(state.PLearn) || !isFinite(state.PForget) || !isFinite(state.PSlip) || !isFinite(state.PGuess) {
		t.Fatalf("state contains NaN/Inf: %+v", state)
	}
	if state.PMastery < 0 || state.PMastery > 1 {
		t.Fatalf("PMastery out of bounds: %f", state.PMastery)
	}
	if state.PLearn < 0 || state.PLearn > 1 {
		t.Fatalf("PLearn out of bounds: %f", state.PLearn)
	}
	if state.PForget < 0 || state.PForget > 1 {
		t.Fatalf("PForget out of bounds: %f", state.PForget)
	}
	if state.PSlip < 0 || state.PSlip > 1 {
		t.Fatalf("PSlip out of bounds: %f", state.PSlip)
	}
	if state.PGuess < 0 || state.PGuess > 1 {
		t.Fatalf("PGuess out of bounds: %f", state.PGuess)
	}
}

func assertFiniteParams(t *testing.T, params IndividualBKTParameters) {
	t.Helper()
	if !isFinite(params.PLearn) || !isFinite(params.PSlip) || !isFinite(params.PGuess) {
		t.Fatalf("params contain NaN/Inf: %+v", params)
	}
}
