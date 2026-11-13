// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

type BKTState struct {
	PMastery float64
	PLearn   float64
	PForget  float64
	PSlip    float64
	PGuess   float64
}

// bktEpsilon is the minimum allowed marginal probability used to clamp the
// Bayesian denominator in BKTObserve. With degenerate inputs (e.g.
// PMastery=0 and PGuess=0 on a "correct" observation), pCorrect collapses to
// zero and the 0/0 division yields NaN, poisoning every downstream consumer.
// Clamping to a small positive number preserves the standard update path for
// sane inputs and short-circuits the NaN without behaviour change.
const bktEpsilon = 1e-9

// Mastery thresholds are exposed via accessors in thresholds.go (MasteryBKT,
// MasteryKST, MasteryMid), all equal to the unified MasteryThreshold (0.85).

// BKTObserve updates the posterior from the response alone. PLearn and PForget
// are preserved as parameters but do not affect this measurement. Separating
// observation and state transition follows the KT inference model; omitting a
// transition for assessment is the runtime's explicit measurement policy.
// Reference: https://www.cs.cmu.edu/~listen/BNT-SM/kt.html
func BKTObserve(state BKTState, correct bool) BKTState {
	var pMasteryGivenObs float64
	if correct {
		pCorrectMastery := 1.0 - state.PSlip
		pCorrectNotMastery := state.PGuess
		pCorrect := pCorrectMastery*state.PMastery + pCorrectNotMastery*(1-state.PMastery)
		// Guard against pCorrect==0 (e.g. PMastery=0 ∧ PGuess=0) which would
		// produce a NaN. This numerical fallback is not a uniquely defined
		// Bayesian posterior for an impossible observation.
		if pCorrect < bktEpsilon {
			pCorrect = bktEpsilon
		}
		pMasteryGivenObs = pCorrectMastery * state.PMastery / pCorrect
	} else {
		pIncorrectMastery := state.PSlip
		pIncorrectNotMastery := 1.0 - state.PGuess
		pIncorrect := pIncorrectMastery*state.PMastery + pIncorrectNotMastery*(1-state.PMastery)
		// Same guard as above for the incorrect branch (e.g. PMastery=0 ∧
		// PGuess=1, or PMastery=1 ∧ PSlip=0).
		if pIncorrect < bktEpsilon {
			pIncorrect = bktEpsilon
		}
		pMasteryGivenObs = pIncorrectMastery * state.PMastery / pIncorrect
	}
	result := state
	result.PMastery = clamp(pMasteryGivenObs, 0, 1)
	return result
}

// BKTTransition applies one modeled learning opportunity to an already updated
// posterior. PForget is a per-transition probability here, not elapsed-time
// forgetting. A response-only observation must apply neither transition term.
func BKTTransition(state BKTState) BKTState {
	result := state
	result.PMastery = clamp(state.PMastery*(1-state.PForget)+(1-state.PMastery)*state.PLearn, 0, 1)
	return result
}

func BKTIsMastered(state BKTState) bool {
	return state.PMastery >= MasteryBKT()
}
