// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import "math"

type IRTItem struct {
	Difficulty     float64
	Discrimination float64
}

func IRTProbability(theta, difficulty, discrimination float64) float64 {
	return 1.0 / (1.0 + math.Exp(-discrimination*(theta-difficulty)))
}

func IRTUpdateTheta(theta float64, items []IRTItem, responses []bool) float64 {
	if len(items) == 0 || len(items) != len(responses) {
		return theta
	}
	for iter := 0; iter < 5; iter++ {
		var dL, d2L float64
		for i, item := range items {
			p := IRTProbability(theta, item.Difficulty, item.Discrimination)
			x := 0.0
			if responses[i] {
				x = 1.0
			}
			dL += item.Discrimination * (x - p)
			d2L -= item.Discrimination * item.Discrimination * p * (1 - p)
		}
		if d2L == 0 {
			break
		}
		step := dL / d2L
		theta -= step
		if math.Abs(step) < 0.001 {
			break
		}
	}
	return clamp(theta, -4, 4)
}

func IRTIsInZPD(pCorrect float64) bool {
	return pCorrect >= 0.55 && pCorrect <= 0.80
}

// FSRSDifficultyToIRT maps FSRS difficulty [1, 10] to IRT scale [-3, 3].
func FSRSDifficultyToIRT(fsrsDifficulty float64) float64 {
	return (fsrsDifficulty-1.0)*2.0/3.0 - 3.0
}
