// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

// These value/result types are returned by the persistence port (package
// store) and its implementations. They live in models so both store and db
// can reference them without importing each other.

// MisconceptionGroup is the DB-layer shape of a grouped misconception row.
type MisconceptionGroup struct {
	Concept           string    `json:"concept"`
	MisconceptionType string    `json:"misconception_type"`
	Count             int       `json:"count"`
	FirstSeen         time.Time `json:"first_seen"`
	LastSeen          time.Time `json:"last_seen"`
	LastErrorDetail   string    `json:"last_error_detail"`
	Status            string    `json:"status"`
}

// ActionHistoryCounts encapsulates per-concept activity-type counts used by
// the phase-transition logic and action-selector heuristics.
type ActionHistoryCounts struct {
	MasteryChallengeCount int
	FeynmanCount          int
	TransferCount         int
	InteractionsAboveBKT  int
}

// RawLearnerEvent is the DB-layer shape of a timeline event. Engine layer
// converts to engine.LearnerEvent (same fields, different package).
type RawLearnerEvent struct {
	At      time.Time
	Kind    string
	Message string
	Concept string
}

// LearningNegotiationOverridePayloadResult is the result type returned by
// ConsumeLearningNegotiationOverridePayload.
type LearningNegotiationOverridePayloadResult struct {
	ID        int64
	Payload   string
	Status    string
	ExpiresAt *time.Time
}
