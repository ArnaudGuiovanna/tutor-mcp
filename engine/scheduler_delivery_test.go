package engine

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type webhookRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn webhookRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func webhookResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func enqueueSchedulerDelivery(t *testing.T, store storeport.Store, learnerID, kind string) int64 {
	t.Helper()
	now := time.Now().UTC()
	id, err := store.EnqueueWebhookMessage(
		context.Background(), learnerID, kind, "durable body", now, now.Add(time.Hour), 1,
	)
	if err != nil {
		t.Fatalf("enqueue scheduler delivery: %v", err)
	}
	return id
}

func TestDoOnceClassifiedRequiresExact2xx(t *testing.T) {
	allowAnyURL(t)
	for _, tc := range []struct {
		status int
		want   webhookHTTPOutcome
	}{
		{http.StatusEarlyHints, webhookHTTPRejected},
		{http.StatusOK, webhookHTTPDelivered},
		{http.StatusNoContent, webhookHTTPDelivered},
		{299, webhookHTTPDelivered},
		{http.StatusMultipleChoices, webhookHTTPRejected},
		{http.StatusServiceUnavailable, webhookHTTPRejected},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			scheduler := newTestScheduler()
			scheduler.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return webhookResponse(tc.status), nil
			})
			got := scheduler.doOnceClassified("https://example.invalid/hook", []byte(`{}`))
			if got.outcome != tc.want || got.statusCode != tc.status {
				t.Fatalf("status %d classified as outcome=%d code=%d, want %d/%d", tc.status, got.outcome, got.statusCode, tc.want, tc.status)
			}
		})
	}
}

func TestDispatchQueuedUsesDurableSuccessLifecycle(t *testing.T) {
	allowAnyURL(t)
	rawDB, store, learnerID := rawTestSetup(t, "https://example.invalid/hook")
	id := enqueueSchedulerDelivery(t, store, learnerID, models.WebhookKindDailyMotivation)
	var requests atomic.Int32
	scheduler := schedulerForTest(store)
	scheduler.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return webhookResponse(http.StatusNoContent), nil
	})

	scheduler.dispatchQueued(models.WebhookKindDailyMotivation, "DAILY_MOTIVATION", nil)
	if requests.Load() != 1 {
		t.Fatalf("requests=%d, want 1", requests.Load())
	}
	var status, eventID, deliveryState string
	var sent int
	if err := rawDB.QueryRow(`SELECT q.status, q.event_id, a.sent, a.delivery_state
		FROM webhook_message_queue q JOIN scheduled_alerts a ON a.id = q.reservation_id
		WHERE q.id = ?`, id).Scan(&status, &eventID, &sent, &deliveryState); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusSent || eventID == "" || sent != 1 || deliveryState != models.NotificationDeliveryStateDelivered {
		t.Fatalf("durable success queue=%q event=%q sent=%d reservation=%q", status, eventID, sent, deliveryState)
	}
}

func TestSendOLMUsesDurableQueueLifecycle(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "false")
	allowAnyURL(t)
	store, rawDB := newOLMTestStore(t)
	if _, err := rawDB.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, webhook_url, last_active, created_at, email_verified_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		"L1", "l1@t.com", "h", "obj", "https://example.invalid/hook", time.Now().UTC(), time.Now().UTC(), time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	optInSchedulerNotifications(t, store, "L1")
	domainID := seedDomain(t, rawDB, "L1", "math", []string{"a", "b"}, map[string][]string{"b": {"a"}}, false)
	seedConceptState(t, store, "L1", "a", 0.90, "review")
	now := time.Now().UTC()
	id, err := store.EnqueueWebhookMessage(
		context.Background(), "L1", "olm:"+domainID,
		`{"domain_id":"`+domainID+`","why_now":"queued evidence","next_action":"review"}`,
		now, now.Add(time.Hour), 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := schedulerForTest(store)
	scheduler.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return webhookResponse(http.StatusNoContent), nil
	})

	scheduler.sendOLM()
	var status string
	var dispatchTransitions int
	if err := rawDB.QueryRow(`SELECT status FROM webhook_message_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM webhook_delivery_transitions
		WHERE queue_id = ? AND to_status = 'dispatching'`, id).Scan(&dispatchTransitions); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusSent || dispatchTransitions != 1 {
		t.Fatalf("OLM queue status=%q dispatch transitions=%d, want sent/1", status, dispatchTransitions)
	}
}

