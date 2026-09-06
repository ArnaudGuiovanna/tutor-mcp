// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"encoding/json"
	"time"
)

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
	MaterialHash        string            `json:"material_hash"`
}

// AssessmentReview is an authenticated opinion, NOT a trusted evaluation. The
// authentication identifies an account, not whether a human or AI used it.
// RubricScore may be redacted by retention; its digest and result remain.
type AssessmentReview struct {
	ID                           string          `json:"id"`
	AttemptID                    string          `json:"attempt_id"`
	ReviewerUserID               string          `json:"reviewer_user_id"`
	ReviewerMembershipID         string          `json:"reviewer_membership_id"`
	ReviewerTokenVersion         int64           `json:"reviewer_token_version"`
	MaterialHash                 string          `json:"material_hash"`
	RubricScoreHash              string          `json:"rubric_score_hash"`
	RubricScore                  json.RawMessage `json:"rubric_score"`
	Total                        float64         `json:"total"`
	Passed                       bool            `json:"passed"`
	TrustedEvaluation            bool            `json:"trusted_evaluation"`
	CreatedAt                    time.Time       `json:"created_at"`
	CurriculumInvalidatedVersion int             `json:"curriculum_invalidated_version"`
}
