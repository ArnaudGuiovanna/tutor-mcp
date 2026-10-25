// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

// Training is the server-owned library projection. IDs are real references,
// capabilities are evaluated against the live membership, never inferred by UI.
type Training struct {
	LearningVersionID string             `json:"learning_version_id,omitempty"`
	ID                string             `json:"id"`
	Kind              string             `json:"kind"`
	Name              string             `json:"name"`
	Description       string             `json:"description"`
	DomainID          string             `json:"domain_id,omitempty"`
	FormationID       string             `json:"formation_id,omitempty"`
	VersionID         string             `json:"version_id,omitempty"`
	CohortID          string             `json:"cohort_id,omitempty"`
	Revision          string             `json:"revision"`
	Status            string             `json:"status"`
	Audience          string             `json:"audience"`
	AuthorID          string             `json:"author_id"`
	CanEdit           bool               `json:"can_edit"`
	CanPublish        bool               `json:"can_publish"`
	CanReview         bool               `json:"can_review"`
	CanLearn          bool               `json:"can_learn"`
	CanShare          bool               `json:"can_share"`
	FreePublication   bool               `json:"free_publication"`
	Progress          []TrainingProgress `json:"progress"`
}

type TrainingProgress struct {
	Concept    string     `json:"concept"`
	Mastery    float64    `json:"mastery"`
	LastReview *time.Time `json:"last_review,omitempty"`
	NextReview *time.Time `json:"next_review,omitempty"`
}

type TrainingPage struct {
	Items     []Training `json:"items"`
	NextAfter string     `json:"next_after"`
}

// One document is used by manual editing, AI proposals, previews and commits.
type TrainingDocument struct {
	Training    Training                `json:"training"`
	Curriculum  *CurriculumSnapshot     `json:"curriculum,omitempty"`
	Modules     []FormationModuleInput  `json:"modules"`
	Concepts    []FormationConceptInput `json:"concepts"`
	Proposal    *TrainingProposal       `json:"proposal,omitempty"`
	Submission  *TrainingSubmission     `json:"submission,omitempty"`
	Calibration any                     `json:"calibration,omitempty"`
}

type TrainingProgramme struct {
	Training   Training                `json:"training"`
	Curriculum *CurriculumSnapshot     `json:"curriculum,omitempty"`
	Modules    []FormationModuleInput  `json:"modules"`
	Concepts   []FormationConceptInput `json:"concepts"`
}

type TrainingProposal struct {
	ID               string             `json:"id"`
	ExpectedRevision string             `json:"expected_revision"`
	Document         *TrainingProgramme `json:"document"`
}

type TrainingSubmission struct {
	Document  *TrainingProgramme `json:"document,omitempty"`
	ID        string             `json:"id"`
	VersionID string             `json:"version_id"`
	Revision  string             `json:"revision"`
	Status    string             `json:"status"`
	Reason    string             `json:"reason"`
	AuthorID  string             `json:"author_id"`
}

type ConversationContext struct {
	ConversationID   string `json:"conversation_id"`
	TrainingID       string `json:"training_id"`
	DomainID         string `json:"domain_id"`
	Revision         string `json:"revision"`
	SelectionVersion int    `json:"selection_version"`
	Name             string `json:"name"`
}

type TrainingRequest struct {
	ID                string            `json:"id,omitempty"`
	Query             string            `json:"query,omitempty"`
	Filter            string            `json:"filter,omitempty"`
	After             string            `json:"after,omitempty"`
	Limit             int               `json:"limit,omitempty"`
	ConversationID    string            `json:"conversation_id,omitempty"`
	ExpectedRevision  string            `json:"expected_revision,omitempty"`
	ExpectedSelection *int              `json:"expected_selection,omitempty"`
	Document          *TrainingDocument `json:"document,omitempty"`
	Key               string            `json:"key,omitempty"`
	Name              string            `json:"name,omitempty"`
	Audience          string            `json:"audience,omitempty"`
	MemberID          string            `json:"member_id,omitempty"`
	Decision          string            `json:"decision,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	FreePublication   *bool             `json:"free_publication,omitempty"`
	PersonalLearning  *bool             `json:"personal_learning,omitempty"`
}

func TrainingReadAction(action string) bool {
	switch action {
	case "list_training_library", "get_training", "get_conversation_context", "preview_training_edit", "get_training_policy", "find_training_members", "get_training_groups":
		return true
	}
	return false
}

func ValidTrainingAction(action string) bool {
	if TrainingReadAction(action) {
		return true
	}
	switch action {
	case "select_conversation_training", "save_training_edit", "propose_training_edit", "share_training", "submit_training", "decide_training", "publish_training", "set_training_access", "set_training_policy", "join_training":
		return true
	}
	return false
}
