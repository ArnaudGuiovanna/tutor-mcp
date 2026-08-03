// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"strings"
	"testing"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// TestEstimateReviewMinutesBranches confirms both branches of estimateReviewMinutes
// are exercised: high-lapse → 12 minutes, otherwise → 8 minutes.
func TestEstimateReviewMinutesBranches(t *testing.T) {
	cases := []struct {
		name string
		cs   *models.ConceptState
		want int
	}{
		{"few lapses", &models.ConceptState{Lapses: 1}, 8},
		{"many lapses", &models.ConceptState{Lapses: 5}, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateReviewMinutes(tc.cs); got != tc.want {
				t.Errorf("estimateReviewMinutes(%+v) = %d, want %d", tc.cs, got, tc.want)
			}
		})
	}

	// Indirectly exercised through ComputeAlerts → ensures the high-lapse
	// branch shows up in a recommended action when retention is low.
	states := []*models.ConceptState{
		{Concept: "X", Stability: 0.1, ElapsedDays: 30, PMastery: 0.3, Lapses: 5, CardState: "review",
			LastReview: ptrTime(time.Now().AddDate(0, 0, -30))},
	}
	alerts := ComputeAlerts(states, nil, time.Time{})
	for _, a := range alerts {
		if a.Type == models.AlertForgetting && !strings.Contains(a.RecommendedAction, "12") {
			t.Errorf("expected '12 minutes' in recommended action for high-lapse concept, got %q", a.RecommendedAction)
		}
	}
}

// TestComputeAlerts_ErrorTypeRecommendations covers the three error-type
// branches in the ZPD_DRIFT recommendation builder.
func TestComputeAlerts_ErrorTypeRecommendations(t *testing.T) {
	cases := []struct {
		name      string
		errorType string
		wantSub   string
	}{
		{"knowledge gap", "KNOWLEDGE_GAP", "conceptual gap"},
		{"logic error", "LOGIC_ERROR", "logic errors"},
		{"syntax error", "SYNTAX_ERROR", "syntax errors"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interactions := []*models.Interaction{
				{Concept: "X", Success: false, ErrorType: tc.errorType},
				{Concept: "X", Success: false, ErrorType: tc.errorType},
				{Concept: "X", Success: false, ErrorType: tc.errorType},
			}
			states := []*models.ConceptState{alertNonForgettingState("X", 0.3)}
			alerts := ComputeAlerts(states, interactions, time.Time{})

			found := false
			for _, a := range alerts {
				if a.Type == models.AlertZPDDrift && a.Concept == "X" {
					found = true
					if !strings.Contains(a.RecommendedAction, tc.wantSub) {
						t.Errorf("recommended action %q missing %q", a.RecommendedAction, tc.wantSub)
					}
				}
			}
			if !found {
				t.Error("expected ZPD_DRIFT alert")
			}
		})
	}
}

// TestComputeAlerts_OverloadOnLongSession covers the sessionStart > 45m branch.
func TestComputeAlerts_OverloadOnLongSession(t *testing.T) {
	start := time.Now().Add(-50 * time.Minute)
	alerts := ComputeAlerts(nil, nil, start)
	found := false
	for _, a := range alerts {
		if a.Type == models.AlertOverload {
			found = true
		}
	}
	if !found {
		t.Error("expected OVERLOAD alert when sessionStart > 45m ago")
	}
}

// TestComputeAlerts_NoOverloadOnShortSession covers the negative branch — a
// recent sessionStart does NOT produce an OVERLOAD alert.
func TestComputeAlerts_NoOverloadOnShortSession(t *testing.T) {
	start := time.Now().Add(-5 * time.Minute)
	alerts := ComputeAlerts(nil, nil, start)
	for _, a := range alerts {
		if a.Type == models.AlertOverload {
			t.Errorf("did not expect OVERLOAD with 5-minute session, got %+v", a)
		}
	}
}

// TestComputeAlerts_PlateauDetected uses a long run of successes so the PFA
// sigmoid saturates and the last 4 deltas all fall below 0.025.
func TestComputeAlerts_PlateauDetected(t *testing.T) {
	// With pfaBetaSuccess = 0.11, the sigmoid only saturates well after 20+
	// successes — by then deltas drop below the 0.025 plateau threshold.
	var interactions []*models.Interaction
	for i := 0; i < 30; i++ {
		interactions = append(interactions, &models.Interaction{Concept: "P", Success: true})
	}
	alerts := ComputeAlerts(nil, interactions, time.Time{})

	found := false
	for _, a := range alerts {
		if a.Type == models.AlertPlateau && a.Concept == "P" {
			found = true
		}
	}
	if !found {
		t.Error("expected PLATEAU alert after sustained successes")
	}
}

