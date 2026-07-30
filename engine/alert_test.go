package engine

import (
	"math"
	"testing"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

func ptrTime(t time.Time) *time.Time { return &t }

func alertStateAtRetention(t *testing.T, concept string, retention float64) *models.ConceptState {
	t.Helper()
	lastReview := time.Now().UTC().Add(-24 * time.Hour)
	return &models.ConceptState{
		Concept:     concept,
		Stability:   stabilityForRetention(t, retention),
		ElapsedDays: 1,
		LastReview:  &lastReview,
		PMastery:    0.50,
		CardState:   "review",
	}
}

func alertNonForgettingState(concept string, mastery float64) *models.ConceptState {
	lastReview := time.Now().UTC().Add(-24 * time.Hour)
	return &models.ConceptState{
		Concept:     concept,
		PMastery:    mastery,
		Stability:   30,
		ElapsedDays: 1,
		LastReview:  &lastReview,
		CardState:   "review",
	}
}

func stabilityForRetention(t *testing.T, retention float64) float64 {
	t.Helper()
	if retention <= 0 || retention >= 1 || math.IsNaN(retention) {
		t.Fatalf("invalid retention target %v", retention)
	}

	low, high := 1e-9, 1.0
	for algorithms.Retrievability(1, high) < retention {
		high *= 2
	}
	for i := 0; i < 80; i++ {
		mid := (low + high) / 2
		if algorithms.Retrievability(1, mid) < retention {
			low = mid
		} else {
			high = mid
		}
	}
	return high
}

func findAlert(alerts []models.Alert, typ models.AlertType, concept string) (models.Alert, bool) {
	for _, a := range alerts {
		if a.Type == typ && a.Concept == concept {
			return a, true
		}
	}
	return models.Alert{}, false
}

func TestComputeAlertsForgetting(t *testing.T) {
	states := []*models.ConceptState{
		{Concept: "goroutines", Stability: 0.2, ElapsedDays: 5, PMastery: 0.5, CardState: "review",
			LastReview: ptrTime(time.Now().AddDate(0, 0, -5))},
	}
	alerts := ComputeAlerts(states, nil, time.Time{})
	found := false
	for _, a := range alerts {
		if a.Type == models.AlertForgetting && a.Concept == "goroutines" {
			found = true
			if a.Urgency != models.UrgencyCritical && a.Urgency != models.UrgencyWarning {
				t.Errorf("urgency = %s, want critical or warning", a.Urgency)
			}
		}
	}
	if !found {
		t.Error("expected FORGETTING alert for goroutines")
	}
}

func TestComputeAlertsForgettingRetentionBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		retention   float64
		wantAlert   bool
		wantUrgency models.AlertUrgency
	}{
		{
			name:      "just above warning threshold",
			retention: algorithms.RetentionAlertWarningThreshold + 0.0001,
			wantAlert: false,
		},
		{
			name:        "just below warning threshold",
			retention:   algorithms.RetentionAlertWarningThreshold - 0.0001,
			wantAlert:   true,
			wantUrgency: models.UrgencyWarning,
		},
		{
			name:        "just above critical threshold remains warning",
			retention:   algorithms.RetentionAlertCriticalThreshold + 0.0001,
			wantAlert:   true,
			wantUrgency: models.UrgencyWarning,
		},
		{
			name:        "just below critical threshold",
			retention:   algorithms.RetentionAlertCriticalThreshold - 0.0001,
			wantAlert:   true,
			wantUrgency: models.UrgencyCritical,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states := []*models.ConceptState{alertStateAtRetention(t, "boundary", tc.retention)}
			alerts := ComputeAlerts(states, nil, time.Time{})

			a, found := findAlert(alerts, models.AlertForgetting, "boundary")
			if found != tc.wantAlert {
				t.Fatalf("FORGETTING presence: got %v, want %v (retention target %.4f)", found, tc.wantAlert, tc.retention)
			}
			if tc.wantAlert && a.Urgency != tc.wantUrgency {
				t.Fatalf("FORGETTING urgency: got %s, want %s", a.Urgency, tc.wantUrgency)
			}
		})
	}
}

