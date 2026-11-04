// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
)

func maintenanceFixture(t *testing.T, count int) (*Store, models.TenantScope, time.Time) {
	t.Helper()
	s := narrativeTestStore(t)
	scope := models.LegacyPrincipal("L1").TenantScope()
	now := time.Now().UTC().Add(time.Second)
	for i := 0; i < count; i++ {
		key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: fmt.Sprintf("2026-04-%02dT10-00-00Z", i+1)}
		if _, _, err := s.CompareAndSwapNarrative(t.Context(), key, 0, fmt.Sprintf("---\nconcepts_touched: []\n---\nPrivate source %d", i), "", "", narrativeTestLimits()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertPendingConsolidation(t.Context(), "L1", "monthly", "2026-04", now); err != nil {
		t.Fatal(err)
	}
	return s, scope, now
}

func TestMemoryMaintenanceBoundedAtomicResume(t *testing.T) {
	s, scope, now := maintenanceFixture(t, 9)
	first, err := s.ClaimMemoryMaintenance(t.Context(), scope, "claim-1", now)
	if err != nil || first.Status != "claimed" || len(first.Sessions) != 8 || !first.HasMore {
		t.Fatalf("claim=%+v err=%v", first, err)
	}
	retry, err := s.ClaimMemoryMaintenance(t.Context(), scope, "claim-1", now.Add(time.Second))
	if err != nil || retry.ClaimToken != first.ClaimToken || len(retry.Sessions) != 8 {
		t.Fatal("lost claim receipt", err)
	}
	busy, err := s.ClaimMemoryMaintenance(t.Context(), scope, "claim-2", now.Add(time.Second))
	if err != nil || busy.Status != "busy" {
		t.Fatal("second writer claimed", err)
	}
	if _, err := s.CommitMemoryMaintenance(t.Context(), scope, first.ClaimToken, "Partial private summary", nil, now.Add(time.Second)); err == nil {
		t.Fatal("missing patches committed")
	}
	job, _ := s.GetConsolidation(t.Context(), "L1", "monthly", "2026-04")
	if job.Status != "delivered" {
		t.Fatal("failed commit advanced job")
	}
	for i := range first.Snapshots {
		first.Snapshots[i].Content = "Validated first batch"
	}
	result, err := s.CommitMemoryMaintenance(t.Context(), scope, first.ClaimToken, "Partial private summary", first.Snapshots, now.Add(2*time.Second))
	if err != nil || !result.OK || result.Completed {
		t.Fatalf("partial=%+v %v", result, err)
	}
	if _, err := s.GetNarrative(t.Context(), memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeArchive, Key: "2026-04"}); !errors.Is(err, memory.ErrNarrativeNotFound) {
		t.Fatal("partial archive became canonical", err)
	}
	var state, manifest string
	if err := s.RawDB().QueryRow(rb(s, "SELECT maintenance_state,maintenance_manifest FROM pending_consolidations WHERE id=?"), first.JobID).Scan(&state, &manifest); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state, "Partial private") || strings.Contains(manifest, "Partial private") {
		t.Fatal("plaintext checkpoint")
	}
	replayed, err := s.CommitMemoryMaintenance(t.Context(), scope, first.ClaimToken, "Partial private summary", first.Snapshots, now.Add(3*time.Second))
	if err != nil || !replayed.Replayed {
		t.Fatal("commit retry lost", err)
	}
	expired, err := s.ClaimMemoryMaintenance(t.Context(), scope, "claim-1", now.Add(3*time.Second))
	if err != nil || expired.Status != "expired" {
		t.Fatal("replayed a consumed lease", err)
	}
	last, err := s.ClaimMemoryMaintenance(t.Context(), scope, "claim-3", now.Add(4*time.Second))
	if err != nil || len(last.Sessions) != 1 || last.HasMore || last.ArchiveSummary != "Partial private summary" {
		t.Fatalf("resume=%+v err=%v", last, err)
	}
	result, err = s.CommitMemoryMaintenance(t.Context(), scope, last.ClaimToken, "All nine sessions summarized", last.Snapshots, now.Add(5*time.Second))
	if err != nil || !result.Completed {
		t.Fatalf("final=%+v %v", result, err)
	}
	archive, err := s.GetNarrative(t.Context(), memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeArchive, Key: "2026-04"})
	if err != nil || archive.Content != "All nine sessions summarized" {
		t.Fatal("missing final archive", err)
	}
}

