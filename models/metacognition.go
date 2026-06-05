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
	ConceptID    string
	Predicted    float64  // 0-1 normalized from 1-5 scale
	Actual       *float64 // nil until exercise completed
	Delta        *float64 // predicted - actual, nil until completed
	CreatedAt    time.Time
}

type MirrorMessage struct {
	Pattern      string `json:"pattern"`
	Message      string `json:"message"`
	OpenQuestion string `json:"open_question"`
}

type AutonomyMetrics struct {
	Score               float64   `json:"score"`
	Trend               string    `json:"trend"`
	InitiativeRate      float64   `json:"initiative_rate"`
	CalibrationAccuracy float64   `json:"calibration_accuracy"`
	HintIndependence    float64   `json:"hint_independence"`
	ProactiveReviewRate float64   `json:"proactive_review_rate"`
	ComputedAt          time.Time `json:"computed_at"`
}

type ConceptGap struct {
	Label         string  `json:"label"`
	Description   string  `json:"description"`
	InitialPL0    float64 `json:"initial_pl0"`
	SourceConcept string  `json:"source_concept"`
}

type TransferRecord struct {
	ID          int64
	LearnerID   string
	ConceptID   string
	ContextType string
	Score       float64
	SessionID   string
	CreatedAt   time.Time
}
