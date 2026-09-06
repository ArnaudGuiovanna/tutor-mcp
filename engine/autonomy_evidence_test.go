package engine

import (
	"math"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestAutonomyCalibrationAccuracyUsesIndividualPredictionErrors(t *testing.T) {
	cases := []struct {
		name    string
		deltas  []float64
		want    float64
		samples int
	}{
		{"opposite maximum errors", []float64{1, -1}, 0, 2},
		{"opposite moderate errors", []float64{0.5, -0.5}, 0.5, 2},
		{"accurate predictions", []float64{0, 0}, 1, 2},
		{"missing predictions", nil, 0, 0},
		{"nonfinite observations", []float64{math.NaN(), math.Inf(1), 0.25}, 0.75, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeAutonomyMetrics(AutonomyInput{CalibrationDeltas: tc.deltas})
			if math.Abs(got.CalibrationAccuracy-tc.want) > 1e-12 || got.CalibrationSamples != tc.samples {
				t.Errorf("got accuracy=%f samples=%d; want accuracy=%f samples=%d", got.CalibrationAccuracy, got.CalibrationSamples, tc.want, tc.samples)
			}
		})
	}
}

func TestAutonomyMissingEvidenceIsNotPerfectCalibrationOrIndependence(t *testing.T) {
	got := ComputeAutonomyMetrics(AutonomyInput{
		Interactions:    []*models.Interaction{nil},
		ConceptStates:   []*models.ConceptState{nil, {Concept: "A", PMastery: 0.95}},
		CalibrationBias: 0, // legacy signed mean provides no accuracy evidence
	})
	if got.Score != 0 || got.CalibrationAccuracy != 0 || got.HintIndependence != 0 || got.ObservedComponents != 0 || got.ScoreStatus != "unavailable" {
		t.Fatalf("missing observations must remain explicitly unavailable: %+v", got)
	}
}

func TestAutonomyPartialScoreReportsCoverage(t *testing.T) {
	got := ComputeAutonomyMetrics(AutonomyInput{CalibrationDeltas: []float64{0.25, -0.25}})
	if got.Score != 0.75 || got.ObservedComponents != 1 || got.ScoreStatus != "partial" || got.SessionCount != 0 || got.HintObservations != 0 || got.ReviewObservations != 0 {
		t.Fatalf("only observed calibration should contribute, with partial coverage: %+v", got)
	}
}

func TestAutonomyHintEvidenceUsesDomainAndAssistedResponses(t *testing.T) {
	now := time.Now().UTC()
	got := ComputeAutonomyMetrics(AutonomyInput{
		ConceptStates: []*models.ConceptState{{DomainID: "one", Concept: "A", PMastery: 0.95}},
		Interactions: []*models.Interaction{
			{DomainID: "one", Concept: "A", HintsRequested: 10, CreatedAt: now},
			{DomainID: "one", Concept: "A", HintsRequested: 0, CreatedAt: now},
			{DomainID: "two", Concept: "A", HintsRequested: 0, CreatedAt: now},
		},
	})
	if got.HintObservations != 2 || got.HintIndependence != 0.5 {
		t.Fatalf("only two in-domain responses, one assisted, should count: %+v", got)
	}
	if got.ReviewObservations != 0 {
		t.Fatalf("non-retrieval activities must not invent review evidence: %+v", got)
	}
}
