// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

type Formation struct {
	ID          string
	TenantID    string
	Name        string
	Description string
	Status      string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type FormationVersion struct {
	ID           string
	TenantID     string
	FormationID  string
	Version      int64
	Status       string
	MetadataJSON string
	CreatedBy    string
	CreatedAt    time.Time
	PublishedAt  *time.Time
}

type FormationModuleInput struct {
	StableKey    string
	Title        string
	Position     int
	MetadataJSON string
}

type FormationConceptInput struct {
	ModuleStableKey string
	StableKey       string
	Label           string
	Position        int
	MetadataJSON    string
	Prerequisites   []string
}

type Cohort struct {
	ID                 string
	TenantID           string
	FormationVersionID string
	Name               string
	StartsAt           *time.Time
	EndsAt             *time.Time
	Capacity           int
	ReservedSeats      int
	Status             string
	Version            int64
	CreatedBy          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Enrollment struct {
	ID                 string
	TenantID           string
	CohortID           string
	FormationVersionID string
	UserID             string
	MembershipID       string
	LearnerID          string
	Status             string
	ObjectivesJSON     string
	SeatReserved       bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CompletedAt        *time.Time
}

type FormationPage struct {
	Items     []Formation
	NextAfter string
}

type CohortPage struct {
	Items     []Cohort
	NextAfter string
}

type EnrollmentPage struct {
	Items     []Enrollment
	NextAfter string
}

type CohortReport struct {
	TenantID        string
	CohortID        string
	EnrollmentCount int64
	ActiveCount     int64
	CompletedCount  int64
	AverageMastery  float64
}
