// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func optedInAvailability(learnerID string) *models.Availability {
	return &models.Availability{
		LearnerID:              learnerID,
		Timezone:               "UTC",
		WindowsJSON:            "[]",
		AvgDuration:            30,
		SessionsWeek:           3,
		NotificationConsent:    true,
		NotificationFrequency:  models.NotificationFrequencyAsScheduled,
		MaxNotificationsPerDay: 10,
		AccessibilityJSON:      "{}",
	}
}

func TestUpdateAvailabilityOptimisticVersionAndOwnership(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	defaults, err := store.GetAvailability(ctx, "L1")
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Version != 0 || defaults.NotificationConsent || defaults.Timezone != "UTC" {
		t.Fatalf("unsafe defaults: %+v", defaults)
	}

	a := optedInAvailability("L1")
	created, err := store.UpdateAvailability(ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || !created.NotificationConsent {
		t.Fatalf("created policy: %+v", created)
	}
	if _, err := store.UpdateAvailability(ctx, a, 0); !errors.Is(err, storeport.ErrAvailabilityVersionConflict) {
		t.Fatalf("second create err=%v, want version conflict", err)
	}
	a.DoNotDisturb = true
	updated, err := store.UpdateAvailability(ctx, a, 1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || !updated.DoNotDisturb {
		t.Fatalf("updated policy: %+v", updated)
	}
	foreign := optedInAvailability("missing")
	if _, err := store.UpdateAvailability(ctx, foreign, 0); err == nil {
		t.Fatal("expected missing/foreign learner update to fail")
	}
}

func TestReserveNotificationDeliveryConsentDNDWindowFrequencyAndCap(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, time.May, 4, 9, 30, 0, 0, time.UTC) // Monday

	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "A", "", false, at); err != nil || reserved {
		t.Fatalf("default no-consent reservation: reserved=%v err=%v", reserved, err)
	}
	a := optedInAvailability("L1")
	a.WindowsJSON = `[{"day":"monday","start":"09:00","end":"10:00"}]`
	a.MaxNotificationsPerDay = 1
	if err := store.UpsertAvailability(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "OUTSIDE", "", false, at.Add(2*time.Hour)); err != nil || reserved {
		t.Fatalf("outside window: reserved=%v err=%v", reserved, err)
	}
	id, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "A", "", false, at)
	if err != nil || !reserved {
		t.Fatalf("first reservation: id=%d reserved=%v err=%v", id, reserved, err)
	}
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "B", "", false, at); err != nil || reserved {
		t.Fatalf("daily cap: reserved=%v err=%v", reserved, err)
	}
	if err := store.ReleaseNotificationDelivery(ctx, id, "L1"); err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "B", "", false, at); err != nil || !reserved {
		t.Fatalf("slot should reopen after release: reserved=%v err=%v", reserved, err)
	}

	a.DoNotDisturb = true
	if err := store.UpsertAvailability(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "NEXT", "", false, at.AddDate(0, 0, 7)); err != nil || reserved {
		t.Fatalf("DND reservation: reserved=%v err=%v", reserved, err)
	}

	a.DoNotDisturb = false
	a.MaxNotificationsPerDay = 10
	a.NotificationFrequency = models.NotificationFrequencyWeekly
	a.WindowsJSON = "[]"
	if err := store.UpsertAvailability(ctx, a); err != nil {
		t.Fatal(err)
	}
	firstWeek := at.AddDate(0, 0, 7)
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "WEEK_A", "", false, firstWeek); err != nil || !reserved {
		t.Fatalf("weekly first: reserved=%v err=%v", reserved, err)
	}
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "WEEK_B", "", false, firstWeek.AddDate(0, 0, 1)); err != nil || reserved {
		t.Fatalf("weekly frequency: reserved=%v err=%v", reserved, err)
	}
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "WEEK_C", "", false, firstWeek.AddDate(0, 0, 7)); err != nil || !reserved {
		t.Fatalf("next week: reserved=%v err=%v", reserved, err)
	}
}

func TestWasAlertSentTodayUsesLearnerLocalCivilDay(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	a := optedInAvailability("L1")
	a.Timezone = "Pacific/Kiritimati"
	if err := store.UpsertAvailability(ctx, a); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	bounds, err := a.NotificationBounds(now)
	if err != nil {
		t.Fatal(err)
	}
	utcStart := now.Truncate(24 * time.Hour)
	createdAt := bounds.DayStart.Add(time.Minute)
	want := true
	if bounds.DayStart.After(utcStart) {
		// This instant belongs to the current UTC day but to the learner's
		// preceding local day. A UTC-only lookup would incorrectly count it.
		createdAt = bounds.DayStart.Add(-time.Minute)
		want = false
	}
	if _, err := store.root.ExecContext(ctx, rb(store,
		`INSERT INTO scheduled_alerts
		    (learner_id, alert_type, concept, scheduled_at, sent, created_at)
		 VALUES (?, 'LOCAL_DAY', '', ?, 1, ?)`),
		"L1", createdAt, createdAt,
	); err != nil {
		t.Fatal(err)
	}

	got, err := store.WasAlertSentToday(ctx, "L1", "LOCAL_DAY")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("WasAlertSentToday=%v, want %v for local day [%s, %s) and alert %s",
			got, want, bounds.DayStart, bounds.DayEnd, createdAt)
	}
}

