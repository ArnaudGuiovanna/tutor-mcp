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

// CollectiveWeightStore applies the current collective BKT weights to a locked
// learner state inside the interaction transaction. It never persists state.
type CollectiveWeightStore interface {
	ApplyCollectiveWeights(context.Context, models.TenantScope, *models.ConceptState, time.Time) (*models.CollectiveWeightApplication, error)
}

// CollectiveWeightWorkerStore is the worker-only publisher of weight versions.
type CollectiveWeightWorkerStore interface {
	RecomputeCollectiveWeights(context.Context, models.TenantScope, time.Time) (int, error)
}