func TestMemoryMaintenanceLeaseAndVersionConflicts(t *testing.T) {
	s, scope, now := maintenanceFixture(t, 1)
	batch, err := s.ClaimMemoryMaintenance(t.Context(), scope, "claim-1", now)
	if err != nil {
		t.Fatal(err)
	}
	other := models.LegacyPrincipal("L2").TenantScope()
	if _, err := s.CommitMemoryMaintenance(t.Context(), other, batch.ClaimToken, "Foreign", batch.Snapshots, now.Add(time.Second)); err == nil {
		t.Fatal("foreign learner committed")
	}
	key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeMemory}
	if _, _, err := s.CompareAndSwapNarrative(t.Context(), key, 0, "New observation during generation", "", "", narrativeTestLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitMemoryMaintenance(t.Context(), scope, batch.ClaimToken, "Stale", batch.Snapshots, now.Add(time.Second)); !errors.Is(err, models.ErrMemoryMaintenanceConflict) {
		t.Fatalf("stale commit=%v", err)
	}
	if _, err := s.CommitMemoryMaintenance(t.Context(), scope, batch.ClaimToken, "Expired", batch.Snapshots, now.Add(6*time.Minute)); !errors.Is(err, models.ErrMemoryMaintenanceLease) {
		t.Fatalf("expired commit=%v", err)
	}
	newBatch, err := s.ClaimMemoryMaintenance(t.Context(), scope, "claim-2", now.Add(6*time.Minute))
	if err != nil || newBatch.Status != "claimed" || newBatch.ClaimToken == batch.ClaimToken {
		t.Fatalf("recovery=%+v %v", newBatch, err)
	}
	if _, err := s.CommitMemoryMaintenance(t.Context(), scope, batch.ClaimToken, "Replaced", batch.Snapshots, now.Add(6*time.Minute)); !errors.Is(err, models.ErrMemoryMaintenanceLease) {
		t.Fatal("replaced lease accepted")
	}
	if _, err := s.CommitMemoryMaintenance(t.Context(), scope, newBatch.ClaimToken, "Fresh", newBatch.Snapshots, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetNarrative(t.Context(), key)
	if err != nil || current.Content != "New observation during generation" {
		t.Fatal("overwrote concurrent observation", err)
	}
}

func TestMemoryMaintenanceShrinksBatchWithoutSkippingSources(t *testing.T) {
	s, scope, now := maintenanceFixture(t, 8)
	for i := 0; i < 8; i++ {
		key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: fmt.Sprintf("2026-04-%02dT10-00-00Z", i+1)}
		if _, _, err := s.CompareAndSwapNarrative(t.Context(), key, 1, "---\nconcepts_touched: []\n---\n"+strings.Repeat("x", 18*1024), "", "", narrativeTestLimits()); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := s.ClaimMemoryMaintenance(t.Context(), scope, "large-claim", now)
	if err != nil || len(batch.Sessions) != 4 || !batch.HasMore {
		t.Fatalf("batch=%+v err=%v", batch, err)
	}
	if _, err := s.CommitMemoryMaintenance(t.Context(), scope, batch.ClaimToken, "First four", batch.Snapshots, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	next, err := s.ClaimMemoryMaintenance(t.Context(), scope, "large-next", now.Add(2*time.Second))
	if err != nil || len(next.Sessions) != 4 || next.HasMore || next.Sessions[0].Key != "2026-04-05T10-00-00Z" {
		t.Fatalf("next=%+v err=%v", next, err)
	}
}

func TestMemoryMaintenancePreservesArchiveEditsAcrossBatches(t *testing.T) {
	s, scope, now := maintenanceFixture(t, 9)
	key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeArchive, Key: "2026-04"}
	if _, _, err := s.CompareAndSwapNarrative(t.Context(), key, 0, "Existing archive", "", "", narrativeTestLimits()); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimMemoryMaintenance(t.Context(), scope, "archive-first", now)
	if err != nil || first.ArchiveSummary != "Existing archive" {
		t.Fatalf("missing existing archive: %+v %v", first, err)
	}
	retry, err := s.ClaimMemoryMaintenance(t.Context(), scope, "archive-first", now)
	if err != nil || retry.ArchiveSummary != first.ArchiveSummary {
		t.Fatalf("replay lost archive: %+v %v", retry, err)
	}
	if _, err := s.CommitMemoryMaintenance(t.Context(), scope, first.ClaimToken, "Cumulative first batch", first.Snapshots, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CompareAndSwapNarrative(t.Context(), key, 1, "Archive corrected between batches", "", "", narrativeTestLimits()); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimMemoryMaintenance(t.Context(), scope, "archive-second", now.Add(2*time.Second))
	if err != nil || len(second.Sessions) != 8 || second.ArchiveSummary != "Archive corrected between batches" {
		t.Fatalf("stale cumulative archive reused: %+v %v", second, err)
	}
	if _, _, err := s.CompareAndSwapNarrative(t.Context(), key, 2, "Archive corrected during generation", "", "", narrativeTestLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimMemoryMaintenance(t.Context(), scope, "archive-second", now.Add(3*time.Second)); !errors.Is(err, models.ErrMemoryMaintenanceConflict) {
		t.Fatalf("stale claim replayed: %v", err)
	}
	if _, err := s.CommitMemoryMaintenance(t.Context(), scope, second.ClaimToken, "Stale summary", second.Snapshots, now.Add(3*time.Second)); !errors.Is(err, models.ErrMemoryMaintenanceConflict) {
		t.Fatalf("archive overwritten: %v", err)
	}
}
