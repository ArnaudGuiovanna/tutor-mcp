// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import (
	"encoding/json"
	"time"
)

type CurriculumReviewMaterial struct {
	Snapshot       *CurriculumSnapshot `json:"snapshot"`
	MaterialHash   string              `json:"material_hash"`
	CurrentVersion int                 `json:"current_version"`
}

type CurriculumFinding struct {
	ConceptID        string `json:"concept_id"`
	Aspect           string `json:"aspect"`
	RelatedConceptID string `json:"related_concept_id,omitempty"`
	Judgment         string `json:"judgment"`
	Rationale        string `json:"rationale"`
}

type CurriculumReviewOpinion struct {
	ID               string          `json:"id"`
	DomainID         string          `json:"domain_id"`
	Version          int             `json:"version"`
	ReviewerUserID   string          `json:"reviewer_user_id"`
	MaterialHash     string          `json:"material_hash"`
	FindingsHash     string          `json:"findings_hash"`
	Findings         json.RawMessage `json:"findings"`
	Disposition      string          `json:"disposition"`
	ReviewedSections int             `json:"reviewed_sections"`
	TotalSections    int             `json:"total_sections"`
	Certified        bool            `json:"certified"`
	CreatedAt        time.Time       `json:"created_at"`
}
