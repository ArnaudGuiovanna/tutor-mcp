// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type statisticsRecorder struct {
	storeport.Store
	scopes []models.TenantScope
	err    error
}

func (r *statisticsRecorder) RecomputeInstitutionStatistics(_ context.Context, scope models.TenantScope, _ time.Time) (int, error) {
	r.scopes = append(r.scopes, scope)
	return 1, r.err
}

func TestRefreshInstitutionStatisticsRunsInsideTheTenantScope(t *testing.T) {
	recorder := &statisticsRecorder{}
	scheduler := NewScheduler(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if result := scheduler.refreshInstitutionStatistics(); !result.failed() || result.FailureCode != "institution_statistics_store_unavailable" {
		t.Fatalf("job ran without a tenant scope: %+v", result)
	}
	scope := models.TenantScope{TenantID: "tenant-a", MembershipID: "m", UserID: "u"}
	scheduler.currentTenantScope = &scope
	if result := scheduler.refreshInstitutionStatistics(); result.failed() {
		t.Fatalf("refresh failed: %+v", result)
	}
	if len(recorder.scopes) != 1 || recorder.scopes[0].TenantID != "tenant-a" {
		t.Fatalf("refresh must stay in the worker's tenant scope: %+v", recorder.scopes)
	}
	recorder.err = errors.New("database detail that must not leak")
	if result := scheduler.refreshInstitutionStatistics(); !result.failed() || result.FailureCode != "institution_statistics_failed" {
		t.Fatalf("failure must carry a stable code: %+v", result)
	}
}
