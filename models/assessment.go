// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

// AssessmentAttemptStatus is deliberately a state machine: a rubric and task
// are frozen before the learner response, and an evaluation can only complete
// a submitted attempt.
type AssessmentAttemptStatus string

const (
	AssessmentAttemptPrepared  AssessmentAttemptStatus = "prepared"
	AssessmentAttemptSubmitted AssessmentAttemptStatus = "submitted"
	AssessmentAttemptEvaluated AssessmentAttemptStatus = "evaluated"
	AssessmentAttemptCancelled AssessmentAttemptStatus = "cancelled"
)

// EvaluationMethod identifies the boundary that produced criterion scores.
// Trust is stored separately and is assigned by the server, never by a host
// tool parameter.
type EvaluationMethod string

const (
	EvaluationMethodHostLLM       EvaluationMethod = "host_llm"
	EvaluationMethodExternal      EvaluationMethod = "external_service"
	EvaluationMethodHumanReview   EvaluationMethod = "human_review"
	EvaluationMethodDeterministic EvaluationMethod = "deterministic"
)

// AssessmentRubric is the canonical rubric for an activity bound to a runtime
// decision. Its generated semantic content is frozen with the assessment.
// Unsupported fields must be rejected at the tool boundary, never discarded.
type AssessmentRubric struct {
	Criteria     []AssessmentRubricCriterion `json:"criteria"`
	PassingScore float64                     `json:"passing_score"`
	AnswerKey    string                      `json:"answer_key,omitempty"`
}

type AssessmentRubricCriterion struct {
	ID          string                   `json:"id"`
	Description string                   `json:"description"`
	MaxScore    float64                  `json:"max_score"`
	Anchors     []AssessmentRubricAnchor `json:"anchors,omitempty"`
}

// AssessmentRubricAnchor describes observable performance at one score. Anchors
// support the evaluator's judgment; they do not introduce another passing rule.
type AssessmentRubricAnchor struct {
	Score       float64 `json:"score"`
	Description string  `json:"description"`
}

// AssessmentAttempt is the immutable-before-response evidence envelope used
// by mastery claims. Large or sensitive artefacts may be represented by their
// SHA-256 hash instead of plaintext; at least one representation is required.
type AssessmentAttempt struct {
	ID                           string
	LearnerID                    string
	DomainID                     string
	ConceptID                    string
	SessionID                    string
	ActivityID                   string
	ActivityVersion              int
	DecisionID                   string
	CurriculumVersion            int
	CurriculumInvalidatedVersion int
	CurriculumConceptJSON        string
	OutcomeIDsJSON               string
	ActivityType                 string
	Observable                   string
	TaskText                     string
	TaskContentHash              string
	ResponseText                 string
	ResponseContentHash          string
	RubricJSON                   string
	PassingScore                 float64
	Status                       AssessmentAttemptStatus
	RubricScoreJSON              string
	Score                        float64
	Passed                       bool
	EvaluatorID                  string
	EvaluationMethod             EvaluationMethod
	EvaluationProvenanceJSON     string
	TrustedEvaluation            bool
	CreatedAt                    time.Time
	SubmittedAt                  *time.Time
	EvaluatedAt                  *time.Time
	CancelledAt                  *time.Time
}
