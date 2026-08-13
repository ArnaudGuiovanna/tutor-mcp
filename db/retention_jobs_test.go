// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/memory"
)

func TestRetentionJobResumesExpiredLeaseAndCheckpointsDatabaseAtomically(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	policy := RetentionPolicy{OperationalEventLogDays: 30}
	if _, err := store.CreateOrResumeRetentionJob(
		ctx, "retention-resume", policy, now, "backup:resume", now.Add(-time.Hour), "operator", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Exec(rb(store,
		`INSERT INTO scheduled_alerts (learner_id, alert_type, scheduled_at, created_at)
		 VALUES (?, 'old', ?, ?)`), "L1", now.AddDate(0, 0, -60), now.AddDate(0, 0, -60)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRetentionJob(ctx, "retention-resume", "worker-one", now); err != nil {
		t.Fatal(err)
	}
	started, err := store.StartRetentionJobPhase(ctx, "retention-resume", "worker-one", RetentionPhaseDatabase, now)
	if err != nil || !started {
		t.Fatalf("start first phase=%v err=%v", started, err)
	}
	// Simulate a process dying before the atomic DB phase begins. A different
	// worker may resume only after the durable lease expires.
	if _, err := store.root.Exec(rb(store,
		`UPDATE retention_jobs SET leased_until = ? WHERE job_id = ?`), now.Add(-time.Second), "retention-resume"); err != nil {
		t.Fatal(err)
	}
	resumedAt := now.Add(time.Minute)
	if _, err := store.ClaimRetentionJob(ctx, "retention-resume", "worker-two", resumedAt); err != nil {
		t.Fatal(err)
	}
	started, err = store.StartRetentionJobPhase(ctx, "retention-resume", "worker-two", RetentionPhaseDatabase, resumedAt)
	if err != nil || !started {
		t.Fatalf("resume phase=%v err=%v", started, err)
	}
	report, err := store.ApplyRetentionDatabaseJobPhase(ctx, "retention-resume", "worker-two", policy, resumedAt)
	if err != nil || report.ScheduledAlertEvents.Applied != 1 {
		t.Fatalf("database phase report=%+v err=%v", report, err)
	}
	if started, err := store.StartRetentionJobPhase(ctx, "retention-resume", "worker-two", RetentionPhaseNarrative, resumedAt); err != nil || started {
		t.Fatalf("skipped narrative phase started=%v err=%v", started, err)
	}
	if err := store.CompleteRetentionJob(ctx, "retention-resume", "worker-two", `{"result":"complete"}`, resumedAt); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetRetentionJob(ctx, "retention-resume")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "completed" || job.AttemptCount != 2 || len(job.Phases) != 2 ||
		job.Phases[0].Status != "completed" || job.Phases[0].AttemptCount != 2 || job.Phases[0].Applied != 1 {
		t.Fatalf("resumed job=%+v", job)
	}
	var alerts int
	if err := store.root.QueryRow(`SELECT COUNT(*) FROM scheduled_alerts`).Scan(&alerts); err != nil || alerts != 0 {
		t.Fatalf("scheduled alerts=%d err=%v", alerts, err)
	}
}

func TestRetentionJobManifestRejectsStaleBackupAndDrift(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	policy := RetentionPolicy{OperationalEventLogDays: 30}
	if _, err := store.CreateOrResumeRetentionJob(
		ctx, "stale-backup", policy, now, "backup:old", now.Add(-RetentionBackupMaxAge-time.Second), "operator", now,
	); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("stale backup accepted: %v", err)
	}
	if _, err := store.CreateOrResumeRetentionJob(
		ctx, "manifest", policy, now, "backup:one", now.Add(-time.Hour), "operator", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrResumeRetentionJob(
		ctx, "manifest", RetentionPolicy{OperationalEventLogDays: 31}, now,
		"backup:one", now.Add(-time.Hour), "operator", now,
	); !errors.Is(err, ErrRetentionJobManifestConflict) {
		t.Fatalf("policy drift error=%v", err)
	}
}

func TestRetentionJobOnlyOneLiveLeaseOwner(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	policy := RetentionPolicy{OperationalEventLogDays: 30}
	if _, err := store.CreateOrResumeRetentionJob(ctx, "lease-race", policy, now, "backup:race", now.Add(-time.Hour), "operator", now); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for index := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.ClaimRetentionJob(ctx, "lease-race", fmt.Sprintf("worker-%d", index), now)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	winners, leased := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrRetentionJobInProgress):
			leased++
		default:
			t.Fatalf("claim error=%v", err)
		}
	}
	if winners != 1 || leased != 1 {
		t.Fatalf("claim winners=%d leased=%d", winners, leased)
	}
}

