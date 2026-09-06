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
// MasteryKST, MasteryMid). The bascule REGULATION_THRESHOLD=on collapses
// them to a single 0.85 — see docs/regulation-design/07-threshold-resolver.md.

// BKTUpdate preserves the traditional observation-plus-transition update for
// an opportunity to learn. Measurement-only callers must use BKTObserve.
func BKTUpdate(state BKTState, correct bool) BKTState {
	return BKTTransition(BKTObserve(state, correct))
}

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

// BKTUpdateHeuristicSlipByErrorType adjusts BKT slip/guess parameters based on
// the error type before applying a standard BKT update. NOTE: this is a
// PROJECT-SPECIFIC HEURISTIC, not part of canonical BKT (Corbett & Anderson
// 1995, "Knowledge Tracing: Modeling the Acquisition of Procedural
// Knowledge"). The literature treats slip and guess as fixed per-skill
// parameters; ramping them per-observation by an externally-supplied error
// label has no source in the BKT corpus and is a heuristic introduced here.
// The function is named to keep the deviation explicit at every call-site.
//
// SYNTAX_ERROR: careless mistake — higher slip, less penalizing to mastery.
// KNOWLEDGE_GAP: genuine lack of understanding — lower guess, more penalizing.
// LOGIC_ERROR or empty: standard BKT update (no heuristic ramp).
//
// The returned (slipUsed, guessUsed) values are the parameters the heuristic
// fed into BKTUpdate for this observation; callers should log them to the
// interaction audit trail so the run can be replayed deterministically.
func BKTUpdateHeuristicSlipByErrorType(state BKTState, correct bool, errorType string) (next BKTState, slipUsed, guessUsed float64) {
	if correct || errorType == "" {
		return BKTUpdate(state, correct), state.PSlip, state.PGuess
	}

	adjusted := state
	switch errorType {
	case "SYNTAX_ERROR":
		// Syntax errors indicate carelessness, not lack of knowledge.
		// Temporarily boost PSlip to reduce mastery penalty.
		adjusted.PSlip = clamp(state.PSlip+0.15, 0, 0.5)
	case "KNOWLEDGE_GAP":
		// Genuine knowledge gap — reduce PGuess to penalize more.
		adjusted.PGuess = clamp(state.PGuess-0.10, 0.05, 0.5)
	}
	// LOGIC_ERROR uses standard parameters

	return BKTUpdate(adjusted, correct), adjusted.PSlip, adjusted.PGuess
}

func BKTIsMastered(state BKTState) bool {
	return state.PMastery >= MasteryBKT()
}
