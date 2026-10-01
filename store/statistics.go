// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"time"

	"tutor-mcp/models"
)

type StatisticsStore interface {
	GetCohortStatistics(context.Context, models.Principal, string, string, int) (*models.CohortStatistics, error)
}

// StatisticsWorkerStore is the worker-only writer of the statistics snapshots.
type StatisticsWorkerStore interface {
	RecomputeInstitutionStatistics(context.Context, models.TenantScope, time.Time) (int, error)
}
