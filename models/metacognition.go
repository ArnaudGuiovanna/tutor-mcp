// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

type AffectState struct {
	ID                  int64
	LearnerID           string
	SessionID           string
	Energy              int     // 1-4: fatigued → on fire
	SubjectConfidence   int     // 1-4: anxious → confident
	Satisfaction        int     // 1-4: frustrating → flow
	PerceivedDifficulty int     // 1-4: too hard → too easy
	NextSessionIntent   int     // 1-4: now → don't know
	AutonomyScore       float64 // snapshot computed at end-of-session
	CreatedAt           time.Time
}

type CalibrationRecord struct {
	PredictionID string
	LearnerID    string
	DomainID     string
	ConceptID    string
	Predicted    float64  // 0-1 normalized from 1-5 scale
	Actual       *float64 // nil until exercise completed
	Delta        *float64 // predicted - actual, nil until completed
	CreatedAt    time.Time
}

type MirrorMessage struct {
	Pattern        string         `json:"pattern"`
	Facts          map[string]any `json:"facts,omitempty"`
	Window         *MirrorWindow  `json:"window,omitempty"`
	Confidence     string         `json:"confidence,omitempty"`
	DialogueIntent string         `json:"dialogue_intent,omitempty"`
	// Historical authored messages remain decodable. Runtime detection emits
	// observations and a dialogue intent; the generative tutor writes the text.
	Message      string `json:"message,omitempty"`
	OpenQuestion string `json:"open_question,omitempty"`
}

type MirrorWindow struct {
	SessionCount     int `json:"session_count"`
	InteractionCount int `json:"interaction_count"`
}

type AutonomyMetrics struct {
	// Score is a legacy descriptive mean of observed rates. It is not a
	// validated measure of autonomy and must not drive support or fading.
	Score               float64   `json:"score"`
	ScoreStatus         string    `json:"score_status"`
	ObservedComponents  int       `json:"observed_components"`
	Trend               string    `json:"trend"`
	InitiativeRate      float64   `json:"initiative_rate"`
	SessionCount        int       `json:"session_count"`
	CalibrationAccuracy float64   `json:"calibration_accuracy"`
	CalibrationSamples  int       `json:"calibration_samples"`
	HintIndependence    float64   `json:"hint_independence"`
	HintObservations    int       `json:"hint_observations"`
	ProactiveReviewRate float64   `json:"proactive_review_rate"`
	ReviewObservations  int       `json:"review_observations"`
	ComputedAt          time.Time `json:"computed_at"`
}

type ConceptGap struct {
	Label         string  `json:"label"`
	Description   string  `json:"description"`
	InitialPL0    float64 `json:"initial_pl0"`
	SourceConcept string  `json:"source_concept"`
}

type TransferRecord struct {
	ID                  int64
	LearnerID           string
	DomainID            string
	AssessmentAttemptID string
	ConceptID           string
	ContextType         string
	Score               float64
	SessionID           string
	CreatedAt           time.Time
}
