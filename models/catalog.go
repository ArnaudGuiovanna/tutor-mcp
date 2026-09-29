// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

type Formation struct {
	OwnerMembershipID string    `json:"owner_membership_id"`
	EnrollmentPolicy  string    `json:"enrollment_policy"`
	ID                string    `json:"id"`
	TenantID          string    `json:"tenant_id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	Status            string    `json:"status"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type FormationVersion struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"tenant_id"`
	FormationID  string     `json:"formation_id"`
	Version      int64      `json:"version"`
	Status       string     `json:"status"`
	MetadataJSON string     `json:"metadata_json,omitempty"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	PublishedAt  *time.Time `json:"published_at"`
}

type FormationModuleInput struct {
	StableKey    string `json:"stable_key"`
	Title        string `json:"title"`
	Position     int    `json:"position"`
	MetadataJSON string `json:"metadata_json,omitempty"`
}

type FormationConceptInput struct {
	Description     string                `json:"description,omitempty"`
	Level           CurriculumLevel       `json:"level,omitempty"`
	Outcomes        []CurriculumOutcome   `json:"outcomes,omitempty"`
	Criteria        []CurriculumCriterion `json:"criteria,omitempty"`
	ModuleStableKey string                `json:"module_stable_key,omitempty"`
	StableKey       string                `json:"stable_key"`
	Label           string                `json:"label"`
	Position        int                   `json:"position"`
	MetadataJSON    string                `json:"metadata_json,omitempty"`
	Prerequisites   []string              `json:"prerequisites,omitempty"`
}

type Cohort struct {
	ID                 string     `json:"id"`
	TenantID           string     `json:"tenant_id"`
	FormationVersionID string     `json:"formation_version_id"`
	Name               string     `json:"name"`
	StartsAt           *time.Time `json:"starts_at"`
	EndsAt             *time.Time `json:"ends_at"`
	Capacity           int        `json:"capacity"`
	ReservedSeats      int        `json:"reserved_seats"`
	Status             string     `json:"status"`
	Version            int64      `json:"version"`
	CreatedBy          string     `json:"created_by"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type Enrollment struct {
	DomainID           string     `json:"domain_id,omitempty"`
	ID                 string     `json:"id"`
	TenantID           string     `json:"tenant_id"`
	CohortID           string     `json:"cohort_id"`
	FormationVersionID string     `json:"formation_version_id"`
	UserID             string     `json:"user_id"`
	MembershipID       string     `json:"membership_id"`
	LearnerID          string     `json:"learner_id"`
	Status             string     `json:"status"`
	ObjectivesJSON     string     `json:"objectives_json"`
	SeatReserved       bool       `json:"seat_reserved"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	CompletedAt        *time.Time `json:"completed_at"`
}

type FormationPage struct {
	Items     []Formation `json:"items"`
	NextAfter string      `json:"next_after"`
}

type CohortPage struct {
	Items     []Cohort `json:"items"`
	NextAfter string   `json:"next_after"`
}

type EnrollmentPage struct {
	Items     []Enrollment `json:"items"`
	NextAfter string       `json:"next_after"`
}

type CohortReport struct {
	TenantID        string  `json:"tenant_id"`
	CohortID        string  `json:"cohort_id"`
	EnrollmentCount int64   `json:"enrollment_count"`
	ActiveCount     int64   `json:"active_count"`
	CompletedCount  int64   `json:"completed_count"`
	AverageMastery  float64 `json:"average_mastery"`
}

// FormationVersionDetail is the complete authoring/read contract.
type FormationVersionDetail struct {
	Formation Formation          `json:"formation"`
	Version   FormationVersion   `json:"version"`
	Modules   []FormationModule  `json:"modules"`
	Concepts  []FormationConcept `json:"concepts"`
}
type FormationModule struct {
	ID string `json:"id"`
	FormationModuleInput
}
type FormationConcept struct {
	ID string `json:"id"`
	FormationConceptInput
}
type FormationMigration struct {
	SourceEnrollmentID     string     `json:"source_enrollment_id"`
	Enrollment             Enrollment `json:"enrollment"`
	InvalidatedConceptKeys []string   `json:"invalidated_concept_keys"`
}