func TestComputeAlertsMasteryReady(t *testing.T) {
	// Stability=1.0, ElapsedDays=1 yields retention well above the named
	// FORGETTING warning threshold, so FORGETTING/MASTERY_READY arbitration does
	// not suppress MASTERY_READY here.
	lastReview := time.Now().UTC().Add(-24 * time.Hour)
	states := []*models.ConceptState{{Concept: "basics", PMastery: 0.90, Stability: 1.0, ElapsedDays: 1, LastReview: &lastReview, CardState: "review"}}
	alerts := ComputeAlerts(states, nil, time.Time{})
	found := false
	for _, a := range alerts {
		if a.Type == models.AlertMasteryReady && a.Concept == "basics" {
			found = true
		}
	}
	if !found {
		t.Error("expected MASTERY_READY for basics")
	}
}

// TestComputeAlertsMasteryForgettingArbitration covers the four corners of the
// (PMastery × retention) matrix to verify the alert-level arbitration rule:
// FORGETTING at UrgencyCritical suppresses MASTERY_READY for the same concept.
// FORGETTING at UrgencyWarning does NOT suppress — the learner is in a nuanced
// "almost mastered, slightly stale" state that warrants both nudges.
//
// Threshold rationale: suppression is tied to
// algorithms.RetentionAlertCriticalThreshold; retuning the critical band retunes
// this arbitration with it.
func TestComputeAlertsMasteryForgettingArbitration(t *testing.T) {
	// Retention closed-form: (1 + (19/81)*elapsed/stability)^(-0.5)
	//   elapsed=1,  stability=1.0  -> retention above warning threshold
	//   elapsed=5,  stability=0.2  -> retention in warning band
	//   elapsed=5,  stability=0.1  -> retention in critical band
	masteryHigh := 0.90 // >= MasteryBKT() (0.85)
	masteryLow := 0.50  // <  MasteryBKT()

	cases := []struct {
		name                  string
		pMastery              float64
		stability             float64
		elapsedDays           int
		wantForgetting        bool
		wantForgettingUrgency models.AlertUrgency
		wantMasteryReady      bool
	}{
		{
			name:             "high mastery + high retention → only MASTERY_READY",
			pMastery:         masteryHigh,
			stability:        1.0,
			elapsedDays:      1,
			wantForgetting:   false,
			wantMasteryReady: true,
		},
		{
			name:                  "high mastery + warning retention → both alerts kept",
			pMastery:              masteryHigh,
			stability:             0.2,
			elapsedDays:           5,
			wantForgetting:        true,
			wantForgettingUrgency: models.UrgencyWarning,
			wantMasteryReady:      true,
		},
		{
			name:                  "high mastery + critical retention → only FORGETTING (arbitration)",
			pMastery:              masteryHigh,
			stability:             0.1,
			elapsedDays:           5,
			wantForgetting:        true,
			wantForgettingUrgency: models.UrgencyCritical,
			wantMasteryReady:      false,
		},
		{
			name:                  "low mastery + critical retention → only FORGETTING",
			pMastery:              masteryLow,
			stability:             0.1,
			elapsedDays:           5,
			wantForgetting:        true,
			wantForgettingUrgency: models.UrgencyCritical,
			wantMasteryReady:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lastReview := time.Now().UTC().Add(-time.Duration(tc.elapsedDays) * 24 * time.Hour)
			states := []*models.ConceptState{
				{
					Concept:     "arbitration_concept",
					Stability:   tc.stability,
					ElapsedDays: tc.elapsedDays,
					LastReview:  &lastReview,
					PMastery:    tc.pMastery,
					CardState:   "review",
				},
			}
			alerts := ComputeAlerts(states, nil, time.Time{})

			gotForgetting := false
			gotForgettingUrgency := models.AlertUrgency("")
			gotMasteryReady := false
			for _, a := range alerts {
				if a.Concept != "arbitration_concept" {
					continue
				}
				switch a.Type {
				case models.AlertForgetting:
					gotForgetting = true
					gotForgettingUrgency = a.Urgency
				case models.AlertMasteryReady:
					gotMasteryReady = true
				}
			}

			if gotForgetting != tc.wantForgetting {
				t.Errorf("FORGETTING presence: got %v, want %v", gotForgetting, tc.wantForgetting)
			}
			if tc.wantForgetting && gotForgettingUrgency != tc.wantForgettingUrgency {
				t.Errorf("FORGETTING urgency: got %s, want %s", gotForgettingUrgency, tc.wantForgettingUrgency)
			}
			if gotMasteryReady != tc.wantMasteryReady {
				t.Errorf("MASTERY_READY presence: got %v, want %v", gotMasteryReady, tc.wantMasteryReady)
			}
		})
	}
}

