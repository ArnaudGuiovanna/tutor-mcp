// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package store

import "errors"

var (
	ErrInvalidAssessmentReviewRequest      = errors.New("invalid assessment review request")
	ErrAssessmentReviewMaterialUnavailable = errors.New("frozen assessment review material unavailable")
)
