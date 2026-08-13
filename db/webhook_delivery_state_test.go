package db

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestEnqueueClaimedWebhookPayloadOncePerDayConcurrentSingleWinner(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	enableWebhookDeliveryForTest(t, store, "L1")
	const contenders = 12
	start := make(chan struct{})
	results := make(chan *models.WebhookQueueItem, contenders)
	errors := make(chan error, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			item, enqueued, err := store.EnqueueClaimedWebhookPayloadOncePerDay(
				ctx, "L1", "fallback", "FALLBACK", "", `{"embeds":[{"title":"fallback"}]}`,
				false, now, now.Add(time.Hour), 0,
			)
			if err != nil {
				errors <- err
				return
			}
			if enqueued {
				results <- item
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent fallback enqueue: %v", err)
		}
	}
	var winners []*models.WebhookQueueItem
	for item := range results {
		winners = append(winners, item)
	}
	if len(winners) != 1 || winners[0] == nil {
		t.Fatalf("fallback enqueue winners=%+v, want exactly one", winners)
	}
	item := winners[0]
	if item.Status != models.WebhookStatusProcessing || item.ContentFormat != models.WebhookContentFormatDiscordPayload || item.AttemptCount != 1 || item.ReservationID == nil {
		t.Fatalf("claimed fallback item=%+v", item)
	}
	for table, want := range map[string]int{"webhook_message_queue": 1, "scheduled_alerts": 1} {
		var count int
		if err := store.root.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s count=%d, want %d", table, count, want)
		}
	}
}

func enableWebhookDeliveryForTest(t *testing.T, store *Store, learnerID string) {
	t.Helper()
	availability := models.DefaultAvailability(learnerID)
	availability.NotificationConsent = true
	availability.NotificationFrequency = models.NotificationFrequencyAsScheduled
	if err := store.UpsertAvailability(context.Background(), availability); err != nil {
		t.Fatalf("enable webhook delivery: %v", err)
	}
}

func claimAndPrepareWebhookForTest(
	t *testing.T,
	store *Store,
	learnerID, kind, alertType string,
	now time.Time,
) (*models.WebhookQueueItem, int64) {
	t.Helper()
	item, err := store.ClaimNextPendingWebhook(context.Background(), learnerID, kind, now, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim webhook: item=%+v err=%v", item, err)
	}
	reservationID, prepared, err := store.PrepareWebhookDelivery(
		context.Background(), learnerID, alertType, "", false, []int64{item.ID}, now,
	)
	if err != nil || !prepared || reservationID == 0 {
		t.Fatalf("prepare webhook: reservation=%d prepared=%v err=%v", reservationID, prepared, err)
	}
	return item, reservationID
}

