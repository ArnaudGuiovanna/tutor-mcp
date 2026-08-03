// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"math"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

func buildIndividualBKTProfile(interactions []*models.Interaction, stability float64) algorithms.IndividualBKTProfile {
	if len(interactions) == 0 {
		return algorithms.IndividualBKTProfile{}
	}

	total := 0
	successes := 0
	hints := 0
	overconfidentFailures := 0
	confidenceSum := 0.0
	responseTimeSum := 0.0

	for _, interaction := range interactions {
		if interaction == nil {
			continue
		}
		total++
		if interaction.Success {
			successes++
		}
		if interaction.HintsRequested > 0 {
			hints++
		}
		if !interaction.Success && interaction.Confidence >= 0.75 {
			overconfidentFailures++
		}
		confidenceSum += clampUnit(interaction.Confidence)
		if interaction.ResponseTime > 0 {
			responseTimeSum += float64(interaction.ResponseTime)
		}
	}
	if total == 0 {
		return algorithms.IndividualBKTProfile{}
	}

	successRate := float64(successes) / float64(total)
	return algorithms.IndividualBKTProfile{
		Observations:        total,
		SuccessRate:         successRate,
		ErrorRate:           1 - successRate,
		AvgConfidence:       confidenceSum / float64(total),
		HintsRate:           float64(hints) / float64(total),
		OverconfidenceRate:  float64(overconfidentFailures) / float64(total),
		Stability:           clampUnit(stability / 10),
		AvgResponseTimeSecs: responseTimeSum / float64(total),
	}
}

func individualBKTProfileSnapshot(profile algorithms.IndividualBKTProfile) map[string]any {
	return map[string]any{
		"observations":           profile.Observations,
		"success_rate":           profile.SuccessRate,
		"error_rate":             profile.ErrorRate,
		"avg_confidence":         profile.AvgConfidence,
		"hints_rate":             profile.HintsRate,
		"overconfidence_rate":    profile.OverconfidenceRate,
		"stability":              profile.Stability,
		"avg_response_time_secs": profile.AvgResponseTimeSecs,
	}
}

func individualBKTParamsSnapshot(params algorithms.IndividualBKTParameters) map[string]any {
	return map[string]any{
		"p_learn": params.PLearn,
		"p_slip":  params.PSlip,
		"p_guess": params.PGuess,
	}
}

func mergeObservation(base map[string]any, extra map[string]any) map[string]any {
	if len(extra) == 0 {
		return base
	}
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range extra {
		base[k] = v
	}
	return base
}

func clampUnit(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, -1) {
		return 0
	}
	if math.IsInf(v, 1) {
		return 1
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
