package db

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type asyncAttemptFixture struct {
	s, runtime *Store
	scope      models.TenantScope
	job        *models.AsyncJob
	delivery   *models.IntegrationDelivery
	payload    models.TenantWebhookJob
	now        time.Time
}

func newAsyncAttemptFixture(t *testing.T) asyncAttemptFixture {
	t.Helper()
	s := setupTestDB(t)
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "key:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "key"))
	if err := ConfigureTenantIntegrationAllowedHosts(s, []string{"hooks.customer.test"}); err != nil {
		t.Fatal(err)
	}
	owner := ownerPrincipal(t, s)
	integration, _, err := s.CreateTenantIntegration(t.Context(), owner, "webhook", "https://hooks.customer.test/events", []string{"formation.version.published"})
	if err != nil {
		t.Fatal(err)
	}
	payload := models.TenantWebhookJob{IntegrationID: integration.ID, EventID: "attempt-event", EventType: "formation.version.published", Payload: json.RawMessage(`{"ok":true}`)}
	encoded, _ := json.Marshal(payload)
	now := time.Now().UTC()
	if err := s.EnqueueAsyncJob(t.Context(), owner.TenantScope(), models.AsyncJob{ID: "attempt-job", TenantID: owner.TenantID, Kind: "tenant_webhook_delivery", IdempotencyKey: "attempt-key", PayloadJSON: string(encoded), MaxAttempts: 8, CreatedAt: now, AvailableAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimAsyncJob(t.Context(), owner.TenantScope(), "attempt-owner-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := s.BeginIntegrationDelivery(t.Context(), owner.TenantScope(), payload, job.AttemptCount, now)
	if err != nil {
		t.Fatal(err)
	}
	runtime := s
	if s.dialect == DialectPostgres {
		runtime = sessionIndexRuntimeStore(t, s)
		var role string
		if err := runtime.root.QueryRow(`SELECT current_user`).Scan(&role); err != nil {
			t.Fatal(err)
		}
		if _, err := s.root.Exec(`GRANT SELECT,UPDATE ON async_jobs,integration_deliveries TO ` + pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	return asyncAttemptFixture{s: s, runtime: runtime, scope: owner.TenantScope(), job: job, delivery: delivery, payload: payload, now: now}
}

func (f asyncAttemptFixture) finish(t *testing.T, outcome models.AsyncJobOutcome, delivery *models.AsyncJobDeliveryResult) (models.AsyncJobOutcome, error) {
	t.Helper()
	return f.runtime.FinalizeAsyncJobAttempt(t.Context(), f.scope, f.job.ID, f.job.LeaseOwner, f.job.AttemptCount, models.AsyncJobAttemptResult{Outcome: outcome, AvailableAt: f.now.Add(time.Minute)}, delivery, f.now)
}

func (f asyncAttemptFixture) assert(t *testing.T, jobStatus, deliveryStatus string, attempt int) {
	t.Helper()
	var job, delivery string
	var count int
	if err := f.s.queryRow(t.Context(), `SELECT j.status,d.status,j.attempt_count FROM async_jobs j JOIN integration_deliveries d ON d.tenant_id=j.tenant_id
	 WHERE j.id=? AND d.id=?`, f.job.ID, f.delivery.ID).Scan(&job, &delivery, &count); err != nil {
		t.Fatal(err)
	}
	if job != jobStatus || delivery != deliveryStatus || count != attempt {
		t.Fatalf("job/delivery/attempt=%s/%s/%d want %s/%s/%d", job, delivery, count, jobStatus, deliveryStatus, attempt)
	}
}

func TestFinalizeAsyncJobAttemptCommitsTogetherAndPreservesAttempts(t *testing.T) {
	for _, outcome := range []models.AsyncJobOutcome{models.AsyncJobCompleted, models.AsyncJobRetry, models.AsyncJobDeadLetter} {
		t.Run(string(outcome), func(t *testing.T) {
			f := newAsyncAttemptFixture(t)
			code := 204
			got, err := f.finish(t, outcome, &models.AsyncJobDeliveryResult{DeliveryID: f.delivery.ID, ResponseCode: &code, ResponseHash: strings.Repeat("a", 64)})
			if err != nil || got != outcome {
				t.Fatalf("outcome=%s err=%v", got, err)
			}
			job, delivery := string(outcome), "delivered"
			if outcome == models.AsyncJobRetry {
				job, delivery = "pending", "failed"
			} else if outcome == models.AsyncJobDeadLetter {
				delivery = "dead_letter"
			}
			f.assert(t, job, delivery, 1)
		})
	}
}

func TestFinalizeAsyncJobAttemptRollsBackJobIfDeliveryWriteFails(t *testing.T) {
	f := newAsyncAttemptFixture(t)
	var err error
	if f.s.dialect == DialectSQLite {
		_, err = f.s.exec(t.Context(), `CREATE TRIGGER reject_delivery_finalization BEFORE UPDATE ON integration_deliveries BEGIN SELECT RAISE(ABORT,'injected delivery write failure'); END`)
	} else {
		_, err = f.s.exec(t.Context(), `CREATE FUNCTION reject_delivery_finalization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected delivery write failure'; END $$;
		 CREATE TRIGGER reject_delivery_finalization BEFORE UPDATE ON integration_deliveries FOR EACH ROW EXECUTE FUNCTION reject_delivery_finalization()`)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.finish(t, models.AsyncJobCompleted, &models.AsyncJobDeliveryResult{DeliveryID: f.delivery.ID}); err == nil {
		t.Fatal("second write failure ignored")
	}
	f.assert(t, "processing", "pending", 1)
}

func TestFinalizeAsyncJobAttemptFencesLeasePayloadAndNestedTransactions(t *testing.T) {
	f := newAsyncAttemptFixture(t)
	for _, tc := range []struct {
		name, owner string
		attempt     int
		now         time.Time
		scope       models.TenantScope
	}{
		{"wrong_owner", "other", 1, f.now, f.scope},
		{"wrong_attempt", f.job.LeaseOwner, 2, f.now, f.scope},
		{"expired", f.job.LeaseOwner, 1, f.now.Add(time.Minute), f.scope},
		{"wrong_tenant", f.job.LeaseOwner, 1, f.now, models.TenantScope{TenantID: "other", UserID: "worker_test", MembershipID: "worker_process"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.runtime.FinalizeAsyncJobAttempt(t.Context(), tc.scope, f.job.ID, tc.owner, tc.attempt, models.AsyncJobAttemptResult{Outcome: models.AsyncJobCompleted}, &models.AsyncJobDeliveryResult{DeliveryID: f.delivery.ID}, tc.now)
			if !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("stale attempt accepted: %v", err)
			}
			f.assert(t, "processing", "pending", 1)
		})
	}
	for _, future := range []bool{false, true} {
		payload := f.payload
		attempt := 1
		if future {
			attempt = 2
		} else {
			payload.EventID = "unrelated-event"
		}
		other, err := f.s.BeginIntegrationDelivery(t.Context(), f.scope, payload, attempt, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.finish(t, models.AsyncJobCompleted, &models.AsyncJobDeliveryResult{DeliveryID: other.ID}); err == nil {
			t.Fatal("unrelated or future delivery accepted")
		}
		f.assert(t, "processing", "pending", 1)
	}
	if err := f.runtime.WithTenantTx(t.Context(), f.scope, func(ctx context.Context, scoped storeport.Store) error {
		for _, target := range []*Store{f.runtime, scoped.(*Store)} {
			if _, err := target.FinalizeAsyncJobAttempt(ctx, f.scope, f.job.ID, f.job.LeaseOwner, 1, models.AsyncJobAttemptResult{Outcome: models.AsyncJobCompleted}, &models.AsyncJobDeliveryResult{DeliveryID: f.delivery.ID}, f.now); err == nil {
				t.Fatal("finalizer inherited caller transaction")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.assert(t, "processing", "pending", 1)
}

func TestFinalizeAsyncJobAttemptPriorAcceptedDeliveryIsAckOnly(t *testing.T) {
	f := newAsyncAttemptFixture(t)
	code := 204
	if err := f.s.FinishIntegrationDelivery(t.Context(), f.scope, f.delivery.ID, "delivered", &code, "original-response", ""); err != nil {
		t.Fatal(err)
	}
	job, err := f.s.ClaimAsyncJob(t.Context(), f.scope, "attempt-owner-b", f.now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.job = job
	f.now = f.now.Add(2 * time.Minute)
	for _, outcome := range []models.AsyncJobOutcome{models.AsyncJobRetry, models.AsyncJobDeadLetter} {
		if _, err := f.finish(t, outcome, &models.AsyncJobDeliveryResult{DeliveryID: f.delivery.ID}); err == nil {
			t.Fatal("accepted delivery was writable")
		}
	}
	if _, err := f.finish(t, models.AsyncJobCompleted, &models.AsyncJobDeliveryResult{DeliveryID: f.delivery.ID}); err != nil {
		t.Fatal(err)
	}
	f.assert(t, "completed", "delivered", 2)
	var hash string
	var gotCode int
	if err := f.s.queryRow(t.Context(), `SELECT response_code,response_hash FROM integration_deliveries WHERE id=?`, f.delivery.ID).Scan(&gotCode, &hash); err != nil || gotCode != 204 || hash != "original-response" {
		t.Fatalf("accepted proof rewritten: %d %s %v", gotCode, hash, err)
	}
}

func TestFinalizeAsyncJobAttemptDSARContinueRefundAndFencing(t *testing.T) {
	s := setupTestDB(t)
	owner := ownerPrincipal(t, s)
	ensureErasureReplacementOwner(t, s, owner)
	request, err := s.RequestTenantDSAR(t.Context(), owner, "L1", "erase", "verified request")
	if err != nil {
		t.Fatal(err)
	}
	scope := owner.TenantScope()
	now := time.Now().UTC()
	var old *models.AsyncJob
	for i := range 40 {
		job, err := s.ClaimAsyncJob(t.Context(), scope, "continuation-owner-"+time.Duration(i).String(), now, time.Minute)
		if err != nil || job.AttemptCount != 1 {
			t.Fatalf("healthy progress exhausted attempts: %+v %v", job, err)
		}
		if old != nil {
			if _, err := s.FinalizeAsyncJobAttempt(t.Context(), scope, old.ID, old.LeaseOwner, old.AttemptCount, models.AsyncJobAttemptResult{Outcome: models.AsyncJobCompleted}, nil, now); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("ABA after refund: %v", err)
			}
		}
		outcome, err := s.FinalizeAsyncJobAttempt(t.Context(), scope, job.ID, job.LeaseOwner, job.AttemptCount, models.AsyncJobAttemptResult{Outcome: models.AsyncJobContinue}, nil, now)
		if err != nil || outcome != models.AsyncJobContinue {
			t.Fatalf("continue=%s %v", outcome, err)
		}
		old = job
	}
	assertDSARRecoveryState(t, s, request.ID, "pending", "pending", 0)
}

func TestPrepareTenantWebhookPermanentAndStorageFailureRemainDistinct(t *testing.T) {
	f := newAsyncAttemptFixture(t)
	if _, err := f.s.PrepareTenantWebhook(t.Context(), f.scope, "missing", f.payload.EventID, f.payload.EventType, f.payload.Payload, f.now); !errors.Is(err, storeport.ErrWebhookPermanent) {
		t.Fatalf("missing integration not terminal: %v", err)
	}
	if _, err := f.s.exec(t.Context(), `ALTER TABLE tenant_integrations RENAME TO unavailable_integrations`); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.PrepareTenantWebhook(t.Context(), f.scope, f.payload.IntegrationID, f.payload.EventID, f.payload.EventType, f.payload.Payload, f.now)
	if err == nil || errors.Is(err, storeport.ErrWebhookPermanent) || errors.Unwrap(err) == nil {
		t.Fatalf("SQL outage collapsed into permanent configuration error: %v", err)
	}
}