func TestComputeAlertsZPDDrift(t *testing.T) {
	interactions := []*models.Interaction{
		{Concept: "pointers", Success: false},
		{Concept: "pointers", Success: false},
		{Concept: "pointers", Success: false},
	}
	states := []*models.ConceptState{alertNonForgettingState("pointers", 0.3)}
	alerts := ComputeAlerts(states, interactions, time.Time{})
	found := false
	for _, a := range alerts {
		if a.Type == models.AlertZPDDrift && a.Concept == "pointers" {
			found = true
		}
	}
	if !found {
		t.Error("expected ZPD_DRIFT for pointers")
	}
}

func TestComputeAlertsForgettingCriticalSuppressesFailureZPDDrift(t *testing.T) {
	interactions := []*models.Interaction{
		{Concept: "pointers", Success: false},
		{Concept: "pointers", Success: false},
		{Concept: "pointers", Success: false},
	}
	cases := []struct {
		name      string
		retention float64
		wantZPD   bool
	}{
		{
			name:      "critical forgetting suppresses failure ZPD",
			retention: algorithms.RetentionAlertCriticalThreshold - 0.0001,
			wantZPD:   false,
		},
		{
			name:      "warning forgetting keeps failure ZPD",
			retention: algorithms.RetentionAlertWarningThreshold - 0.0001,
			wantZPD:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states := []*models.ConceptState{alertStateAtRetention(t, "pointers", tc.retention)}
			alerts := ComputeAlerts(states, interactions, time.Time{})

			if _, found := findAlert(alerts, models.AlertForgetting, "pointers"); !found {
				t.Fatal("expected FORGETTING alert in collision scenario")
			}
			_, gotZPD := findAlert(alerts, models.AlertZPDDrift, "pointers")
			if gotZPD != tc.wantZPD {
				t.Fatalf("ZPD_DRIFT presence: got %v, want %v", gotZPD, tc.wantZPD)
			}
		})
	}
}

func TestComputeAlertsIRTPredictiveZPDDrift(t *testing.T) {
	// Concept with low theta and high difficulty → IRT pCorrect < 0.55
	// Theta = -1.0, FSRS difficulty = 8.0 → IRT difficulty ≈ 1.67
	// pCorrect = 1/(1+exp(-1*(-1.0-1.67))) ≈ 0.065 — well below 0.55
	states := []*models.ConceptState{
		alertNonForgettingState("channels", 0.3),
	}
	states[0].Theta = -1.0
	states[0].Difficulty = 8.0
	states[0].Reps = 3
	alerts := ComputeAlerts(states, nil, time.Time{})

	found := false
	for _, a := range alerts {
		if a.Type == models.AlertZPDDrift && a.Concept == "channels" && a.Urgency == models.UrgencyInfo {
			found = true
		}
	}
	if !found {
		t.Error("expected IRT-predictive ZPD_DRIFT (info) for channels")
	}
}

