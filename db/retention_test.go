// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestRetentionPolicyValidation(t *testing.T) {
	if !(RetentionPolicy{}).Enabled() {
		// Zero is intentionally a valid, preservation-only policy.
	} else {
		t.Fatal("zero retention policy must be disabled")
	}
	for _, policy := range []RetentionPolicy{
		{WebhookTerminalDays: -1},
		{AssessmentPlaintextDays: -1},
		{IdempotencyResponseDays: -1},
		{PedagogicalSnapshotDays: -1},
		{OperationalEventLogDays: -1},
		{WebhookTerminalDays: maxRetentionDays + 1},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("Validate(%+v) succeeded, want error", policy)
		}
	}
}

func TestRunDataRetention_DryRunThenApply(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -60)
	fresh := now.AddDate(0, 0, -5)
	seedRetentionDomainAndInteraction(t, store, now)

	oldSent := enqueueRetentionWebhook(t, store, "old-sent", models.WebhookStatusSent, old)
	enqueueRetentionWebhook(t, store, "old-failed", models.WebhookStatusFailed, old)
	enqueueRetentionWebhook(t, store, "old-expired", models.WebhookStatusExpired, old)
	freshSent := enqueueRetentionWebhook(t, store, "fresh-sent", models.WebhookStatusSent, fresh)
	oldPending := enqueueRetentionWebhook(t, store, "old-pending", models.WebhookStatusPending, old)
	oldProcessing := enqueueRetentionWebhook(t, store, "old-processing", models.WebhookStatusProcessing, old)

	if _, err := store.CreateWebhookPushLog(ctx, "L1", oldSent, &models.WebhookBrief{Kind: "test"}, now); err != nil {
		t.Fatalf("create linked push log: %v", err)
	}
	insertWebhookPushEvent(t, store, "old-event", old)
	insertWebhookPushEvent(t, store, "fresh-event", fresh)
	insertWebhookPushEventAt(t, store, "future-push-event", now.AddDate(0, 0, 60), old)
	insertScheduledAlertEvent(t, store, "old-alert", old)
	insertScheduledAlertEvent(t, store, "fresh-alert", fresh)
	insertScheduledAlertEventAt(t, store, "future-alert", now.AddDate(0, 0, 60), old)

	insertRetentionAssessment(t, store, "safe-old", models.AssessmentAttemptEvaluated, old, true)
	insertRetentionAssessment(t, store, "blocked-old", models.AssessmentAttemptEvaluated, old, false)
	insertRetentionAssessment(t, store, "active-old", models.AssessmentAttemptSubmitted, old, true)
	insertRetentionAssessment(t, store, "safe-fresh", models.AssessmentAttemptEvaluated, fresh, true)

	createRetentionSnapshot(t, store, "old", old)
	createRetentionSnapshot(t, store, "fresh", fresh)

	policy := RetentionPolicy{
		WebhookTerminalDays:     30,
		AssessmentPlaintextDays: 30,
		PedagogicalSnapshotDays: 30,
		OperationalEventLogDays: 30,
	}
	dry, err := store.RunDataRetention(ctx, policy, now, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.DryRun {
		t.Fatal("dry-run report must identify itself")
	}
	assertRetentionMetric(t, "terminal webhooks", dry.WebhookTerminalRows, 3, 0)
	assertRetentionMetric(t, "detached links", dry.WebhookPushLinksDetached, 1, 0)
	assertRetentionMetric(t, "task plaintext", dry.AssessmentTaskPlaintext, 1, 0)
	assertRetentionMetric(t, "response plaintext", dry.AssessmentResponsePlaintext, 1, 0)
	if dry.AssessmentPlaintextBlocked != 1 {
		t.Fatalf("blocked assessment rows = %d, want 1", dry.AssessmentPlaintextBlocked)
	}
	assertRetentionMetric(t, "snapshots", dry.PedagogicalSnapshots, 1, 0)
	assertRetentionMetric(t, "push events", dry.WebhookPushEvents, 1, 0)
	assertRetentionMetric(t, "alert events", dry.ScheduledAlertEvents, 1, 0)

	// Dry-run must be observational only.
	if got := retentionCountTable(t, store, "webhook_message_queue"); got != 6 {
		t.Fatalf("queue rows after dry-run = %d, want 6", got)
	}
	assertAssessmentPlaintext(t, store, "safe-old", true, true)

	applied, err := store.RunDataRetention(ctx, policy, now, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.DryRun {
		t.Fatal("apply report marked as dry-run")
	}
	assertRetentionMetric(t, "terminal webhooks", applied.WebhookTerminalRows, 3, 3)
	assertRetentionMetric(t, "detached links", applied.WebhookPushLinksDetached, 1, 1)
	assertRetentionMetric(t, "task plaintext", applied.AssessmentTaskPlaintext, 1, 1)
	assertRetentionMetric(t, "response plaintext", applied.AssessmentResponsePlaintext, 1, 1)
	assertRetentionMetric(t, "snapshots", applied.PedagogicalSnapshots, 1, 1)
	assertRetentionMetric(t, "push events", applied.WebhookPushEvents, 1, 1)
	assertRetentionMetric(t, "alert events", applied.ScheduledAlertEvents, 1, 1)

	for _, id := range []int64{freshSent, oldPending, oldProcessing} {
		if got := retentionCountWhereID(t, store, "webhook_message_queue", id); got != 1 {
			t.Errorf("preserved queue id=%d count=%d, want 1", id, got)
		}
	}
	if got := retentionCountTable(t, store, "webhook_message_queue"); got != 3 {
		t.Fatalf("queue rows after apply = %d, want 3", got)
	}
	var detachedQueueID int64
	if err := store.root.QueryRow(
		rb(store, `SELECT queue_id FROM webhook_push_log WHERE kind = ?`), "test",
	).Scan(&detachedQueueID); err != nil {
		t.Fatalf("read detached push link: %v", err)
	}
	if detachedQueueID != 0 {
		t.Fatalf("retained push log queue_id = %d, want 0", detachedQueueID)
	}

	assertAssessmentPlaintext(t, store, "safe-old", false, false)
	assertAssessmentPlaintext(t, store, "blocked-old", true, true)
	assertAssessmentPlaintext(t, store, "active-old", true, true)
	assertAssessmentPlaintext(t, store, "safe-fresh", true, true)
	var taskHash, responseHash string
	if err := store.root.QueryRow(
		rb(store, `SELECT task_content_hash, response_content_hash FROM assessment_attempts WHERE id = ?`), "safe-old",
	).Scan(&taskHash, &responseHash); err != nil {
		t.Fatal(err)
	}
	if taskHash == "" || responseHash == "" {
		t.Fatal("plaintext redaction must preserve assessment content hashes")
	}

	if got := retentionCountTable(t, store, "interactions"); got != 1 {
		t.Fatalf("snapshot retention deleted parent interaction: count=%d", got)
	}
	if got := retentionCountTable(t, store, "pedagogical_snapshots"); got != 1 {
		t.Fatalf("snapshot rows after apply = %d, want 1", got)
	}
	if got := retentionCountWhere(t, store, "webhook_push_log", "kind", "old-event"); got != 0 {
		t.Errorf("old push event count=%d, want 0", got)
	}
	if got := retentionCountWhere(t, store, "webhook_push_log", "kind", "fresh-event"); got != 1 {
		t.Errorf("fresh push event count=%d, want 1", got)
	}
	if got := retentionCountWhere(t, store, "webhook_push_log", "kind", "future-push-event"); got != 1 {
		t.Errorf("future-dated push event count=%d, want 1", got)
	}
	if got := retentionCountWhere(t, store, "scheduled_alerts", "alert_type", "old-alert"); got != 0 {
		t.Errorf("old alert event count=%d, want 0", got)
	}
	if got := retentionCountWhere(t, store, "scheduled_alerts", "alert_type", "fresh-alert"); got != 1 {
		t.Errorf("fresh alert event count=%d, want 1", got)
	}
	if got := retentionCountWhere(t, store, "scheduled_alerts", "alert_type", "future-alert"); got != 1 {
		t.Errorf("future scheduled alert count=%d, want 1", got)
	}
}

func TestRunDataRetention_RollsBackAllCategoriesOnFailure(t *testing.T) {
	store := setupTestDB(t)
	if store.dialect != DialectSQLite {
		t.Skip("SQLite trigger injects a deterministic mid-transaction failure")
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -60)
	seedRetentionDomainAndInteraction(t, store, now)
	queueID := enqueueRetentionWebhook(t, store, "rollback", models.WebhookStatusSent, old)
	createRetentionSnapshot(t, store, "rollback", old)
	if _, err := store.root.Exec(`CREATE TRIGGER block_snapshot_retention
		BEFORE DELETE ON pedagogical_snapshots
		BEGIN SELECT RAISE(ABORT, 'retention test failure'); END`); err != nil {
		t.Fatal(err)
	}

	_, err := store.RunDataRetention(context.Background(), RetentionPolicy{
		WebhookTerminalDays:     30,
		PedagogicalSnapshotDays: 30,
	}, now, true)
	if err == nil {
		t.Fatal("apply succeeded despite injected snapshot delete failure")
	}
	if got := retentionCountWhereID(t, store, "webhook_message_queue", queueID); got != 1 {
		t.Fatalf("webhook deletion was not rolled back: count=%d", got)
	}
	if got := retentionCountTable(t, store, "pedagogical_snapshots"); got != 1 {
		t.Fatalf("snapshot count after rollback=%d, want 1", got)
	}
}

func TestRunDataRetention_IdempotencyResponseExpiryPreservesReplayTombstone(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -60)
	fresh := now.AddDate(0, 0, -5)

	complete := func(key, hash, response string, at time.Time) {
		t.Helper()
		if _, execute, err := store.ClaimIdempotencyKey(ctx, "L1", "record_interaction", key, hash, at); err != nil || !execute {
			t.Fatalf("claim %s: execute=%v err=%v", key, execute, err)
		}
		if err := store.CompleteIdempotencyKey(ctx, "L1", "record_interaction", key, hash, response, at); err != nil {
			t.Fatalf("complete %s: %v", key, err)
		}
	}
	complete("old-completed", "old-hash", `{"learner_detail":"private"}`, old)
	complete("fresh-completed", "fresh-hash", `{"fresh":true}`, fresh)
	if _, execute, err := store.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "old-processing", "processing-hash", old); err != nil || !execute {
		t.Fatalf("claim processing row: execute=%v err=%v", execute, err)
	}

	policy := RetentionPolicy{IdempotencyResponseDays: 30}
	dry, err := store.RunDataRetention(ctx, policy, now, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	assertRetentionMetric(t, "idempotency response plaintext", dry.IdempotencyResponsePlaintext, 1, 0)
	assertIdempotencyResponseState(t, store, "old-completed", "old-hash", "completed", `{"learner_detail":"private"}`, false)

	applied, err := store.RunDataRetention(ctx, policy, now, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertRetentionMetric(t, "idempotency response plaintext", applied.IdempotencyResponsePlaintext, 1, 1)
	if got := retentionCountTable(t, store, "tool_call_idempotency"); got != 3 {
		t.Fatalf("idempotency rows after apply=%d, want 3", got)
	}
	assertIdempotencyResponseState(t, store, "old-completed", "old-hash", "completed", "", true)
	assertIdempotencyResponseState(t, store, "fresh-completed", "fresh-hash", "completed", `{"fresh":true}`, false)
	assertIdempotencyResponseState(t, store, "old-processing", "processing-hash", "processing", "", false)

	cached, execute, err := store.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "old-completed", "old-hash", now)
	if !errors.Is(err, storeport.ErrIdempotencyResponseExpired) || execute || cached != "" {
		t.Fatalf("expired response claim: cached=%q execute=%v err=%v", cached, execute, err)
	}
	// The preserved request hash still wins over the expiry marker, preventing
	// the same key from being repurposed for a different mutation as well.
	if _, execute, err := store.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "old-completed", "different-hash", now); !errors.Is(err, storeport.ErrIdempotencyKeyConflict) || execute {
		t.Fatalf("expired key with changed payload: execute=%v err=%v", execute, err)
	}
	if cached, execute, err := store.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "fresh-completed", "fresh-hash", now); err != nil || execute || cached != `{"fresh":true}` {
		t.Fatalf("fresh replay: cached=%q execute=%v err=%v", cached, execute, err)
	}
	if _, execute, err := store.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "old-processing", "processing-hash", now); !errors.Is(err, storeport.ErrIdempotencyInProgress) || execute {
		t.Fatalf("processing claim after retention: execute=%v err=%v", execute, err)
	}
}