func TestWebhookDeliveryStateMachineCompletesAtomically(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	enableWebhookDeliveryForTest(t, store, "L1")
	id, err := store.EnqueueWebhookMessage(ctx, "L1", "delivery-success", "body", now, now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	item, reservationID := claimAndPrepareWebhookForTest(t, store, "L1", "delivery-success", "DELIVERY_SUCCESS", now)
	if item.ID != id || item.EventID == "" {
		t.Fatalf("claimed item=%+v, want id=%d and stable event id", item, id)
	}
	if err := store.BeginWebhookDelivery(ctx, "L1", []int64{id}, reservationID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteWebhookDelivery(ctx, "L1", []int64{id}, reservationID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	var status, deliveryState string
	var sent int
	if err := store.root.QueryRow(
		rb(store, `SELECT q.status, a.sent, a.delivery_state
			FROM webhook_message_queue q JOIN scheduled_alerts a ON a.id = q.reservation_id
			WHERE q.id = ?`), id,
	).Scan(&status, &sent, &deliveryState); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusSent || sent != 1 || deliveryState != models.NotificationDeliveryStateDelivered {
		t.Fatalf("completed state queue=%q sent=%d reservation=%q", status, sent, deliveryState)
	}
	transitions, err := store.GetWebhookDeliveryTransitions(ctx, "L1", item.EventID, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		models.WebhookStatusPending:     false,
		models.WebhookStatusProcessing:  false,
		models.WebhookStatusDispatching: false,
		models.WebhookStatusSent:        false,
	}
	for _, transition := range transitions {
		if _, ok := want[transition.ToStatus]; ok {
			want[transition.ToStatus] = true
		}
	}
	for status, seen := range want {
		if !seen {
			t.Errorf("transition to %q missing: %+v", status, transitions)
		}
	}
}

func TestWebhookDeliveryKnownFailureRetriesWithStableEventID(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	enableWebhookDeliveryForTest(t, store, "L1")
	id, err := store.EnqueueWebhookMessageWithMaxAttempts(
		ctx, "L1", "delivery-known-failure", "body", now, now.Add(time.Hour), 0, 3,
	)
	if err != nil {
		t.Fatal(err)
	}
	item, reservationID := claimAndPrepareWebhookForTest(t, store, "L1", "delivery-known-failure", "KNOWN_FAILURE", now)
	if err := store.BeginWebhookDelivery(ctx, "L1", []int64{id}, reservationID, now); err != nil {
		t.Fatal(err)
	}
	dead, err := store.RecordKnownWebhookDeliveryFailure(
		ctx, "L1", []int64{id}, reservationID, "http_503", now,
	)
	if err != nil || dead != 0 {
		t.Fatalf("known failure dead=%d err=%v", dead, err)
	}
	assertWebhookRetryState(t, store, id, models.WebhookStatusPending, 1, now.Add(time.Minute), "http_503", false)
	var reservations int
	if err := store.root.QueryRow(
		rb(store, `SELECT COUNT(*) FROM scheduled_alerts WHERE id = ?`), reservationID,
	).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("failed reservation count=%d err=%v, want 0", reservations, err)
	}

	retry, retryReservation := claimAndPrepareWebhookForTest(
		t, store, "L1", "delivery-known-failure", "KNOWN_FAILURE", now.Add(time.Minute),
	)
	if retry.EventID != item.EventID || retry.AttemptCount != 2 || retryReservation == reservationID {
		t.Fatalf("retry item=%+v reservation=%d, first event=%q reservation=%d", retry, retryReservation, item.EventID, reservationID)
	}
}

func TestWebhookDeliveryUnknownRequiresOperatorResolution(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	enableWebhookDeliveryForTest(t, store, "L1")
	id, err := store.EnqueueWebhookMessageWithMaxAttempts(
		ctx, "L1", "delivery-unknown", "body", now, now.Add(time.Hour), 0, 4,
	)
	if err != nil {
		t.Fatal(err)
	}
	item, reservationID := claimAndPrepareWebhookForTest(t, store, "L1", "delivery-unknown", "UNKNOWN", now)
	if err := store.BeginWebhookDelivery(ctx, "L1", []int64{id}, reservationID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWebhookDeliveryUnknown(
		ctx, "L1", []int64{id}, reservationID, "transport_outcome_unknown", now,
	); err != nil {
		t.Fatal(err)
	}
	unknown, err := store.GetWebhookDeliveryUnknown(ctx, "L1", 10)
	if err != nil || len(unknown) != 1 || unknown[0].ID != id {
		t.Fatalf("unknown deliveries=%+v err=%v", unknown, err)
	}
	if got, err := store.ClaimNextPendingWebhook(ctx, "L1", "delivery-unknown", now.Add(time.Hour), time.Hour); err != nil || got != nil {
		t.Fatalf("quarantined row became claimable: item=%+v err=%v", got, err)
	}
	if err := store.ResolveWebhookDeliveryUnknown(ctx, id, "L1", false, now); err != nil {
		t.Fatal(err)
	}
	assertWebhookRetryState(t, store, id, models.WebhookStatusPending, 1, now.Add(time.Minute), "operator_retry_authorized", false)

	retry, retryReservation := claimAndPrepareWebhookForTest(t, store, "L1", "delivery-unknown", "UNKNOWN", now.Add(time.Minute))
	if retry.EventID != item.EventID {
		t.Fatalf("event id changed across operator-authorized retry: %q -> %q", item.EventID, retry.EventID)
	}
	if err := store.BeginWebhookDelivery(ctx, "L1", []int64{id}, retryReservation, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWebhookDeliveryUnknown(ctx, "L1", []int64{id}, retryReservation, "transport_outcome_unknown", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveWebhookDeliveryUnknown(ctx, id, "L1", true, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.root.QueryRow(rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), id).Scan(&status); err != nil || status != models.WebhookStatusSent {
		t.Fatalf("operator-confirmed status=%q err=%v", status, err)
	}
}

func TestHasLiveWebhookMessageKindTracksOnlyRetryableOrUncertainWork(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	enableWebhookDeliveryForTest(t, store, "L1")
	id, err := store.EnqueueWebhookMessage(
		ctx, "L1", "live-kind", "body", now, now.Add(time.Hour), 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertLiveWebhookKind(t, store, "live-kind", now, true)

	_, reservationID := claimAndPrepareWebhookForTest(t, store, "L1", "live-kind", "LIVE_KIND", now)
	assertLiveWebhookKind(t, store, "live-kind", now, true)
	if err := store.BeginWebhookDelivery(ctx, "L1", []int64{id}, reservationID, now); err != nil {
		t.Fatal(err)
	}
	assertLiveWebhookKind(t, store, "live-kind", now, true)
	if err := store.MarkWebhookDeliveryUnknown(
		ctx, "L1", []int64{id}, reservationID, "transport_outcome_unknown", now,
	); err != nil {
		t.Fatal(err)
	}
	assertLiveWebhookKind(t, store, "live-kind", now, true)
	if err := store.ResolveWebhookDeliveryUnknown(ctx, id, "L1", true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertLiveWebhookKind(t, store, "live-kind", now.Add(time.Minute), false)

	if _, err := store.EnqueueWebhookMessage(
		ctx, "L1", "expired-kind", "body", now.Add(-2*time.Hour), now.Add(-time.Hour), 0,
	); err != nil {
		t.Fatal(err)
	}
	assertLiveWebhookKind(t, store, "expired-kind", now, false)
}

func assertLiveWebhookKind(t *testing.T, store *Store, kind string, now time.Time, want bool) {
	t.Helper()
	got, err := store.HasLiveWebhookMessageKind(context.Background(), "L1", kind, now)
	if err != nil {
		t.Fatalf("check live webhook kind %q: %v", kind, err)
	}
	if got != want {
		t.Fatalf("live webhook kind %q = %v, want %v", kind, got, want)
	}
}

func TestWebhookDeliveryReconciliationSeparatesPreAndPostHTTPBoundaries(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	claimedAt := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)
	now := claimedAt.Add(20 * time.Minute)
	enableWebhookDeliveryForTest(t, store, "L1")
	processingID, err := store.EnqueueWebhookMessage(ctx, "L1", "stale-processing", "body", claimedAt, now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if item, err := store.ClaimNextPendingWebhook(ctx, "L1", "stale-processing", claimedAt, time.Minute); err != nil || item == nil {
		t.Fatalf("claim processing row: item=%+v err=%v", item, err)
	}
	dispatchingID, err := store.EnqueueWebhookMessage(ctx, "L1", "stale-dispatching", "body", claimedAt, now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, reservationID := claimAndPrepareWebhookForTest(t, store, "L1", "stale-dispatching", "STALE", claimedAt)
	if err := store.BeginWebhookDelivery(ctx, "L1", []int64{dispatchingID}, reservationID, claimedAt); err != nil {
		t.Fatal(err)
	}

	reconciled, err := store.RequeueStaleWebhookClaims(ctx, now.Add(-15*time.Minute), now)
	if err != nil || reconciled != 2 {
		t.Fatalf("reconciled=%d err=%v, want 2", reconciled, err)
	}
	var processingStatus, dispatchingStatus, reservationState string
	if err := store.root.QueryRow(rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), processingID).Scan(&processingStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.root.QueryRow(rb(store, `SELECT status FROM webhook_message_queue WHERE id = ?`), dispatchingID).Scan(&dispatchingStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.root.QueryRow(rb(store, `SELECT delivery_state FROM scheduled_alerts WHERE id = ?`), reservationID).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	if processingStatus != models.WebhookStatusPending || dispatchingStatus != models.WebhookStatusDeliveryUnknown || reservationState != models.NotificationDeliveryStateDeliveryUnknown {
		t.Fatalf("reconciled states processing=%q dispatching=%q reservation=%q", processingStatus, dispatchingStatus, reservationState)
	}
}

func TestWebhookDeliveryCompletionRollbackLeavesDispatchingForQuarantine(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	enableWebhookDeliveryForTest(t, store, "L1")
	id, err := store.EnqueueWebhookMessage(ctx, "L1", "completion-rollback", "body", now, now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, reservationID := claimAndPrepareWebhookForTest(t, store, "L1", "completion-rollback", "ROLLBACK", now)
	if err := store.BeginWebhookDelivery(ctx, "L1", []int64{id}, reservationID, now); err != nil {
		t.Fatal(err)
	}
	installWebhookCompletionFailureTrigger(t, store)
	if err := store.CompleteWebhookDelivery(ctx, "L1", []int64{id}, reservationID, now.Add(time.Second)); err == nil {
		t.Fatal("completion unexpectedly succeeded through injected database failure")
	}
	var status, reservationState string
	var sent int
	if err := store.root.QueryRow(
		rb(store, `SELECT q.status, a.sent, a.delivery_state
			FROM webhook_message_queue q JOIN scheduled_alerts a ON a.id = q.reservation_id
			WHERE q.id = ?`), id,
	).Scan(&status, &sent, &reservationState); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusDispatching || sent != 0 || reservationState != models.NotificationDeliveryStateReserved {
		t.Fatalf("rollback state queue=%q sent=%d reservation=%q", status, sent, reservationState)
	}
}

func installWebhookCompletionFailureTrigger(t *testing.T, store *Store) {
	t.Helper()
	if store.dialect == DialectPostgres {
		if _, err := store.root.Exec(`
			CREATE FUNCTION fail_webhook_completion() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'injected webhook completion failure';
			END
			$$`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.root.Exec(`
			CREATE TRIGGER fail_webhook_completion
			BEFORE UPDATE OF sent ON scheduled_alerts
			FOR EACH ROW WHEN (NEW.sent = 1)
			EXECUTE FUNCTION fail_webhook_completion()`); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := store.root.Exec(`
		CREATE TRIGGER fail_webhook_completion
		BEFORE UPDATE OF sent ON scheduled_alerts
		WHEN NEW.sent = 1
		BEGIN
			SELECT RAISE(ABORT, 'injected webhook completion failure');
		END`); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
}
