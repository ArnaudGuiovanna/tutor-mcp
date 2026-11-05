// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import (
	"encoding/json"
	"time"
)

const PedagogicalTurnSchemaVersion = "pedagogical-turn/v1"

// PedagogicalTurn is a server-confirmed view for formulation by a harness.
// It is never a capability token: each later mutation revalidates its principal,
// selected domain, curriculum and assessment state against durable storage.
type PedagogicalTurn struct {
	SchemaVersion          string                       `json:"schema_version"`
	Mode                   string                       `json:"mode"`
	GeneratedAt            time.Time                    `json:"generated_at"`
	Conversation           *ConversationContext         `json:"conversation"`
	Training               *TrainingContext             `json:"training,omitempty"`
	Learner                PedagogicalTurnLearner       `json:"learner"`
	Permissions            PedagogicalTurnPermissions   `json:"permissions"`
	Curriculum             *PedagogicalTurnCurriculum   `json:"curriculum,omitempty"`
	Difficulties           []PedagogicalTurnDifficulty  `json:"difficulties"`
	RecentObservations     []PedagogicalTurnObservation `json:"recent_observations"`
	Session                *LearningSession             `json:"session,omitempty"`
	Attempt                *PedagogicalTurnAttempt      `json:"attempt,omitempty"`
	Memory                 json.RawMessage              `json:"memory,omitempty"`
	Activity               map[string]any               `json:"activity,omitempty"`
	NeedsSelection         bool                         `json:"needs_selection"`
	SelectionReason        string                       `json:"selection_reason,omitempty"`
	TrainingChoices        []TrainingContext            `json:"training_choices,omitempty"`
	TrainingChoicesHasMore bool                         `json:"training_choices_has_more"`
	Presentation           string                       `json:"presentation,omitempty"`
	PresentationReason     string                       `json:"presentation_reason,omitempty"`
	Provenance             PedagogicalTurnProvenance    `json:"provenance"`
	DegradedComponents     []string                     `json:"degraded_components,omitempty"`
}

type PedagogicalTurnLearner struct {
	ID            string                   `json:"id"`
	Objective     string                   `json:"objective"`
	Profile       LearnerProfile           `json:"profile"`
	Accessibility AccessibilityPreferences `json:"accessibility"`
}

type PedagogicalTurnPermissions struct {
	Read            bool `json:"read"`
	PrepareActivity bool `json:"prepare_activity"`
}

type PedagogicalTurnCurriculum struct {
	DomainID      string              `json:"domain_id"`
	Version       int                 `json:"version"`
	FocusConcept  string              `json:"focus_concept,omitempty"`
	FocusSource   string              `json:"focus_source,omitempty"`
	HighStakes    bool                `json:"high_stakes"`
	Concepts      []CurriculumConcept `json:"concepts"`
	Prerequisites map[string][]string `json:"prerequisites"`
}

type PedagogicalTurnDifficulty struct {
	Concept             string `json:"concept"`
	ActiveMisconception bool   `json:"active_misconception"`
}

type PedagogicalTurnObservation struct {
	ID                  int64     `json:"id"`
	Concept             string    `json:"concept"`
	ActivityType        string    `json:"activity_type"`
	ObservedSuccess     bool      `json:"observed_success"`
	HintsRequested      int       `json:"hints_requested"`
	AssessmentAttemptID string    `json:"assessment_attempt_id,omitempty"`
	EvidenceKind        string    `json:"evidence_kind"`
	CreatedAt           time.Time `json:"created_at"`
}

// Frozen task and rubric are returned intact or the request fails. They must
// never be shortened into a different assessment contract by a context budget.
type PedagogicalTurnAttempt struct {
	ID                string                  `json:"id"`
	DomainID          string                  `json:"domain_id"`
	Concept           string                  `json:"concept"`
	SessionID         string                  `json:"session_id"`
	DecisionID        string                  `json:"decision_id"`
	CurriculumVersion int                     `json:"curriculum_version"`
	Status            AssessmentAttemptStatus `json:"status"`
	TaskText          string                  `json:"task_text"`
	TaskHash          string                  `json:"task_content_hash,omitempty"`
	ActivityID        string                  `json:"activity_id"`
	ActivityVersion   int                     `json:"activity_version"`
	ActivityType      string                  `json:"activity_type"`
	Observable        string                  `json:"observable"`
	PassingScore      float64                 `json:"passing_score"`
	Competency        json.RawMessage         `json:"competency,omitempty"`
	OutcomeIDs        json.RawMessage         `json:"outcome_ids,omitempty"`
	EventProtocol     string                  `json:"learning_event_protocol,omitempty"`
	Rubric            json.RawMessage         `json:"rubric"`
	ResponseText      string                  `json:"response_text,omitempty"`
	ResponseHash      string                  `json:"response_content_hash,omitempty"`
	SubmittedAt       *time.Time              `json:"submitted_at,omitempty"`
}

type PedagogicalTurnProvenance struct {
	Source             string                     `json:"source"`
	PolicyVersion      string                     `json:"policy_version"`
	SelectionVersion   int                        `json:"selection_version"`
	Revision           string                     `json:"revision"`
	ModelUpdated       bool                       `json:"model_updated"`
	ContextBudgetBytes int                        `json:"context_budget_bytes"`
	Reductions         []PedagogicalTurnReduction `json:"reductions"`
}

type PedagogicalTurnReduction struct {
	Field         string `json:"field"`
	Reason        string `json:"reason"`
	SourceVersion string `json:"source_version"`
	Available     int    `json:"available,omitempty"`
	Returned      int    `json:"returned,omitempty"`
}
