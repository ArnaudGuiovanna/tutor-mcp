package engine

import (
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestComputeAutonomyMetrics(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name    string
		input   AutonomyInput
		wantMin float64
		wantMax float64
	}{
		{
			name: "all self-initiated, perfect calibration, no hints, all proactive",
			input: AutonomyInput{
				Interactions:      makeInteractions(10, true, true, 0, true, now),
				ConceptStates:     []*models.ConceptState{{Concept: "A", PMastery: 0.9}},
				CalibrationDeltas: []float64{0, 0, 0},
				SessionGap:        2 * time.Hour,
			},
			wantMin: 0.9,
			wantMax: 1.0,
		},
		{
			name: "no initiative, poor calibration, heavy hints, no proactive",
			input: AutonomyInput{
				Interactions:      makeInteractions(10, false, false, 3, false, now),
				ConceptStates:     []*models.ConceptState{{Concept: "A", PMastery: 0.9}},
				CalibrationDeltas: []float64{1, -1},
				SessionGap:        2 * time.Hour,
			},
			wantMin: 0.0,
			wantMax: 0.15,
		},
		{
			name: "empty interactions",
			input: AutonomyInput{
				Interactions:    nil,
				ConceptStates:   nil,
				CalibrationBias: 0.0,
				SessionGap:      2 * time.Hour,
			},
			wantMin: 0.0,
			wantMax: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeAutonomyMetrics(tt.input)
			if got.Score < tt.wantMin || got.Score > tt.wantMax {
				t.Errorf("Score = %.2f, want [%.2f, %.2f]", got.Score, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestGroupIntoSessions(t *testing.T) {
	now := time.Now().UTC()
	interactions := []*models.Interaction{
		{CreatedAt: now.Add(-5 * time.Hour)},
		{CreatedAt: now.Add(-4 * time.Hour)},
		{CreatedAt: now.Add(-1 * time.Hour)},
		{CreatedAt: now.Add(-30 * time.Minute)},
	}

	sessions := groupIntoSessions(interactions, 2*time.Hour)
	if len(sessions) != 2 {
		t.Errorf("got %d sessions, want 2", len(sessions))
	}
}

func TestGroupIntoSessions_PrefersDurableSessionIDAndKeepsLegacyFallback(t *testing.T) {
	now := time.Now().UTC()
	interactions := []*models.Interaction{
		{SessionID: "sess_a", CreatedAt: now},
		// Same durable session survives a gap much larger than the legacy
		// heuristic.
		{SessionID: "sess_a", CreatedAt: now.Add(5 * time.Hour)},
		// A new explicit session is a boundary even only one minute later.
		{SessionID: "sess_b", CreatedAt: now.Add(5*time.Hour + time.Minute)},
		// Legacy rows never merge into an explicit session; legacy rows still
		// use the requested gap amongst themselves.
		{CreatedAt: now.Add(5*time.Hour + 2*time.Minute)},
		{CreatedAt: now.Add(5*time.Hour + 30*time.Minute)},
		{CreatedAt: now.Add(8 * time.Hour)},
	}

	sessions := groupIntoSessions(interactions, 2*time.Hour)
	if len(sessions) != 4 {
		t.Fatalf("sessions = %d, want 4: %+v", len(sessions), sessions)
	}
	if len(sessions[0]) != 2 || sessions[0][0].SessionID != "sess_a" {
		t.Fatalf("durable session A was split: %+v", sessions[0])
	}
	if len(sessions[1]) != 1 || sessions[1][0].SessionID != "sess_b" {
		t.Fatalf("durable session B merged unexpectedly: %+v", sessions[1])
	}
	if len(sessions[2]) != 2 || len(sessions[3]) != 1 {
		t.Fatalf("legacy gap fallback incorrect: %+v", sessions[2:])
	}
}

func TestComputeAutonomyTrend(t *testing.T) {
	tests := []struct {
		name   string
		scores []float64
		want   string
	}{
		{"improving", []float64{0.3, 0.4, 0.5, 0.6, 0.7, 0.5, 0.4, 0.3, 0.2, 0.1}, "improving"},
		{"declining", []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.7, 0.8, 0.8, 0.9, 0.9}, "declining"},
		{"stable", []float64{0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5}, "stable"},
		{"too few", []float64{0.5}, "stable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeAutonomyTrend(tt.scores)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectMirrorPattern(t *testing.T) {
	tests := []struct {
		name    string
		input   MirrorInput
		wantNil bool
		wantPat string
	}{
		{
			name: "hint use described without overuse diagnosis",
			input: MirrorInput{
				Interactions:    makeInteractions(15, true, false, 3, true, time.Now().UTC()),
				ConceptStates:   []*models.ConceptState{{Concept: "A", PMastery: 0.9}},
				AutonomyScores:  []float64{0.5, 0.5, 0.5},
				CalibrationBias: 0.0,
				SessionCount:    5,
			},
			wantNil: false,
			wantPat: "hint_use_observed",
		},
		{
			name: "no pattern — too few sessions",
			input: MirrorInput{
				Interactions:    makeInteractions(5, true, false, 0, true, time.Now().UTC()),
				ConceptStates:   []*models.ConceptState{{Concept: "A", PMastery: 0.5}},
				AutonomyScores:  []float64{0.5, 0.5},
				CalibrationBias: 0.0,
				SessionCount:    2,
			},
			wantNil: true,
		},
		{
			name: "declining composite is not a dependency diagnosis",
			input: MirrorInput{
				Interactions:    makeInteractions(10, true, false, 0, true, time.Now().UTC()),
				ConceptStates:   []*models.ConceptState{{Concept: "A", PMastery: 0.5}},
				AutonomyScores:  []float64{0.4, 0.5, 0.6},
				CalibrationBias: 0.0,
				SessionCount:    5,
			},
			wantNil: true,
		},
		{
			name: "no recorded initiative described",
			input: MirrorInput{
				Interactions:    makeInteractions(10, false, false, 0, true, time.Now().UTC()),
				ConceptStates:   []*models.ConceptState{{Concept: "A", PMastery: 0.5}},
				AutonomyScores:  []float64{0.5, 0.5, 0.5},
				CalibrationBias: 0.0,
				SessionCount:    5,
			},
			wantNil: false,
			wantPat: "no_recorded_initiative",
		},
		{
			name: "calibration_drift detected",
			input: MirrorInput{
				Interactions: func() []*models.Interaction {
					is := makeInteractions(10, true, false, 0, true, time.Now().UTC())
					is[0].SelfInitiated = true
					return is
				}(),
				ConceptStates:      []*models.ConceptState{{Concept: "A", PMastery: 0.5}},
				AutonomyScores:     []float64{0.5, 0.5, 0.5},
				CalibrationBias:    0.3,
				CalibrationSamples: 5,
				SessionCount:       5,
			},
			wantNil: false,
			wantPat: "calibration_drift",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectMirrorPattern(tt.input)
			if tt.wantNil && got != nil {
				t.Errorf("expected nil, got %+v", got)
			}
			if !tt.wantNil && got == nil {
				t.Errorf("expected pattern %s, got nil", tt.wantPat)
			}
			if !tt.wantNil && got != nil && got.Pattern != tt.wantPat {
				t.Errorf("got pattern %q, want %q", got.Pattern, tt.wantPat)
			}
			if got != nil {
				if got.Message != "" || got.OpenQuestion != "" {
					t.Fatal("runtime must leave the pedagogical message and question to generation")
				}
				if len(got.Facts) == 0 || got.DialogueIntent == "" || got.Confidence != "descriptive_only" {
					t.Fatalf("missing descriptive generation context: %+v", got)
				}
				if got.Window == nil || got.Window.SessionCount != tt.input.SessionCount || got.Window.InteractionCount != len(tt.input.Interactions) {
					t.Fatalf("observation window must state its evidence counts: %+v", got.Window)
				}
			}
		})
	}
}

func TestMirrorMissingInitiativeDoesNotInferNotificationOrigin(t *testing.T) {
	mirror := DetectMirrorPattern(MirrorInput{
		SessionCount: 3,
		Interactions: makeInteractions(10, false, false, 0, true, time.Now().UTC()),
	})
	if mirror == nil || mirror.Pattern != "no_recorded_initiative" {
		t.Fatalf("expected observed missing initiative, got %+v", mirror)
	}
	if mirror.Facts["notification_origin_verified"] != false {
		t.Fatal("absence of an initiative flag must not establish a notification as the cause")
	}
	if got := DetectMirrorPattern(MirrorInput{SessionCount: 3}); got != nil {
		t.Fatalf("no interactions cannot establish an initiative pattern: %+v", got)
	}
}

func TestComputeTutorMode(t *testing.T) {
	tests := []struct {
		name   string
		affect *models.AffectState
		alerts []models.Alert
		want   string
	}{
		{"normal — no affect", nil, nil, "normal"},
		{"scaffolding — anxious", &models.AffectState{SubjectConfidence: 1}, nil, "scaffolding"},
		{"lighter — fatigued", &models.AffectState{Energy: 1, SubjectConfidence: 3}, nil, "lighter"},
		{"lighter — affect negative frustration", &models.AffectState{Energy: 2, Satisfaction: 1}, []models.Alert{{Type: models.AlertAffectNegative}}, "lighter"},
		{"lighter — affect negative bored (was recontextualize)", &models.AffectState{Energy: 4, Satisfaction: 1}, []models.Alert{{Type: models.AlertAffectNegative}}, "lighter"},
		{"normal — happy", &models.AffectState{Energy: 3, SubjectConfidence: 3, Satisfaction: 3}, nil, "normal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeTutorMode(tt.affect, tt.alerts)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestComputeTutorMode_NeverReturnsRecontextualize is a regression guard for
// issue #60: the cosmetic "recontextualize" mode was removed because it had no
// behavioural effect (it only appended a label suffix to Activity.Rationale).
// This sweep exercises the full input space ComputeTutorMode reads and asserts
// the removed mode is never produced.
func TestComputeTutorMode_NeverReturnsRecontextualize(t *testing.T) {
	alertSets := [][]models.Alert{
		nil,
		{{Type: models.AlertAffectNegative}},
		{{Type: models.AlertCalibrationDiverging}},
		{{Type: models.AlertAffectNegative}, {Type: models.AlertCalibrationDiverging}},
	}
	// AffectState fields used by ComputeTutorMode are Energy, Satisfaction,
	// SubjectConfidence — sweep their full 0..5 range.
	for _, alerts := range alertSets {
		for energy := 0; energy <= 5; energy++ {
			for sat := 0; sat <= 5; sat++ {
				for conf := 0; conf <= 5; conf++ {
					affect := &models.AffectState{
						Energy:            energy,
						Satisfaction:      sat,
						SubjectConfidence: conf,
					}
					got := ComputeTutorMode(affect, alerts)
					if got == "recontextualize" {
						t.Errorf("ComputeTutorMode returned removed mode %q for affect=%+v alerts=%v", got, affect, alerts)
					}
				}
			}
		}
	}
	// Nil-affect path.
	if got := ComputeTutorMode(nil, nil); got == "recontextualize" {
		t.Errorf("ComputeTutorMode(nil, nil) returned removed mode %q", got)
	}
}

func makeInteractions(n int, selfInitiated bool, proactive bool, hints int, success bool, baseTime time.Time) []*models.Interaction {
	var interactions []*models.Interaction
	for i := 0; i < n; i++ {
		interactions = append(interactions, &models.Interaction{
			LearnerID:         "test",
			Concept:           "A",
			ActivityType:      "RECALL_EXERCISE",
			Success:           success,
			HintsRequested:    hints,
			SelfInitiated:     selfInitiated,
			IsProactiveReview: proactive,
			CreatedAt:         baseTime.Add(-time.Duration(n-i) * 30 * time.Minute),
		})
	}
	return interactions
}
