// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import "math"

const (
	individualBKTMaxSlip  = 0.5
	individualBKTMaxGuess = 0.5
)

// Delta coefficients for the individualized BKT adjustment. Each per-signal
// weight is the contribution (per unit of evidence weight) that a fully
// saturated signal makes to a probability before it is clamped. The values
// were hand-tuned so a maxed-out single signal nudges a parameter by a few
// hundredths — deliberately gentle so individualization refines rather than
// overrides the base BKT update. See docs/regulation-design for provenance.
const (
	// individualBKTLearnStableCoeff rewards P(L) when the learner shows strong,
	// confident success (strongStable).
	individualBKTLearnStableCoeff = 0.06
	// individualBKTLearnStabilityCoeff rewards P(L) for response stability.
	individualBKTLearnStabilityCoeff = 0.04
	// individualBKTLearnErrorCoeff penalizes P(L) for the error signal.
	individualBKTLearnErrorCoeff = 0.07
	// individualBKTLearnHintsCoeff penalizes P(L) for hint reliance.
	individualBKTLearnHintsCoeff = 0.05
	// individualBKTLearnOverconfCoeff penalizes P(L) for overconfidence.
	individualBKTLearnOverconfCoeff = 0.06

	// individualBKTSlipStableCoeff lowers P(slip) when success is strong/stable.
	individualBKTSlipStableCoeff = 0.06
	// individualBKTSlipStabilityCoeff lowers P(slip) for response stability.
	individualBKTSlipStabilityCoeff = 0.05
	// individualBKTSlipErrorCoeff raises P(slip) for the error signal.
	individualBKTSlipErrorCoeff = 0.06
	// individualBKTSlipHintsCoeff raises P(slip) for hint reliance.
	individualBKTSlipHintsCoeff = 0.04
	// individualBKTSlipOverconfCoeff raises P(slip) for overconfidence.
	individualBKTSlipOverconfCoeff = 0.05

	// individualBKTGuessStableCoeff lowers P(guess) when success is strong.
	individualBKTGuessStableCoeff = 0.04
	// individualBKTGuessStabilityCoeff lowers P(guess) for response stability.
	individualBKTGuessStabilityCoeff = 0.02
	// individualBKTGuessErrorCoeff raises P(guess) for the error signal.
	individualBKTGuessErrorCoeff = 0.04
	// individualBKTGuessHintsCoeff raises P(guess) for hint reliance.
	individualBKTGuessHintsCoeff = 0.06
	// individualBKTGuessOverconfCoeff raises P(guess) for overconfidence.
	individualBKTGuessOverconfCoeff = 0.05

	// individualBKTOverconfidenceScale converts the margin of avg confidence
	// above 0.75 into an overconfidence proxy: a learner 0.25 above the
	// threshold (i.e. fully confident) saturates the [0,1] proxy.
	individualBKTOverconfidenceScale = 4
	// individualBKTConfidenceThreshold is the avg-confidence level above which
	// confidence starts counting as overconfidence.
	individualBKTConfidenceThreshold = 0.75

	// individualBKTEvidenceRamp is the observation count at which evidence
	// weight saturates to 1; below it, weight ramps linearly as obs/ramp.
	individualBKTEvidenceRamp = 20
)

// IndividualBKTProfile summarizes learner/concept observations used to tune a
// single BKT update. All rates are expected in [0, 1]; invalid values are
// treated defensively so this layer remains pure and deterministic.
type IndividualBKTProfile struct {
	Observations        int
	SuccessRate         float64
	ErrorRate           float64
	AvgConfidence       float64
	HintsRate           float64
	OverconfidenceRate  float64
	Stability           float64
	AvgResponseTimeSecs float64
}

// IndividualBKTParameters reports the BKT parameters effectively used for one
// individualized update.
type IndividualBKTParameters struct {
	PLearn  float64
	PForget float64
	PSlip   float64
	PGuess  float64
}

// IndividualBKTUpdateResult exposes the observation posterior separately from
// the optional transition, along with the parameters effectively applied.
type IndividualBKTUpdateResult struct {
	State             BKTState
	Params            IndividualBKTParameters
	PosteriorMastery  float64
	TransitionApplied bool
}

// BKTUpdateIndividualized applies a deterministic learner/concept adjustment
// before delegating to the standard BKT update. An empty profile keeps the
// existing BKT behaviour, including the current error-type heuristic.
func BKTUpdateIndividualized(state BKTState, profile IndividualBKTProfile, correct bool, errorType string) IndividualBKTUpdateResult {
	result := BKTObserveIndividualized(state, profile, correct, errorType)
	result.Params.PLearn = result.State.PLearn
	result.Params.PForget = result.State.PForget
	result.State = BKTTransition(result.State)
	result.TransitionApplied = true
	return result
}

