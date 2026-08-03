// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import "math"

const (
	// Calibration deltas are normalized to [-1,1]. A mean absolute error of
	// 0.25 is large enough to warrant intervention without treating ordinary
	// item noise as a persistent learner pattern.
	calibrationActionableThreshold = 0.25
	// Require repeated observations before labelling a learner's calibration.
	calibrationMinimumSamples = 5
)

func calibrationBiasIsActionable(bias float64, samples int) bool {
	return samples >= calibrationMinimumSamples && math.Abs(bias) >= calibrationActionableThreshold
}