func TestDispatchQueuedPersistsKnownHTTPFailure(t *testing.T) {
	allowAnyURL(t)
	rawDB, store, learnerID := rawTestSetup(t, "https://example.invalid/hook")
	id := enqueueSchedulerDelivery(t, store, learnerID, models.WebhookKindDailyMotivation)
	scheduler := schedulerForTest(store)
	scheduler.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return webhookResponse(http.StatusServiceUnavailable), nil
	})

	scheduler.dispatchQueued(models.WebhookKindDailyMotivation, "DAILY_MOTIVATION", nil)
	var status, eventID, lastError string
	var attempts int
	var reservationID any
	if err := rawDB.QueryRow(`SELECT status, event_id, attempt_count, last_error, reservation_id
		FROM webhook_message_queue WHERE id = ?`, id).Scan(&status, &eventID, &attempts, &lastError, &reservationID); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusPending || eventID == "" || attempts != 1 || lastError != "http_503" || reservationID != nil {
		t.Fatalf("known failure status=%q event=%q attempts=%d error=%q reservation=%v", status, eventID, attempts, lastError, reservationID)
	}
}

func TestDispatchQueuedQuarantinesAmbiguousTransportFailure(t *testing.T) {
	allowAnyURL(t)
	rawDB, store, learnerID := rawTestSetup(t, "https://example.invalid/hook")
	id := enqueueSchedulerDelivery(t, store, learnerID, models.WebhookKindDailyMotivation)
	scheduler := schedulerForTest(store)
	scheduler.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("injected transport timeout with secret material")
	})

	scheduler.dispatchQueued(models.WebhookKindDailyMotivation, "DAILY_MOTIVATION", nil)
	var status, lastError, deliveryState string
	var nextAttempt any
	if err := rawDB.QueryRow(`SELECT q.status, q.last_error, q.next_attempt_at, a.delivery_state
		FROM webhook_message_queue q JOIN scheduled_alerts a ON a.id = q.reservation_id
		WHERE q.id = ?`, id).Scan(&status, &lastError, &nextAttempt, &deliveryState); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusDeliveryUnknown || lastError != "transport_outcome_unknown" || nextAttempt != nil || deliveryState != models.NotificationDeliveryStateDeliveryUnknown {
		t.Fatalf("ambiguous failure status=%q error=%q next=%v reservation=%q", status, lastError, nextAttempt, deliveryState)
	}
}

type completeWebhookFailStore struct {
	storeport.Store
}

func (completeWebhookFailStore) CompleteWebhookDelivery(context.Context, string, []int64, int64, time.Time) error {
	return errors.New("injected completion failure")
}

type credentialBoundaryStore struct {
	storeport.Store
	raw     *sql.DB
	queueID int64
	lookups atomic.Int32
}

func (s *credentialBoundaryStore) GetWebhookDispatchURL(ctx context.Context, learnerID string) (string, error) {
	var status string
	if err := s.raw.QueryRowContext(ctx, `SELECT status FROM webhook_message_queue WHERE id = ?`, s.queueID).Scan(&status); err != nil {
		return "", err
	}
	if status != models.WebhookStatusProcessing {
		return "", errors.New("credential resolved before the durable queue claim")
	}
	s.lookups.Add(1)
	return s.Store.GetWebhookDispatchURL(ctx, learnerID)
}

