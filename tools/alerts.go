// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"

	"tutor-mcp/engine"
	"tutor-mcp/models"
)

// learnerMetacognitiveAlerts computes the learner-level metacognitive alerts
// (DEPENDENCY_INCREASING, CALIBRATION_DIVERGING, AFFECT_NEGATIVE,
// TRANSFER_BLOCKED) from the already loaded affect history and the concept
// states and interactions of the domains in view. A failed source read is
// returned, never reported as "no alert".
func learnerMetacognitiveAlerts(ctx context.Context, deps *Deps, learnerID string, affects []*models.AffectState, states []*models.ConceptState, interactions []*models.Interaction) ([]models.Alert, error) {
	var autonomyScores []float64
	for _, a := range affects {
		autonomyScores = append(autonomyScores, a.AutonomyScore)
	}
	calibBias, err := deps.Store.GetCalibrationBias(ctx, learnerID, 20)
	if err != nil {
		return nil, err
	}
	calibHistory, err := deps.Store.GetCalibrationBiasHistory(ctx, learnerID, 20)
	if err != nil {
		return nil, err
	}
	transfers, err := deps.Store.GetTransferRecordsByLearner(ctx, learnerID)
	if err != nil {
		return nil, err
	}
	return engine.ComputeMetacognitiveAlerts(
		autonomyScores,
		calibBias,
		affects,
		interactions,
		engine.WithTransferData(states, transfers),
		engine.WithCalibrationEvidence(len(calibHistory)),
	), nil
}

// mergeMetacognitiveAlerts appends meta alerts to the activity-level alert
// list while deduping on (Type, Concept). The Type is the alert kind
// (DEPENDENCY_INCREASING, CALIBRATION_DIVERGING, AFFECT_NEGATIVE,
// TRANSFER_BLOCKED) and Concept disambiguates per-concept TRANSFER_BLOCKED
// entries (the other three are learner-wide and carry an empty concept).
// This guards against double-emit if ComputeAlerts ever starts producing
// the same kinds, and against duplicates if this function is called twice
// in the same payload assembly.
func mergeMetacognitiveAlerts(base, extra []models.Alert) []models.Alert {
	seen := make(map[string]bool, len(base))
	for _, a := range base {
		seen[string(a.Type)+"|"+a.Concept] = true
	}
	for _, a := range extra {
		key := string(a.Type) + "|" + a.Concept
		if seen[key] {
			continue
		}
		seen[key] = true
		base = append(base, a)
	}
	return base
}
