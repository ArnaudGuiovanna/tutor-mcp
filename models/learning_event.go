// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

const LearningEventProtocol = "response-feedback-v1"

// LearningEvent separates a committed answer from host-reported delivery of
// feedback or instruction. OccurredAt is server receipt time, not an assertion
// that the host displayed the material to the learner.
type LearningEvent struct {
	ID                string    `json:"id"`
	EventKey          string    `json:"event_key"`
	DomainID          string    `json:"domain_id"`
	ConceptID         string    `json:"concept"`
	AttemptID         string    `json:"attempt_id,omitempty"`
	Kind              string    `json:"kind"`
	Source            string    `json:"source"`
	CurriculumVersion int       `json:"curriculum_version"`
	OccurredAt        time.Time `json:"occurred_at"`
}

type LearningEventRequest struct {
	EventKey  string `json:"event_key"`
	DomainID  string `json:"domain_id"`
	ConceptID string `json:"concept"`
	AttemptID string `json:"attempt_id,omitempty"`
	Kind      string `json:"kind"`
}
