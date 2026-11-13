// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package algorithms

import (
	"fmt"
	"math"
	"testing"
)

func TestBKTObserveSeparatesEvidenceFromTransition(t *testing.T) {
	base := BKTState{PMastery: 0.1, PLearn: 0.15, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}
	for _, correct := range []bool{false, true} {
		t.Run(fmt.Sprint(correct), func(t *testing.T) {
			want := 1.0 / 73 // P(known | incorrect) = .01 / (.01 + .72)
			if correct {
				want = 1.0 / 3 // P(known | correct) = .09 / (.09 + .18)
			}
			posterior := BKTObserve(base, correct)
			if math.Abs(posterior.PMastery-want) > 1e-12 {
				t.Fatalf("posterior = %.15f, want %.15f", posterior.PMastery, want)
			}
			unchanged := posterior
			unchanged.PMastery = base.PMastery
			if unchanged != base {
				t.Fatalf("observation changed model parameters: %+v", posterior)
			}
			for _, learn := range []float64{0, 0.15, 1} {
				for _, forget := range []float64{0, 0.05, 1} {
					other := base
					other.PLearn, other.PForget = learn, forget
					if got := BKTObserve(other, correct); got.PMastery != posterior.PMastery {
						t.Fatalf("transition parameters affected observation: %+v", got)
					}
				}
			}
			wantNext := want*0.95 + (1-want)*0.15
			if got := BKTTransition(posterior); math.Abs(got.PMastery-wantNext) > 1e-12 || got != BKTTransition(BKTObserve(base, correct)) {
				t.Fatalf("legacy update no longer composes observation + transition: %+v", got)
			}
		})
	}
}

// Learning-integrity release gates 1 and 2: one observation cannot saturate
// the estimate, and alternating outcomes on learning activities (observation
// plus transition) stay bounded away from 0 and 1.
func TestBKTObserveNeverSaturates(t *testing.T) {
	base := BKTState{PMastery: 0.5, PSlip: 0.1, PGuess: 0.2}
	for _, correct := range []bool{false, true} {
		if p := BKTObserve(base, correct).PMastery; p <= 0.05 || p >= 0.95 {
			t.Fatalf("one answer (correct=%v) saturated the estimate: %v", correct, p)
		}
	}
	state := base
	state.PLearn, state.PForget = 0.15, 0.05
	for i := range 40 {
		state = BKTTransition(BKTObserve(state, i%2 == 0))
		if state.PMastery <= 0.05 || state.PMastery >= 0.95 {
			t.Fatalf("alternating outcomes saturated the estimate at step %d: %v", i, state.PMastery)
		}
	}
}
