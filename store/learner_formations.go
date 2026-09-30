// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"tutor-mcp/models"
)

var ErrFormationAdmissionRequired = errors.New("formation admission is required")
var ErrFormationUnavailable = errors.New("formation is unavailable for enrollment")
var ErrEnrollmentTransition = errors.New("enrollment cannot make this transition")

type LearnerFormationStore interface {
	ListLearnerFormations(context.Context, models.Principal, bool, string, int) (models.LearnerFormationPage, error)
	GetLearnerFormation(context.Context, models.Principal, string) (*models.LearnerFormationDetail, error)
	JoinFormation(context.Context, models.Principal, string, string) (*models.FormationJoinResult, bool, error)
	LeaveFormation(context.Context, models.Principal, string, string) (*models.FormationJoinResult, bool, error)
}

type FormationAdmissionStore interface {
	FindFormationLearner(context.Context, models.Principal, string, string) (string, error)
	ListFormationAdmissions(context.Context, models.Principal, string, string, int) ([]models.FormationAdmission, error)
	DecideFormationAdmission(context.Context, models.Principal, string, string, string, string, int64) (*models.FormationAdmission, bool, error)
}