// BKTObserveIndividualized uses the same observation parameters as a learning
// update, but records only the posterior. Effective transition parameters are
// zero; the state's adjusted transition parameters are retained, not erased.
func BKTObserveIndividualized(state BKTState, profile IndividualBKTProfile, correct bool, errorType string) IndividualBKTUpdateResult {
	adjusted := sanitizeBKTProbabilities(state)
	weight := individualBKTEvidenceWeight(profile)

	if weight > 0 {
		signals := sanitizeIndividualBKTSignals(profile)
		strongStable := signals.success * (0.75 + 0.25*signals.confidence)
		overconfidence := math.Max(signals.overconfidence, clamp((signals.confidence-individualBKTConfidenceThreshold)*individualBKTOverconfidenceScale, 0, 1)*signals.error)

		learnDelta := weight * (individualBKTLearnStableCoeff*strongStable + individualBKTLearnStabilityCoeff*signals.stability - individualBKTLearnErrorCoeff*signals.error - individualBKTLearnHintsCoeff*signals.hints - individualBKTLearnOverconfCoeff*overconfidence)
		slipDelta := weight * (-individualBKTSlipStableCoeff*strongStable - individualBKTSlipStabilityCoeff*signals.stability + individualBKTSlipErrorCoeff*signals.error + individualBKTSlipHintsCoeff*signals.hints + individualBKTSlipOverconfCoeff*overconfidence)
		guessDelta := weight * (-individualBKTGuessStableCoeff*strongStable - individualBKTGuessStabilityCoeff*signals.stability + individualBKTGuessErrorCoeff*signals.error + individualBKTGuessHintsCoeff*signals.hints + individualBKTGuessOverconfCoeff*overconfidence)

		adjusted.PLearn = finiteClamp(adjusted.PLearn+learnDelta, 0, 1)
		adjusted.PSlip = finiteClamp(adjusted.PSlip+slipDelta, 0, individualBKTMaxSlip)
		adjusted.PGuess = finiteClamp(adjusted.PGuess+guessDelta, 0, individualBKTMaxGuess)
	}

	if !correct {
		switch errorType {
		case "SYNTAX_ERROR":
			adjusted.PSlip = finiteClamp(adjusted.PSlip+0.15, 0, individualBKTMaxSlip)
		case "KNOWLEDGE_GAP":
			adjusted.PGuess = finiteClamp(adjusted.PGuess-0.10, 0.05, individualBKTMaxGuess)
			adjusted.PLearn = finiteClamp(adjusted.PLearn-0.03*weight, 0, 1)
		case "LOGIC_ERROR":
			adjusted.PSlip = finiteClamp(adjusted.PSlip+0.03*weight, 0, individualBKTMaxSlip)
		}
	}

	next := BKTObserve(adjusted, correct)
	next = sanitizeBKTProbabilities(next)

	return IndividualBKTUpdateResult{
		State:            next,
		PosteriorMastery: next.PMastery,
		Params: IndividualBKTParameters{
			PLearn:  0,
			PForget: 0,
			PSlip:   adjusted.PSlip,
			PGuess:  adjusted.PGuess,
		},
	}
}

type individualBKTSignals struct {
	success        float64
	error          float64
	confidence     float64
	hints          float64
	overconfidence float64
	stability      float64
}

func sanitizeIndividualBKTSignals(profile IndividualBKTProfile) individualBKTSignals {
	success := finiteClamp(profile.SuccessRate, 0, 1)
	errorRate := finiteClamp(profile.ErrorRate, 0, 1)
	if profile.Observations > 0 && profile.ErrorRate == 0 {
		errorRate = 1 - success
	}

	confidence := finiteClamp(profile.AvgConfidence, 0, 1)
	if !isFinite(profile.AvgConfidence) || profile.AvgConfidence == 0 {
		confidence = 0.5
	}

	return individualBKTSignals{
		success:        success,
		error:          errorRate,
		confidence:     confidence,
		hints:          finiteClamp(profile.HintsRate, 0, 1),
		overconfidence: finiteClamp(profile.OverconfidenceRate, 0, 1),
		stability:      finiteClamp(profile.Stability, 0, 1),
	}
}

func individualBKTEvidenceWeight(profile IndividualBKTProfile) float64 {
	if profile.Observations > 0 {
		return finiteClamp(float64(profile.Observations)/individualBKTEvidenceRamp, 0, 1)
	}
	if profile.SuccessRate != 0 ||
		profile.ErrorRate != 0 ||
		profile.AvgConfidence != 0 ||
		profile.HintsRate != 0 ||
		profile.OverconfidenceRate != 0 ||
		profile.Stability != 0 ||
		profile.AvgResponseTimeSecs != 0 {
		return 0.5
	}
	return 0
}

func sanitizeBKTProbabilities(state BKTState) BKTState {
	return BKTState{
		PMastery: finiteClamp(state.PMastery, 0, 1),
		PLearn:   finiteClamp(state.PLearn, 0, 1),
		PForget:  finiteClamp(state.PForget, 0, 1),
		PSlip:    finiteClamp(state.PSlip, 0, 1),
		PGuess:   finiteClamp(state.PGuess, 0, 1),
	}
}

func finiteClamp(v, min, max float64) float64 {
	if !isFinite(v) {
		return min
	}
	return clamp(v, min, max)
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
