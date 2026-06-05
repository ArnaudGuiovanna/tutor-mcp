// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import "math"

const (
	pfaBetaSuccess = 0.11
	pfaBetaFailure = -0.11
)

type PFAState struct {
	Successes float64
	Failures  float64
}

func PFAUpdate(state PFAState, success bool) PFAState {
	result := state
	if success {
		result.Successes++
	} else {
		result.Failures++
	}
	return result
}

func PFAScore(state PFAState) float64 {
	return pfaBetaSuccess*state.Successes + pfaBetaFailure*state.Failures
}

// PFAProbability applies the logistic sigmoid to PFAScore, producing a
// proper probability in (0, 1). At extreme scores the sigmoid saturates,
// making consecutive deltas small — which is how PFADetectPlateau
// identifies genuine learning plateaus.
func PFAProbability(state PFAState) float64 {
	return 1.0 / (1.0 + math.Exp(-PFAScore(state)))
}

func PFADetectPlateau(recentScores []float64, minCount int) bool {
	if len(recentScores) < minCount {
		return false
	}
	scores := recentScores[len(recentScores)-minCount:]
	maxDelta := 0.0
	for i := 1; i < len(scores); i++ {
		delta := math.Abs(scores[i] - scores[i-1])
		if delta > maxDelta {
			maxDelta = delta
		}
	}
	return maxDelta < 0.025
}