func TestComputeAlertsForgettingCriticalSuppressesPlateau(t *testing.T) {
	var interactions []*models.Interaction
	for i := 0; i < 30; i++ {
		interactions = append(interactions, &models.Interaction{Concept: "P", Success: true})
	}

	cases := []struct {
		name        string
		retention   float64
		wantPlateau bool
	}{
		{
			name:        "critical forgetting suppresses plateau",
			retention:   algorithms.RetentionAlertCriticalThreshold - 0.0001,
			wantPlateau: false,
		},
		{
			name:        "warning forgetting keeps plateau",
			retention:   algorithms.RetentionAlertWarningThreshold - 0.0001,
			wantPlateau: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states := []*models.ConceptState{alertStateAtRetention(t, "P", tc.retention)}
			alerts := ComputeAlerts(states, interactions, time.Time{})

			if _, found := findAlert(alerts, models.AlertForgetting, "P"); !found {
				t.Fatal("expected FORGETTING alert in plateau collision scenario")
			}
			_, gotPlateau := findAlert(alerts, models.AlertPlateau, "P")
			if gotPlateau != tc.wantPlateau {
				t.Fatalf("PLATEAU presence: got %v, want %v", gotPlateau, tc.wantPlateau)
			}
		})
	}
}

// TestComputeAlertsAt_DeterministicForgetting freezes now so the FORGETTING
// retention decay is computed from a known elapsed interval (now - LastReview)
// rather than the wall clock. With stability 0.1 and ~30 days elapsed,
// retrievability falls below the critical threshold and fires UrgencyCritical.
func TestComputeAlertsAt_DeterministicForgetting(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	lastReview := now.AddDate(0, 0, -30)
	states := []*models.ConceptState{
		{Concept: "X", Stability: 0.1, ElapsedDays: 0, PMastery: 0.3, CardState: "review",
			LastReview: &lastReview},
	}

	alerts := ComputeAlertsAt(states, nil, time.Time{}, now)

	a, found := findAlert(alerts, models.AlertForgetting, "X")
	if !found {
		t.Fatal("expected FORGETTING alert at 30 days elapsed")
	}
	if a.Urgency != models.UrgencyCritical {
		t.Errorf("expected UrgencyCritical at 30 days elapsed, got %v (retention=%.4f)", a.Urgency, a.Retention)
	}

	// A near-zero elapsed interval keeps retention high → no FORGETTING.
	recent := now.Add(-1 * time.Hour)
	statesRecent := []*models.ConceptState{
		{Concept: "X", Stability: 0.1, ElapsedDays: 0, PMastery: 0.3, CardState: "review",
			LastReview: &recent},
	}
	if _, found := findAlert(ComputeAlertsAt(statesRecent, nil, time.Time{}, now), models.AlertForgetting, "X"); found {
		t.Error("did not expect FORGETTING alert for a 1-hour-old review")
	}
}

// TestComputeAlertsAt_DeterministicOverload pins now so OVERLOAD depends only on
// now - sessionStart, with no wall-clock dependency.
func TestComputeAlertsAt_DeterministicOverload(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// 46 minutes elapsed → just past the 45-minute threshold → OVERLOAD.
	start := now.Add(-46 * time.Minute)
	if _, found := findAlert(ComputeAlertsAt(nil, nil, start, now), models.AlertOverload, ""); !found {
		t.Error("expected OVERLOAD at 46 minutes elapsed")
	}

	// 44 minutes elapsed → below threshold → no OVERLOAD.
	startShort := now.Add(-44 * time.Minute)
	if _, found := findAlert(ComputeAlertsAt(nil, nil, startShort, now), models.AlertOverload, ""); found {
		t.Error("did not expect OVERLOAD at 44 minutes elapsed")
	}

	// Zero sessionStart → never OVERLOAD.
	if _, found := findAlert(ComputeAlertsAt(nil, nil, time.Time{}, now), models.AlertOverload, ""); found {
		t.Error("did not expect OVERLOAD with a zero sessionStart")
	}
}

// TestComputeMetacognitiveAlerts_DifficultyBranch covers the second
// AFFECT_NEGATIVE branch (perceived_difficulty == 1 on two consecutives).
func TestComputeMetacognitiveAlerts_DifficultyBranch(t *testing.T) {
	affects := []*models.AffectState{
		{PerceivedDifficulty: 1, Satisfaction: 4},
		{PerceivedDifficulty: 1, Satisfaction: 4},
	}
	alerts := ComputeMetacognitiveAlerts(nil, 0, affects, nil)
	for _, a := range alerts {
		if a.Type == models.AlertAffectNegative {
			return
		}
	}
	t.Error("expected AFFECT_NEGATIVE alert from difficulty=1 branch")
}

// TestComputeMetacognitiveAlerts_NegCalibration covers the bias < 0 branch.
func TestComputeMetacognitiveAlerts_NegCalibration(t *testing.T) {
	alerts := ComputeMetacognitiveAlerts(nil, -0.3, nil, nil, WithCalibrationEvidence(5))
	for _, a := range alerts {
		if a.Type == models.AlertCalibrationDiverging {
			if !strings.Contains(a.RecommendedAction, "sous-estimation") {
				t.Errorf("expected 'sous-estimation' in action, got %q", a.RecommendedAction)
			}
			return
		}
	}
	t.Error("expected CALIBRATION_DIVERGING alert with negative bias")
}

// TestComputeMetacognitiveAlerts_TransferBlockedNoData ensures the
// transfer-blocked branch is skipped when no transfer data is provided.
func TestComputeMetacognitiveAlerts_TransferBlockedNoData(t *testing.T) {
	alerts := ComputeMetacognitiveAlerts(nil, 0, nil, nil)
	for _, a := range alerts {
		if a.Type == models.AlertTransferBlocked {
			t.Errorf("did not expect TRANSFER_BLOCKED without input data, got %+v", a)
		}
	}
}