func TestReserveNotificationDeliveryConcurrentCap(t *testing.T) {
	raw, err := OpenDB(filepath.Join(t.TempDir(), "availability-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := Migrate(raw); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES ('L1','l1@test','h','o',?)`,
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	store := NewStore(raw)
	a := optedInAvailability("L1")
	a.MaxNotificationsPerDay = 1
	if err := store.UpsertAvailability(ctx, a); err != nil {
		t.Fatal(err)
	}

	const contenders = 16
	var wins atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, contenders)
	at := time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "RACE_"+time.Duration(i).String(), "", false, at)
			if err != nil {
				errs <- err
				return
			}
			if reserved {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("reservation error: %v", err)
	}
	if got := wins.Load(); got != 1 {
		t.Fatalf("concurrent reservations won=%d, want exactly 1", got)
	}
}

func TestHighStakesRequiresTrustedHumanReviewForClaimsAndIntrusiveNotifications(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	domain, err := store.CreateDomain(ctx, "L1", "clinical safety", "", models.KnowledgeSpace{
		Concepts: []string{"triage"}, Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDomainHighStakes(ctx, domain.ID, "L1"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAvailability(ctx, optedInAvailability("L1")); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "HS", domain.ID, true, at); err != nil || reserved {
		t.Fatalf("unreviewed high-stakes reservation: reserved=%v err=%v", reserved, err)
	}

	createEvaluatedAttempt(t, store, domain.ID, "host", models.EvaluationMethodHostLLM, true)
	createEvaluatedAttempt(t, store, domain.ID, "external", models.EvaluationMethodExternal, true)
	got, err := store.GetTrustedPassedAssessmentAttemptsInDomain(ctx, "L1", domain.ID, "triage", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("host/external evaluation established high-stakes demonstration: %+v", got)
	}
	evaluated, err := store.GetEvaluatedAssessmentAttemptsInDomain(ctx, "L1", domain.ID, "triage", 10)
	if err != nil || len(evaluated) != 2 {
		t.Fatalf("evaluated high-stakes evidence=%+v err=%v", evaluated, err)
	}
	for _, attempt := range evaluated {
		if attempt.TrustedEvaluation {
			t.Fatalf("non-human high-stakes attempt retained effective trust: %+v", attempt)
		}
	}
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "HS_HOST", domain.ID, true, at); err != nil || reserved {
		t.Fatalf("host-reviewed high-stakes reservation: reserved=%v err=%v", reserved, err)
	}

	createEvaluatedAttempt(t, store, domain.ID, "human", models.EvaluationMethodHumanReview, true)
	got, err = store.GetTrustedPassedAssessmentAttemptsInDomain(ctx, "L1", domain.ID, "triage", 10)
	if err != nil || len(got) != 1 || got[0].EvaluationMethod != models.EvaluationMethodHumanReview {
		t.Fatalf("human-reviewed claim evidence=%+v err=%v", got, err)
	}
	evaluated, err = store.GetEvaluatedAssessmentAttemptsInDomain(ctx, "L1", domain.ID, "triage", 10)
	if err != nil || len(evaluated) != 3 || !evaluated[0].TrustedEvaluation || evaluated[0].EvaluationMethod != models.EvaluationMethodHumanReview {
		t.Fatalf("effective high-stakes evidence trust=%+v err=%v", evaluated, err)
	}
	if _, reserved, err := store.ReserveNotificationDelivery(ctx, "L1", "HS_HUMAN", domain.ID, true, at); err != nil || !reserved {
		t.Fatalf("human-reviewed high-stakes reservation: reserved=%v err=%v", reserved, err)
	}
	if err := store.MarkDomainHighStakes(ctx, domain.ID, "foreign"); err == nil {
		t.Fatal("foreign learner marked domain high stakes")
	}
}

func createEvaluatedAttempt(t *testing.T, store *Store, domainID, suffix string, method models.EvaluationMethod, trusted bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	id := "attempt_" + suffix
	a := &models.AssessmentAttempt{
		ID: id, LearnerID: "L1", DomainID: domainID, ConceptID: "triage",
		ActivityID: "activity_" + suffix, ActivityVersion: 1,
		ActivityType: "MASTERY_CHALLENGE", Observable: "safe triage",
		TaskText: "task", TaskContentHash: "hash", RubricJSON: `{}`,
		PassingScore: 1, Status: models.AssessmentAttemptPrepared, CreatedAt: now,
	}
	if err := store.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := store.SubmitAssessmentAttempt(ctx, "L1", id, "response", "response-hash", now); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAssessmentEvaluation(ctx, "L1", id, `{}`, "reviewer", models.EvaluationMethodHostLLM, `{}`, 1, true, now); err != nil {
		t.Fatal(err)
	}
	if trusted {
		// There is deliberately no production method that can mint trusted
		// evidence until an operator-controlled evaluator is configured. Seed
		// the read-side policy directly in this persistence-package test.
		if _, err := store.root.Exec(
			rb(store, `UPDATE assessment_attempts SET trusted_evaluation = 1, evaluation_method = ? WHERE id = ?`),
			string(method), id,
		); err != nil {
			t.Fatal(err)
		}
	}
}
