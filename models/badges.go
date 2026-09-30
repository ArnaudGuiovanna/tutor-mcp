// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

const BadgePolicyVersion = "institution-badges-v1"

const (
	BadgeMastery   = "mastery"
	BadgeFormation = "formation_completed"
	BadgeRetention = "retention"
)

// Badges attest dated achievements. They do not assert current memory strength
// or confer an external qualification. Evidence contains identifiers, not answers.
type Badge struct {
	ID             string          `json:"id"`
	EnrollmentID   string          `json:"enrollment_id"`
	VersionID      string          `json:"version_id"`
	ConceptID      string          `json:"concept_id,omitempty"`
	Label          string          `json:"label"`
	Kind           string          `json:"kind"`
	RetentionDays  int             `json:"retention_days,omitempty"`
	PolicyVersion  string          `json:"policy_version"`
	AchievedAt     time.Time       `json:"achieved_at"`
	RecordedAt     time.Time       `json:"recorded_at"`
	EvidenceStatus string          `json:"evidence_status"`
	Evidence       []BadgeEvidence `json:"evidence"`
}

type BadgeEvidence struct {
	AttemptID        string           `json:"attempt_id"`
	ConceptID        string           `json:"concept_id"`
	SnapshotID       int64            `json:"snapshot_id,omitempty"`
	RespondedAt      time.Time        `json:"responded_at"`
	EvaluatedAt      time.Time        `json:"evaluated_at"`
	EvaluationMethod EvaluationMethod `json:"evaluation_method"`
}

type BadgePage struct {
	Items     []Badge `json:"items"`
	NextAfter string  `json:"next_after"`
}
