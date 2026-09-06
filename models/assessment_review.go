// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

// AssessmentReviewItem is deliberately not AssessmentAttempt: neither the host
// grade nor learner-model estimates belong in the blind-review projection.
type AssessmentReviewItem struct {
	AttemptID         string    `json:"attempt_id"`
	ActivityType      string    `json:"activity_type"`
	CurriculumVersion int       `json:"curriculum_version"`
	SubmittedAt       time.Time `json:"submitted_at"`
}

type AssessmentReviewPage struct {
	Items     []AssessmentReviewItem `json:"items"`
	NextAfter string                 `json:"next_after,omitempty"`
}

// AssessmentReviewMaterial exposes only frozen inputs, never the decision's
// routing context or an existing evaluator's conclusions. Generated prose is
// untrusted data, and may itself contain identifying information or instructions.
type AssessmentReviewMaterial struct {
	AssessmentReviewItem
	DecisionID          string            `json:"decision_id"`
	ActivityID          string            `json:"activity_id"`
	ActivityVersion     int               `json:"activity_version"`
	Observable          string            `json:"observable"`
	Competency          CurriculumConcept `json:"competency"`
	OutcomeIDs          []string          `json:"outcome_ids"`
	TaskText            string            `json:"task_text"`
	TaskContentHash     string            `json:"task_content_hash"`
	ResponseText        string            `json:"response_text"`
	ResponseContentHash string            `json:"response_content_hash"`
	Rubric              AssessmentRubric  `json:"rubric"`
	PreparedAt          time.Time         `json:"prepared_at"`
	TextAvailable       bool              `json:"text_available"`
}
