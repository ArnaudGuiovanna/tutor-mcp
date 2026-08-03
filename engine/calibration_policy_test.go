// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import "testing"

func TestCalibrationBiasIsActionableRequiresMagnitudeAndEvidence(t *testing.T) {
	tests := []struct {
		name    string
		bias    float64
		samples int
		want    bool
	}{
		{name: "no evidence", bias: 1, samples: 0},
		{name: "too few samples", bias: 0.8, samples: calibrationMinimumSamples - 1},
		{name: "below magnitude", bias: 0.24, samples: calibrationMinimumSamples},
		{name: "positive actionable", bias: 0.25, samples: calibrationMinimumSamples, want: true},
		{name: "negative actionable", bias: -0.40, samples: 10, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := calibrationBiasIsActionable(tt.bias, tt.samples); got != tt.want {
				t.Fatalf("calibrationBiasIsActionable(%v,%d)=%v, want %v", tt.bias, tt.samples, got, tt.want)
			}
		})
	}
}
