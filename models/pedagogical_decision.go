// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

// PedagogicalPolicyVersion identifies the rules that produced a decision.
// A decision is an immutable contract; generated tasks reference its ID.
const PedagogicalPolicyVersion = "2026-09-scoring-v4"

type PedagogicalDecision struct {
	ID                string              `json:"id"`
	LearnerID         string              `json:"learner_id"`
	DomainID          string              `json:"domain_id"`
	SessionID         string              `json:"session_id"`
	CurriculumVersion int                 `json:"curriculum_version"`
	PolicyVersion     string              `json:"policy_version"`
	Contract          PedagogicalContract `json:"contract"`
	ContextJSON       string              `json:"context_json"`
	CreatedAt         time.Time           `json:"created_at"`
}
