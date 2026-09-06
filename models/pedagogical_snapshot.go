// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

type PedagogicalSnapshot struct {
	// Read-time applicability annotation; never rewrites the historical JSON.
	CurriculumInvalidatedVersion int
	ID                           int64     `json:"id"`
	InteractionID                int64     `json:"interaction_id"`
	LearnerID                    string    `json:"learner_id"`
	DomainID                     string    `json:"domain_id"`
	Concept                      string    `json:"concept"`
	ActivityType                 string    `json:"activity_type"`
	BeforeJSON                   string    `json:"before_json"`
	ObservationJSON              string    `json:"observation_json"`
	AfterJSON                    string    `json:"after_json"`
	DecisionJSON                 string    `json:"decision_json"`
	InterpretationBrief          string    `json:"interpretation_brief"`
	CreatedAt                    time.Time `json:"created_at"`
}
