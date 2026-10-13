// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// updateBKTWithFixedParameters changes mastery without adjusting the supplied
// parameters. A measurement applies no modeled learning/forgetting transition;
// a learning opportunity uses the parameters declared by the current policy.
func updateBKTWithFixedParameters(state algorithms.BKTState, mode models.BKTUpdateMode, success bool) algorithms.IndividualBKTUpdateResult {
	posterior := algorithms.BKTObserve(state, success)
	result := algorithms.IndividualBKTUpdateResult{
		State:            posterior,
		PosteriorMastery: posterior.PMastery,
		Params: algorithms.IndividualBKTParameters{
			PSlip:  state.PSlip,
			PGuess: state.PGuess,
		},
	}
	if mode == models.BKTLearningOpportunity {
		result.State = algorithms.BKTTransition(posterior)
		result.Params.PLearn = state.PLearn
		result.Params.PForget = state.PForget
		result.TransitionApplied = true
	}
	return result
}

// estimateIndependentBKTParameters uses only admissible observations and the
// immutable reference. In particular, neither the applied collective weight
// nor the learner's mastery/FSRS state can become an estimation input. This is
// the existing deterministic individual heuristic, not a statistical fit.
func estimateIndependentBKTParameters(interactions []*models.Interaction, success bool, errorType string) (models.BKTParams, algorithms.IndividualBKTProfile) {
	ref := models.BKTReferenceParams()
	profile := buildIndividualBKTProfile(interactions, 0)
	result := algorithms.BKTObserveIndividualized(algorithms.BKTState{
		PLearn: ref.PLearn, PForget: ref.PForget, PSlip: ref.PSlip, PGuess: ref.PGuess,
	}, profile, success, errorType)
	// Observation-only Params intentionally has zero transition probabilities;
	// State retains the estimated parameters for the independent source.
	return models.BKTParams{
		PLearn: result.State.PLearn, PForget: result.State.PForget,
		PSlip: result.State.PSlip, PGuess: result.State.PGuess,
	}, profile
}

func stateBKTParams(state *models.ConceptState) models.BKTParams {
	return models.BKTParams{PLearn: state.PLearn, PForget: state.PForget, PSlip: state.PSlip, PGuess: state.PGuess}
}

func setStateBKTParams(state *models.ConceptState, params models.BKTParams) {
	state.PLearn, state.PForget, state.PSlip, state.PGuess = params.PLearn, params.PForget, params.PSlip, params.PGuess
}