func TestComputeAlertsForgettingCriticalSuppressesPredictiveZPDDrift(t *testing.T) {
	cs := alertStateAtRetention(t, "channels", algorithms.RetentionAlertCriticalThreshold-0.0001)
	cs.Theta = -1.0
	cs.Difficulty = 8.0
	cs.Reps = 3

	alerts := ComputeAlerts([]*models.ConceptState{cs}, nil, time.Time{})
	if a, found := findAlert(alerts, models.AlertForgetting, "channels"); !found || a.Urgency != models.UrgencyCritical {
		t.Fatalf("expected critical FORGETTING alert, got found=%v alert=%+v", found, a)
	}
	if _, found := findAlert(alerts, models.AlertZPDDrift, "channels"); found {
		t.Fatal("FORGETTING critical must suppress same-concept predictive ZPD_DRIFT")
	}
}

func TestComputeAlertsIRTPredictiveSkipsWhenFailureBased(t *testing.T) {
	// Concept already has 3 failures → failure-based ZPD_DRIFT (warning) exists.
	// IRT-predictive should NOT add a duplicate.
	states := []*models.ConceptState{
		alertNonForgettingState("pointers", 0.3),
	}
	states[0].Theta = -1.0
	states[0].Difficulty = 8.0
	states[0].Reps = 5
	interactions := []*models.Interaction{
		{Concept: "pointers", Success: false},
		{Concept: "pointers", Success: false},
		{Concept: "pointers", Success: false},
	}
	alerts := ComputeAlerts(states, interactions, time.Time{})

	zpdCount := 0
	for _, a := range alerts {
		if a.Type == models.AlertZPDDrift && a.Concept == "pointers" {
			zpdCount++
		}
	}
	if zpdCount != 1 {
		t.Errorf("expected exactly 1 ZPD_DRIFT for pointers, got %d", zpdCount)
	}
}

func TestComputeAlertsDependencyIncreasing(t *testing.T) {
	autonomyScores := []float64{0.4, 0.5, 0.6} // declining (newest first)
	alerts := ComputeMetacognitiveAlerts(autonomyScores, 0.3, nil, nil)

	found := false
	for _, a := range alerts {
		if a.Type == models.AlertDependencyIncreasing {
			found = true
		}
	}
	if !found {
		t.Error("expected DEPENDENCY_INCREASING alert")
	}
}

func TestComputeAlertsCalibrationDiverging(t *testing.T) {
	alerts := ComputeMetacognitiveAlerts(nil, 1.6, nil, nil)

	found := false
	for _, a := range alerts {
		if a.Type == models.AlertCalibrationDiverging {
			found = true
		}
	}
	if !found {
		t.Error("expected CALIBRATION_DIVERGING alert")
	}
}

func TestComputeAlertsAffectNegative(t *testing.T) {
	affects := []*models.AffectState{
		{Satisfaction: 2},
		{Satisfaction: 1},
	}
	alerts := ComputeMetacognitiveAlerts(nil, 0, affects, nil)

	found := false
	for _, a := range alerts {
		if a.Type == models.AlertAffectNegative {
			found = true
		}
	}
	if !found {
		t.Error("expected AFFECT_NEGATIVE alert")
	}
}

func TestComputeAlertsTransferBlocked(t *testing.T) {
	states := []*models.ConceptState{
		{Concept: "A", PMastery: 0.90},
	}
	transfers := []*models.TransferRecord{
		{ConceptID: "A", ContextType: "real_world", Score: 0.3},
		{ConceptID: "A", ContextType: "interview", Score: 0.4},
	}
	alerts := ComputeMetacognitiveAlerts(nil, 0, nil, nil, WithTransferData(states, transfers))

	found := false
	for _, a := range alerts {
		if a.Type == models.AlertTransferBlocked {
			found = true
		}
	}
	if !found {
		t.Error("expected TRANSFER_BLOCKED alert")
	}
}
