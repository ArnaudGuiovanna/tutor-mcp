// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"tutor-mcp/models"
)

type BadgeStore interface {
	GetEnrollmentBadges(context.Context, models.Principal, string, string, int, bool) (models.BadgePage, error)
}
