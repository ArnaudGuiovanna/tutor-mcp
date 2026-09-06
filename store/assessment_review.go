// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package store

import "errors"

var (
	ErrInvalidAssessmentReviewRequest      = errors.New("invalid assessment review request")
	ErrAssessmentReviewMaterialUnavailable = errors.New("frozen assessment review material unavailable")
	ErrAssessmentReviewConflict            = errors.New("assessment review already recorded or idempotency key reused")
	ErrAssessmentReviewMaterialChanged     = errors.New("assessment review material changed")
	ErrInvalidAssessmentReviewScore        = errors.New("invalid assessment review score")
)
