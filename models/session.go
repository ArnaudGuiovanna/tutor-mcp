// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

const (
	LearningSessionStatusOpen   = "open"
	LearningSessionStatusClosed = "closed"
)

// LearningSession is the durable boundary for one learning episode. Unlike
// the legacy two-hour activity window, its identity remains stable through
// pauses and is closed explicitly by the learner/tutor workflow.
type LearningSession struct {
	ID           string     `json:"id"`
	LearnerID    string     `json:"learner_id"`
	DomainID     string     `json:"domain_id,omitempty"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	LastActiveAt time.Time  `json:"last_active_at"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
}
