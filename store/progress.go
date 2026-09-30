// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"tutor-mcp/models"
)

type ProgressStore interface {
	ListTrainerCohorts(context.Context, models.Principal, string, int) (models.TrainerCohortPage, error)
	GetCohortInsights(context.Context, models.Principal, string, string, string, int) (*models.CohortInsights, error)
	GetLearnerProgress(context.Context, models.Principal, string, string, int) (*models.LearnerProgress, error)
}

type ProgressAssignmentStore interface {
	SetCohortTrainerAssignment(context.Context, models.Principal, string, string, bool) error
}

var ErrInvalidProgressRequest = errors.New("invalid progress request")
var ErrProgressUnavailable = errors.New("progress unavailable")

type ProgressError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func PublicProgressError(err error) ProgressError {
	switch {
	case errors.Is(err, ErrInvalidProgressRequest):
		return ProgressError{Code: "invalid_request", Message: "Use valid identifiers and a page size between 1 and 100."}
	case errors.Is(err, ErrInvalidPrincipal):
		return ProgressError{Code: "forbidden", Message: "Current staff membership and progress read access are required."}
	case errors.Is(err, ErrNotFound):
		return ProgressError{Code: "not_found", Message: "The requested progress is not available in your assigned scope."}
	default:
		return ProgressError{Code: "unavailable", Message: "Progress is temporarily unavailable. Try again later.", Retryable: true}
	}
}
