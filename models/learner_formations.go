// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

// LearnerFormation exposes a cohort and only the requesting learner's state.
type LearnerFormation struct {
	FormationID      string      `json:"formation_id"`
	Name             string      `json:"name"`
	Description      string      `json:"description"`
	FormationStatus  string      `json:"formation_status"`
	EnrollmentPolicy string      `json:"enrollment_policy"`
	CohortID         string      `json:"cohort_id"`
	CohortName       string      `json:"cohort_name"`
	CohortStatus     string      `json:"cohort_status"`
	VersionID        string      `json:"version_id"`
	Version          int64       `json:"version"`
	AvailableSeats   int         `json:"available_seats"`
	StartsAt         *time.Time  `json:"starts_at,omitempty"`
	EndsAt           *time.Time  `json:"ends_at,omitempty"`
	AdmissionStatus  string      `json:"admission_status,omitempty"`
	Enrollment       *Enrollment `json:"enrollment,omitempty"`
}

type LearnerFormationPage struct {
	Items     []LearnerFormation `json:"items"`
	NextAfter string             `json:"next_after"`
}

type LearnerFormationDetail struct {
	Offering LearnerFormation   `json:"offering"`
	Modules  []FormationModule  `json:"modules"`
	Concepts []FormationConcept `json:"concepts"`
}

type FormationJoinResult struct {
	Status     string      `json:"status"`
	Enrollment *Enrollment `json:"enrollment,omitempty"`
}

type FormationAdmission struct {
	EnrollmentPolicy string    `json:"enrollment_policy,omitempty"`
	Email            string    `json:"email,omitempty"`
	CohortID         string    `json:"cohort_id"`
	MembershipID     string    `json:"membership_id"`
	Status           string    `json:"status"`
	Version          int64     `json:"version"`
	UpdatedAt        time.Time `json:"updated_at"`
}