func TestRunDataRetention_ClosesAbandonedWorkAndCompletedMarkers(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -60)
	seedRetentionDomainAndInteraction(t, store, now)

	create := func(id string, created time.Time) {
		t.Helper()
		if err := store.CreateAssessmentAttempt(ctx, &models.AssessmentAttempt{
			ID: id, LearnerID: "L1", DomainID: "D-retention", ConceptID: "c",
			ActivityID: "activity-" + id, ActivityVersion: 1,
			ActivityType: string(models.ActivityRecall), Observable: "answer",
			TaskText: "sensitive task", TaskContentHash: "task-hash",
			RubricJSON: `{"criteria":[{"id":"correct","weight":1}]}`, PassingScore: 0.5,
			CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	create("old-prepared", old)
	create("old-submitted", old)
	if err := store.SubmitAssessmentAttempt(ctx, "L1", "old-submitted", "sensitive response", "response-hash", old); err != nil {
		t.Fatal(err)
	}
	create("fresh-prepared", now)

	oldPending, err := store.EnqueueWebhookMessage(ctx, "L1", "old-pending", "body", old, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldProcessing, err := store.EnqueueWebhookMessage(ctx, "L1", "old-processing", "body", old, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextPendingWebhook(ctx, "L1", "old-processing", old, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Exec(rb(store,
		`UPDATE webhook_message_queue SET created_at = ? WHERE id IN (?, ?)`), old, oldPending, oldProcessing); err != nil {
		t.Fatal(err)
	}
	freshPending, err := store.EnqueueWebhookMessage(ctx, "L1", "fresh-pending", "body", now, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.MarkConsolidationCompleted(ctx, "L1", "monthly", "old", old); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkConsolidationCompleted(ctx, "L1", "monthly", "fresh", now); err != nil {
		t.Fatal(err)
	}
	policy := RetentionPolicy{
		AssessmentAbandonedDays:    30,
		WebhookLiveDays:            30,
		CompletedConsolidationDays: 30,
	}
	dry, err := store.RunDataRetention(ctx, policy, now, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionMetric(t, "abandoned assessments", dry.AssessmentAbandonedAttempts, 2, 0)
	assertRetentionMetric(t, "live webhooks", dry.WebhookLiveRowsTerminalized, 2, 0)
	assertRetentionMetric(t, "completed consolidations", dry.CompletedConsolidations, 1, 0)

	applied, err := store.RunDataRetention(ctx, policy, now, true)
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionMetric(t, "abandoned assessments", applied.AssessmentAbandonedAttempts, 2, 2)
	assertRetentionMetric(t, "live webhooks", applied.WebhookLiveRowsTerminalized, 2, 2)
	assertRetentionMetric(t, "completed consolidations", applied.CompletedConsolidations, 1, 1)
	for _, id := range []string{"old-prepared", "old-submitted"} {
		var status string
		var task, response sql.NullString
		if err := store.root.QueryRow(rb(store,
			`SELECT status, task_text, response_text FROM assessment_attempts WHERE id = ?`), id).Scan(&status, &task, &response); err != nil {
			t.Fatal(err)
		}
		if status != string(models.AssessmentAttemptCancelled) || task.Valid || response.Valid {
			t.Fatalf("abandoned assessment %s not safely redacted: status=%s task=%v response=%v", id, status, task, response)
		}
	}
	if attempt, err := store.GetAssessmentAttempt(ctx, "L1", "fresh-prepared"); err != nil || attempt.Status != models.AssessmentAttemptPrepared {
		t.Fatalf("fresh assessment changed: %+v err=%v", attempt, err)
	}
	for _, id := range []int64{oldPending, oldProcessing} {
		var status string
		if err := store.root.QueryRow(rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), id).Scan(&status); err != nil || status != models.WebhookStatusExpired {
			t.Fatalf("old webhook %d status=%s err=%v", id, status, err)
		}
	}
	if active, err := store.IsWebhookClaimActive(ctx, freshPending, "L1"); err != nil || active {
		t.Fatalf("fresh pending row unexpectedly claimed: active=%v err=%v", active, err)
	}
	if got := retentionCountWhere(t, store, "pending_consolidations", "period_key", "old"); got != 0 {
		t.Fatalf("old completed consolidation retained: %d", got)
	}
	if got := retentionCountWhere(t, store, "pending_consolidations", "period_key", "fresh"); got != 1 {
		t.Fatalf("fresh completed consolidation removed: %d", got)
	}
}

func TestRunDataRetention_ConcurrentApplyIsIdempotent(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -60)
	const rows = 20
	for i := 0; i < rows; i++ {
		enqueueRetentionWebhook(t, store, "concurrent", models.WebhookStatusSent, old)
	}

	const workers = 8
	start := make(chan struct{})
	reports := make(chan *RetentionReport, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			report, err := store.RunDataRetention(ctx, RetentionPolicy{WebhookTerminalDays: 30}, now, true)
			reports <- report
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(reports)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent retention apply: %v", err)
		}
	}
	var totalApplied int64
	for report := range reports {
		if report == nil {
			t.Fatal("nil report without error")
		}
		totalApplied += report.WebhookTerminalRows.Applied
	}
	if totalApplied != rows {
		t.Fatalf("total applied=%d, want %d", totalApplied, rows)
	}
	if got := retentionCountTable(t, store, "webhook_message_queue"); got != 0 {
		t.Fatalf("queue rows after concurrent apply=%d, want 0", got)
	}
}

func seedRetentionDomainAndInteraction(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO domains (id, learner_id, name, graph_json, created_at) VALUES (?, ?, ?, ?, ?)`),
		"D-retention", "L1", "Retention", `{"concepts":["c"]}`, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO interactions (learner_id, domain_id, concept, activity_type, success, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`),
		"L1", "D-retention", "c", "RECALL_EXERCISE", 1, now,
	); err != nil {
		t.Fatal(err)
	}
}

func enqueueRetentionWebhook(t *testing.T, store *Store, kind, status string, at time.Time) int64 {
	t.Helper()
	id, err := store.EnqueueWebhookMessage(context.Background(), "L1", kind, "content", at, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sentAt, claimedAt any
	switch status {
	case models.WebhookStatusSent:
		sentAt = at
	case models.WebhookStatusFailed, models.WebhookStatusProcessing:
		claimedAt = at
	}
	if _, err := store.root.Exec(
		rb(store, `UPDATE webhook_message_queue
		 SET status = ?, created_at = ?, claimed_at = ?, sent_at = ? WHERE id = ?`),
		status, at, claimedAt, sentAt, id,
	); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertRetentionAssessment(t *testing.T, store *Store, id string, status models.AssessmentAttemptStatus, at time.Time, hashed bool) {
	t.Helper()
	taskHash, responseHash := "", ""
	if hashed {
		taskHash, responseHash = "task-hash", "response-hash"
	}
	var submittedAt, evaluatedAt, cancelledAt any
	switch status {
	case models.AssessmentAttemptSubmitted:
		submittedAt = at
	case models.AssessmentAttemptEvaluated:
		submittedAt, evaluatedAt = at, at
	case models.AssessmentAttemptCancelled:
		cancelledAt = at
	}
	_, err := store.root.Exec(rb(store, `INSERT INTO assessment_attempts
		(id, learner_id, domain_id, concept_id, activity_id, activity_version,
		 activity_type, observable, task_text, task_content_hash, response_text,
		 response_content_hash, rubric_json, passing_score, status, created_at,
		 submitted_at, evaluated_at, cancelled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, "L1", "D-retention", "c", "activity-"+id, 1,
		"RECALL_EXERCISE", "observable", "task plaintext", taskHash,
		"response plaintext", responseHash, `{}`, 0.5, string(status), at,
		submittedAt, evaluatedAt, cancelledAt)
	if err != nil {
		t.Fatal(err)
	}
}

func createRetentionSnapshot(t *testing.T, store *Store, suffix string, at time.Time) {
	t.Helper()
	var interactionID int64
	if err := store.root.QueryRow(
		`SELECT id FROM interactions WHERE learner_id = 'L1' ORDER BY id LIMIT 1`,
	).Scan(&interactionID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePedagogicalSnapshot(context.Background(), &models.PedagogicalSnapshot{
		InteractionID:   interactionID,
		LearnerID:       "L1",
		DomainID:        "D-retention",
		Concept:         "c-" + suffix,
		ActivityType:    "RECALL_EXERCISE",
		BeforeJSON:      `{}`,
		ObservationJSON: `{}`,
		AfterJSON:       `{}`,
		DecisionJSON:    `{}`,
		CreatedAt:       at,
	}); err != nil {
		t.Fatal(err)
	}
}

func insertWebhookPushEvent(t *testing.T, store *Store, kind string, at time.Time) {
	insertWebhookPushEventAt(t, store, kind, at, at)
}

func insertWebhookPushEventAt(t *testing.T, store *Store, kind string, pushedAt, createdAt time.Time) {
	t.Helper()
	if _, err := store.root.Exec(rb(store, `INSERT INTO webhook_push_log
		(learner_id, queue_id, kind, pushed_at, created_at) VALUES (?, 0, ?, ?, ?)`),
		"L1", kind, pushedAt, createdAt); err != nil {
		t.Fatal(err)
	}
}

func insertScheduledAlertEvent(t *testing.T, store *Store, kind string, at time.Time) {
	insertScheduledAlertEventAt(t, store, kind, at, at)
}

func insertScheduledAlertEventAt(t *testing.T, store *Store, kind string, scheduledAt, createdAt time.Time) {
	t.Helper()
	if _, err := store.root.Exec(rb(store, `INSERT INTO scheduled_alerts
		(learner_id, alert_type, concept, scheduled_at, created_at) VALUES (?, ?, '', ?, ?)`),
		"L1", kind, scheduledAt, createdAt); err != nil {
		t.Fatal(err)
	}
}

func assertRetentionMetric(t *testing.T, name string, metric RetentionMetric, eligible, applied int64) {
	t.Helper()
	if metric.Eligible != eligible || metric.Applied != applied {
		t.Fatalf("%s metric=%+v, want eligible=%d applied=%d", name, metric, eligible, applied)
	}
}

func assertAssessmentPlaintext(t *testing.T, store *Store, id string, wantTask, wantResponse bool) {
	t.Helper()
	var task, response sql.NullString
	if err := store.root.QueryRow(
		rb(store, `SELECT task_text, response_text FROM assessment_attempts WHERE id = ?`), id,
	).Scan(&task, &response); err != nil {
		t.Fatal(err)
	}
	if (task.Valid && task.String != "") != wantTask || (response.Valid && response.String != "") != wantResponse {
		t.Fatalf("assessment %s plaintext task=%q response=%q, want task=%v response=%v",
			id, task.String, response.String, wantTask, wantResponse)
	}
}

func assertIdempotencyResponseState(t *testing.T, store *Store, key, wantHash, wantStatus, wantResponse string, wantExpired bool) {
	t.Helper()
	var requestHash, status, response string
	var expiredAt sql.NullTime
	if err := store.root.QueryRow(rb(store, `SELECT request_hash, status, response_text, response_expired_at
		FROM tool_call_idempotency
		WHERE learner_id = ? AND tool_name = ? AND idempotency_key = ?`),
		"L1", "record_interaction", key,
	).Scan(&requestHash, &status, &response, &expiredAt); err != nil {
		t.Fatal(err)
	}
	if requestHash != wantHash || status != wantStatus || response != wantResponse || expiredAt.Valid != wantExpired {
		t.Fatalf("idempotency %s state hash=%q status=%q response=%q expired=%v, want hash=%q status=%q response=%q expired=%v",
			key, requestHash, status, response, expiredAt.Valid, wantHash, wantStatus, wantResponse, wantExpired)
	}
}

func retentionCountTable(t *testing.T, store *Store, table string) int {
	t.Helper()
	var count int
	if err := store.root.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func retentionCountWhereID(t *testing.T, store *Store, table string, id int64) int {
	t.Helper()
	var count int
	if err := store.root.QueryRow(rb(store, `SELECT COUNT(*) FROM `+table+` WHERE id = ?`), id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func retentionCountWhere(t *testing.T, store *Store, table, column, value string) int {
	t.Helper()
	var count int
	if err := store.root.QueryRow(rb(store, `SELECT COUNT(*) FROM `+table+` WHERE `+column+` = ?`), value).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
