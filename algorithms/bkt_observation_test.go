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
			if got := BKTTransition(posterior); math.Abs(got.PMastery-wantNext) > 1e-12 || got != BKTUpdate(base, correct) {
				t.Fatalf("legacy update no longer composes observation + transition: %+v", got)
			}
		})
	}
}

func TestBKTObserveIndividualizedSharesPosteriorWithoutTransition(t *testing.T) {
	base := BKTState{PMastery: 0.1, PLearn: 0.15, PForget: 0.05, PSlip: 0.1, PGuess: 0.2}
	profiles := []IndividualBKTProfile{
		{},
		{Observations: 12, SuccessRate: 0.7, ErrorRate: 0.3, AvgConfidence: 0.8, HintsRate: 0.2, Stability: 0.6},
	}
	for i, profile := range profiles {
		for _, correct := range []bool{false, true} {
			for _, errorType := range []string{"", "SYNTAX_ERROR", "KNOWLEDGE_GAP", "LOGIC_ERROR"} {
				t.Run(fmt.Sprintf("profile%d/%t/%s", i, correct, errorType), func(t *testing.T) {
					observed := BKTObserveIndividualized(base, profile, correct, errorType)
					learned := BKTUpdateIndividualized(base, profile, correct, errorType)
					if observed.TransitionApplied || observed.Params.PLearn != 0 || observed.Params.PForget != 0 {
						t.Fatalf("measurement applied a transition: %+v", observed)
					}
					if observed.State.PMastery != observed.PosteriorMastery || observed.PosteriorMastery != learned.PosteriorMastery {
						t.Fatalf("different observation inference: %+v vs %+v", observed, learned)
					}
					if observed.Params.PSlip != learned.Params.PSlip || observed.Params.PGuess != learned.Params.PGuess {
						t.Fatal("observation parameters differ by transition mode")
					}
					if observed.State.PLearn != learned.Params.PLearn || observed.State.PForget != base.PForget {
						t.Fatalf("measurement erased future transition parameters: %+v", observed)
					}
					if !learned.TransitionApplied || learned.State != BKTTransition(observed.State) {
						t.Fatalf("learning update is not exactly one transition: %+v", learned)
					}
				})
			}
		}
	}
}