func TestDispatchCredentialIsResolvedOnlyAtFinalBoundary(t *testing.T) {
	allowAnyURL(t)
	rawDB, store, learnerID := rawTestSetup(t, "https://example.invalid/final-boundary")
	id := enqueueSchedulerDelivery(t, store, learnerID, models.WebhookKindDailyMotivation)
	boundary := &credentialBoundaryStore{Store: store, raw: rawDB, queueID: id}
	scheduler := &Scheduler{
		store:  boundary,
		logger: quietLogger(),
		client: &http.Client{Transport: webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return webhookResponse(http.StatusNoContent), nil
		})},
	}

	result := scheduler.dispatchQueued(models.WebhookKindDailyMotivation, "DAILY_MOTIVATION", nil)
	if result.failed() {
		t.Fatalf("final-boundary dispatch failed: %+v", result)
	}
	if got := boundary.lookups.Load(); got != 1 {
		t.Fatalf("credential lookups=%d, want exactly 1 after claim preparation", got)
	}
}

func TestWebhookTransportFailureRedactsCredentialFromErrorAndLogs(t *testing.T) {
	allowAnyURL(t)
	const secretURL = "https://discord.com/api/webhooks/123/do-not-log-this-token"
	capture := &captureHandler{}
	scheduler := &Scheduler{
		logger: slog.New(capture),
		client: &http.Client{Transport: webhookRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("transport failed for " + request.URL.String())
		})},
	}

	result := scheduler.doOnceClassified(secretURL, []byte(`{}`))
	if result.outcome != webhookHTTPUnknown || result.err == nil {
		t.Fatalf("transport result=%+v, want sanitized unknown outcome", result)
	}
	for _, forbidden := range []string{secretURL, "do-not-log-this-token"} {
		if strings.Contains(result.err.Error(), forbidden) || capture.contains(forbidden) {
			t.Fatalf("transport failure disclosed credential fragment %q", forbidden)
		}
	}
}

func TestDispatchQueuedLeaves2xxCompletionFailureForStaleQuarantine(t *testing.T) {
	allowAnyURL(t)
	rawDB, store, learnerID := rawTestSetup(t, "https://example.invalid/hook")
	id := enqueueSchedulerDelivery(t, store, learnerID, models.WebhookKindDailyMotivation)
	scheduler := &Scheduler{
		store:  completeWebhookFailStore{Store: store},
		logger: quietLogger(),
		client: &http.Client{Transport: webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return webhookResponse(http.StatusNoContent), nil
		})},
	}

	scheduler.dispatchQueued(models.WebhookKindDailyMotivation, "DAILY_MOTIVATION", nil)
	var status string
	if err := rawDB.QueryRow(`SELECT status FROM webhook_message_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusDispatching {
		t.Fatalf("post-2xx completion failure status=%q, want dispatching", status)
	}
	future := time.Now().UTC().Add(time.Minute)
	if count, err := store.RequeueStaleWebhookClaims(context.Background(), future, future.Add(time.Second)); err != nil || count != 1 {
		t.Fatalf("stale reconciliation count=%d err=%v", count, err)
	}
	if err := rawDB.QueryRow(`SELECT status FROM webhook_message_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusDeliveryUnknown {
		t.Fatalf("post-2xx stale status=%q, want delivery_unknown", status)
	}
}

func TestDispatchQueuedRejectsUnsafeURLBeforeHTTPBoundary(t *testing.T) {
	rawDB, store, learnerID := rawTestSetup(t, "http://unsafe.invalid/hook")
	id := enqueueSchedulerDelivery(t, store, learnerID, models.WebhookKindDailyMotivation)
	var requests atomic.Int32
	scheduler := schedulerForTest(store)
	scheduler.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return webhookResponse(http.StatusNoContent), nil
	})

	scheduler.dispatchQueued(models.WebhookKindDailyMotivation, "DAILY_MOTIVATION", nil)
	if requests.Load() != 0 {
		t.Fatalf("unsafe URL crossed HTTP boundary with %d requests", requests.Load())
	}
	var status string
	var attempts int
	if err := rawDB.QueryRow(`SELECT status, attempt_count FROM webhook_message_queue WHERE id = ?`, id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != models.WebhookStatusPending || attempts != 0 {
		t.Fatalf("pre-boundary rejection status=%q attempts=%d, want pending/0", status, attempts)
	}
}
