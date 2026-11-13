// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import "testing"

func TestMasteryThresholdsAreUnified(t *testing.T) {
	for name, got := range map[string]float64{"BKT": MasteryBKT(), "KST": MasteryKST(), "Mid": MasteryMid()} {
		if got != 0.85 {
			t.Errorf("Mastery%s: want 0.85, got %v", name, got)
		}
	}
}

// TestMasteryThresholdsIgnoreEnvironment pins the removal of the former
// runtime opt-out: the environment never changes a threshold.
func TestMasteryThresholdsIgnoreEnvironment(t *testing.T) {
	t.Setenv("REGULATION_THRESHOLD", "off")
	if MasteryKST() != MasteryThreshold || MasteryMid() != MasteryThreshold {
		t.Fatalf("thresholds changed with the environment: KST=%v Mid=%v", MasteryKST(), MasteryMid())
	}
}

func TestRetentionThresholds_NamedForgettingBands(t *testing.T) {
	if got := RetentionAlertWarningThreshold; got != 0.40 {
		t.Errorf("RetentionAlertWarningThreshold: want 0.40, got %v", got)
	}
	if got := RetentionAlertCriticalThreshold; got != 0.30 {
		t.Errorf("RetentionAlertCriticalThreshold: want 0.30, got %v", got)
	}
	if got := RetentionRecallRoutingThreshold; got != 0.50 {
		t.Errorf("RetentionRecallRoutingThreshold: want 0.50, got %v", got)
	}
	if !(RetentionAlertCriticalThreshold < RetentionAlertWarningThreshold &&
		RetentionAlertWarningThreshold < RetentionRecallRoutingThreshold) {
		t.Fatalf(
			"retention thresholds must be ordered critical < warning < routing, got critical=%v warning=%v routing=%v",
			RetentionAlertCriticalThreshold,
			RetentionAlertWarningThreshold,
			RetentionRecallRoutingThreshold,
		)
	}
}