func TestRetentionJobPersistsPartialFailureAndResumesNextPhase(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	policy := RetentionPolicy{OperationalEventLogDays: 30, NarrativeMemoryDays: 30}
	if _, err := store.CreateOrResumeRetentionJob(
		ctx, "retention-partial", policy, now, "backup:partial", now.Add(-time.Hour), "operator", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRetentionJob(ctx, "retention-partial", "worker-one", now); err != nil {
		t.Fatal(err)
	}
	if started, err := store.StartRetentionJobPhase(ctx, "retention-partial", "worker-one", RetentionPhaseDatabase, now); err != nil || !started {
		t.Fatalf("start database phase=%v err=%v", started, err)
	}
	if _, err := store.ApplyRetentionDatabaseJobPhase(ctx, "retention-partial", "worker-one", policy, now); err != nil {
		t.Fatal(err)
	}
	if started, err := store.StartRetentionJobPhase(ctx, "retention-partial", "worker-one", RetentionPhaseNarrative, now); err != nil || !started {
		t.Fatalf("start narrative phase=%v err=%v", started, err)
	}
	injected := errors.New("injected narrative storage failure")
	if err := store.FailRetentionJobPhase(ctx, "retention-partial", "worker-one", RetentionPhaseNarrative, injected, now); err != nil {
		t.Fatal(err)
	}
	failed, err := store.GetRetentionJob(ctx, "retention-partial")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.LeaseOwner != "" || failed.LeasedUntil != nil ||
		failed.LastError != injected.Error() || failed.Phases[0].Status != "completed" ||
		failed.Phases[1].Status != "failed" || failed.Phases[1].LastError != injected.Error() {
		t.Fatalf("partial failure checkpoint=%+v", failed)
	}

	resumedAt := now.Add(time.Minute)
	if _, err := store.ClaimRetentionJob(ctx, "retention-partial", "worker-two", resumedAt); err != nil {
		t.Fatal(err)
	}
	if started, err := store.StartRetentionJobPhase(ctx, "retention-partial", "worker-two", RetentionPhaseDatabase, resumedAt); err != nil || started {
		t.Fatalf("completed database replay started=%v err=%v", started, err)
	}
	if started, err := store.StartRetentionJobPhase(ctx, "retention-partial", "worker-two", RetentionPhaseNarrative, resumedAt); err != nil || !started {
		t.Fatalf("resume narrative phase=%v err=%v", started, err)
	}
	if err := store.CompleteRetentionJobPhase(
		ctx, "retention-partial", "worker-two", RetentionPhaseNarrative,
		2, 1, 1, `{"eligible":2,"applied":1,"held":1}`, resumedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRetentionJob(ctx, "retention-partial", "worker-two", `{"result":"complete"}`, resumedAt); err != nil {
		t.Fatal(err)
	}
	completed, err := store.GetRetentionJob(ctx, "retention-partial")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" || completed.AttemptCount != 2 ||
		completed.Phases[0].AttemptCount != 1 || completed.Phases[1].AttemptCount != 2 ||
		completed.Phases[1].Eligible != 2 || completed.Phases[1].Applied != 1 || completed.Phases[1].Held != 1 {
		t.Fatalf("resumed partial job=%+v", completed)
	}
}

func TestRetentionLegalHoldProtectsEverySelectedLearnerRow(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	if _, err := store.root.Exec(rb(store,
		`INSERT INTO learners (id, email, password_hash, objective, created_at, email_verified_at)
		 VALUES (?, ?, 'hash', 'test', ?, ?)`), "L2", "l2-hold@test.invalid", now, now); err != nil {
		t.Fatal(err)
	}
	for _, learnerID := range []string{"L1", "L2"} {
		if _, err := store.root.Exec(rb(store,
			`INSERT INTO scheduled_alerts (learner_id, alert_type, scheduled_at, created_at)
			 VALUES (?, 'old', ?, ?)`), learnerID, now.AddDate(0, 0, -60), now.AddDate(0, 0, -60)); err != nil {
			t.Fatal(err)
		}
	}
	hold := RetentionLegalHold{
		HoldID: "litigation-L1", LearnerID: "L1", Reason: "active litigation",
		CreatedBy: "legal", CreatedAt: now,
	}
	if err := store.CreateRetentionLegalHold(ctx, hold); err != nil {
		t.Fatal(err)
	}
	policy := RetentionPolicy{OperationalEventLogDays: 30}
	dry, err := store.RunDataRetention(ctx, policy, now, false)
	if err != nil || dry.ScheduledAlertEvents.Eligible != 1 || dry.ScheduledAlertEvents.Held != 1 {
		t.Fatalf("held dry-run=%+v err=%v", dry.ScheduledAlertEvents, err)
	}
	applied, err := store.RunDataRetention(ctx, policy, now, true)
	if err != nil || applied.ScheduledAlertEvents.Applied != 1 || applied.ScheduledAlertEvents.Held != 1 {
		t.Fatalf("held apply=%+v err=%v", applied.ScheduledAlertEvents, err)
	}
	var remainingLearner string
	if err := store.root.QueryRow(`SELECT learner_id FROM scheduled_alerts`).Scan(&remainingLearner); err != nil || remainingLearner != "L1" {
		t.Fatalf("remaining learner=%q err=%v", remainingLearner, err)
	}
	released, err := store.ReleaseRetentionLegalHold(ctx, hold.HoldID, "legal", "matter closed", now.Add(time.Hour))
	if err != nil || !released {
		t.Fatalf("release applied=%v err=%v", released, err)
	}
	final, err := store.RunDataRetention(ctx, policy, now.Add(2*time.Hour), true)
	if err != nil || final.ScheduledAlertEvents.Applied != 1 || final.ScheduledAlertEvents.Held != 0 {
		t.Fatalf("post-release apply=%+v err=%v", final.ScheduledAlertEvents, err)
	}
}

func TestRetentionLegalHoldProtectsSharedNarrativeAndMutationJournal(t *testing.T) {
	store := narrativeTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	if _, err := store.root.Exec(rb(store,
		`INSERT INTO learners (id, email, password_hash, objective, created_at, email_verified_at)
		 VALUES (?, ?, 'hash', 'test', ?, ?)`), "L2", "l2-narrative-hold@test.invalid", now, now); err != nil {
		t.Fatal(err)
	}
	for _, learnerID := range []string{"L1", "L2"} {
		if _, _, err := store.CompareAndSwapNarrative(
			ctx, memory.NarrativeKey{LearnerID: learnerID, Scope: memory.ScopeMemory},
			0, "old", "mutation-"+learnerID, narrativeChecksum("mutation-"+learnerID), narrativeTestLimits(),
		); err != nil {
			t.Fatal(err)
		}
	}
	old := now.AddDate(0, 0, -60)
	if _, err := store.root.Exec(rb(store, `UPDATE narrative_objects SET updated_at = ?`), old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Exec(rb(store, `UPDATE narrative_mutations SET created_at = ?`), old); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRetentionLegalHold(ctx, RetentionLegalHold{
		HoldID: "narrative-L1", LearnerID: "L1", Reason: "preserve narrative",
		CreatedBy: "legal", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	eligible, applied, held, err := store.DeleteNarrativesBefore(ctx, now.AddDate(0, 0, -30), true)
	if err != nil || eligible != 1 || applied != 1 || held != 1 {
		t.Fatalf("narrative retention eligible=%d applied=%d held=%d err=%v", eligible, applied, held, err)
	}
	var heldObjects, heldMutations int
	if err := store.root.QueryRow(rb(store, `SELECT COUNT(*) FROM narrative_objects WHERE learner_id = ?`), "L1").Scan(&heldObjects); err != nil {
		t.Fatal(err)
	}
	if err := store.root.QueryRow(rb(store, `SELECT COUNT(*) FROM narrative_mutations WHERE learner_id = ?`), "L1").Scan(&heldMutations); err != nil {
		t.Fatal(err)
	}
	if heldObjects != 1 || heldMutations != 1 {
		t.Fatalf("held narrative objects=%d mutations=%d", heldObjects, heldMutations)
	}
}
