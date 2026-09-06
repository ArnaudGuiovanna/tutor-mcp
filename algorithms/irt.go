// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import "math"

const (
	// irtBasePriorPrecision anchors the online estimate to the previously
	// persisted theta. Without a prior, the MLE for one binary response is at
	// +/-Inf, which made the old singleton update jump straight to the clamp.
	irtBasePriorPrecision = 1.0

	// A dichotomous 2PL item with discrimination 1 contributes at most 0.25
	// Fisher information. ConceptState does not persist posterior variance, so
	// prior response count is used as a regularization heuristic, not a
	// calibrated posterior precision. Runtime-generated tasks no longer use
	// this update because their item parameters have not been calibrated.
	irtInformationPerObservation = 0.25
	irtMaxNewtonStep             = 1.0
)

type IRTItem struct {
	Difficulty     float64
	Discrimination float64
}

func IRTProbability(theta, difficulty, discrimination float64) float64 {
	return 1.0 / (1.0 + math.Exp(-discrimination*(theta-difficulty)))
}

func IRTUpdateTheta(theta float64, items []IRTItem, responses []bool) float64 {
	return IRTUpdateThetaCumulative(theta, 0, items, responses)
}

// IRTUpdateThetaCumulative performs a regularized online 2PL update.
//
// theta is the posterior mode persisted after previous observations and
// priorObservations is their cumulative count. The update maximizes the new
// items' likelihood under a Gaussian prior centred on theta. This is a compact
// Laplace/MAP approximation: it needs no schema change, keeps prior evidence in
// the estimate, and—unlike an unregularized one-item MLE—has a finite optimum.
func IRTUpdateThetaCumulative(theta float64, priorObservations int, items []IRTItem, responses []bool) float64 {
	if len(items) == 0 || len(items) != len(responses) {
		return theta
	}
	if priorObservations < 0 {
		priorObservations = 0
	}
	priorMean := clamp(theta, -4, 4)
	priorPrecision := irtBasePriorPrecision + irtInformationPerObservation*float64(priorObservations)
	estimate := priorMean

	for iter := 0; iter < 8; iter++ {
		// Gaussian prior N(priorMean, 1/priorPrecision).
		dL := -priorPrecision * (estimate - priorMean)
		d2L := -priorPrecision
		for i, item := range items {
			if !isFiniteIRTItem(item) || item.Discrimination == 0 {
				continue
			}
			p := IRTProbability(estimate, item.Difficulty, item.Discrimination)
			x := 0.0
			if responses[i] {
				x = 1.0
			}
			dL += item.Discrimination * (x - p)
			d2L -= item.Discrimination * item.Discrimination * p * (1 - p)
		}
		if d2L == 0 || math.IsNaN(dL) || math.IsNaN(d2L) {
			break
		}
		step := dL / d2L
		// Bound each Newton step as an additional guard for extreme imported
		// item parameters. Normal data converges without reaching this bound.
		step = clamp(step, -irtMaxNewtonStep, irtMaxNewtonStep)
		estimate -= step
		estimate = clamp(estimate, -4, 4)
		if math.Abs(step) < 0.001 {
			break
		}
	}
	return clamp(estimate, -4, 4)
}

func isFiniteIRTItem(item IRTItem) bool {
	return !math.IsNaN(item.Difficulty) && !math.IsInf(item.Difficulty, 0) &&
		!math.IsNaN(item.Discrimination) && !math.IsInf(item.Discrimination, 0)
}

// IRTIsInZPD retains a legacy project-specific band for compatibility.
// This interval is not an empirically established zone of proximal development.
func IRTIsInZPD(pCorrect float64) bool {
	return pCorrect >= 0.55 && pCorrect <= 0.80
}

// FSRSDifficultyToIRT maps FSRS difficulty [1, 10] to IRT scale [-3, 3].
// Deprecated: FSRS difficulty describes a learner's memory of a concept, not
// item difficulty. This numerical mapping must not feed a live learner model
// or pedagogical decision. It remains only for compatibility with old replays.
func FSRSDifficultyToIRT(fsrsDifficulty float64) float64 {
	// FSRS difficulty is defined on [1,10]. Bootstrap/imported rows may be
	// outside that range; normalize them before mapping so one malformed card
	// cannot manufacture an extreme IRT item.
	fsrsDifficulty = clamp(fsrsDifficulty, 1, 10)
	return (fsrsDifficulty-1.0)*2.0/3.0 - 3.0
}
