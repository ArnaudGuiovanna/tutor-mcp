// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

const (
	LearningSessionStatusOpen   = "open"
	LearningSessionStatusClosed = "closed"
)

// LearningSessionIdleTimeout bounds how long an open session survives without
// activity. A learner who closes the chat without record_session_close would
// otherwise keep one session open for days; its age then trips the OVERLOAD
// alert (session longer than 45 minutes) on every later call and forces a
// CLOSE_SESSION activity. A session idle for longer than this is closed with
// closed_at = last_active_at before a new one is opened, and read paths ignore
// it.
const LearningSessionIdleTimeout = 4 * time.Hour

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
