package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

type acceptedDeadlineBody struct {
	ctx           context.Context
	reads, closed atomic.Int32
}

func (b *acceptedDeadlineBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *acceptedDeadlineBody) Close() error { b.closed.Add(1); return nil }

type acceptedDeadlineFinalizer struct {
	*db.Store
	httpCtx context.Context
}

func (s *acceptedDeadlineFinalizer) FinalizeAsyncJobAttempt(ctx context.Context, scope models.TenantScope, id, owner string, attempt int, result models.AsyncJobAttemptResult, delivery *models.AsyncJobDeliveryResult, now time.Time) (models.AsyncJobOutcome, error) {
	select {
	case <-s.httpCtx.Done():
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if ctx.Err() != nil {
		return "", errors.New("HTTP deadline leaked into attempt finalization")
	}
	return s.Store.FinalizeAsyncJobAttempt(ctx, scope, id, owner, attempt, result, delivery, time.Now().UTC())
}

func TestSaaSAcceptedHTTPDeadlineKeepsFinalizationContext(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	scope := seedSaaSConsumerTenant(t, raw, store, "accepted_deadline", 1)
	job, err := store.ClaimAsyncJob(t.Context(), scope, "http-deadline-owner", time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &acceptedDeadlineFinalizer{Store: store}
	s := NewScheduler(store, quietLogger())
	var body *acceptedDeadlineBody
	var sends atomic.Int32
	s.integrationClient = &http.Client{Timeout: 20 * time.Millisecond, Transport: webhookRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		backend.httpCtx = req.Context()
		body = &acceptedDeadlineBody{ctx: req.Context()}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	outcome, err := s.consumeSaaSJob(ctx, backend, scope, job)
	if err != nil || outcome != models.AsyncJobCompleted {
		t.Fatalf("accepted status lost on HTTP deadline: %s %v", outcome, err)
	}
	if !errors.Is(backend.httpCtx.Err(), context.DeadlineExceeded) || body.reads.Load() != 0 || body.closed.Load() != 1 {
		t.Fatalf("body/deadline contract: %v read=%d close=%d", backend.httpCtx.Err(), body.reads.Load(), body.closed.Load())
	}
	var jobStatus, deliveryStatus string
	if err := raw.QueryRow(`SELECT j.status,d.status FROM async_jobs j JOIN integration_deliveries d ON d.tenant_id=j.tenant_id WHERE j.id=?`, job.ID).Scan(&jobStatus, &deliveryStatus); err != nil || jobStatus != "completed" || deliveryStatus != "delivered" {
		t.Fatalf("durable state=%s/%s %v", jobStatus, deliveryStatus, err)
	}
	if _, err := store.ClaimAsyncJob(t.Context(), scope, "retry-after-acceptance", time.Now().UTC().Add(time.Hour), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("accepted delivery became runnable: %v", err)
	}
	if sends.Load() != 1 {
		t.Fatal("accepted event resent")
	}
}

type smallDSARBatchStore struct {
	*db.Store
	batches, continuations atomic.Int32
}

func (s *smallDSARBatchStore) ProcessTenantDSARErasureBatch(ctx context.Context, scope models.TenantScope, worker models.WorkerPrincipal, id string, _ int, now time.Time) (bool, int64, error) {
	s.batches.Add(1)
	return s.Store.ProcessTenantDSARErasureBatch(ctx, scope, worker, id, 1, now)
}
func (s *smallDSARBatchStore) FinalizeAsyncJobAttempt(ctx context.Context, scope models.TenantScope, id, owner string, attempt int, result models.AsyncJobAttemptResult, delivery *models.AsyncJobDeliveryResult, now time.Time) (models.AsyncJobOutcome, error) {
	out, err := s.Store.FinalizeAsyncJobAttempt(ctx, scope, id, owner, attempt, result, delivery, now)
	if err == nil && out == models.AsyncJobContinue {
		s.continuations.Add(1)
	}
	return out, err
}

func TestSaaSDSARHealthyContinuationExceedsTechnicalRetryBudget(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	if err := store.EnsureRecoveryEnrollment(t.Context(), "L1"); err != nil {
		t.Fatal(err)
	}
	p, err := store.GetPrincipalForLearner(t.Context(), "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMembershipAuthorization(t.Context(), p.TenantScope(), models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordMembershipMFAVerification(t.Context(), p.TenantScope(), time.Now()); err != nil {
		t.Fatal(err)
	}
	p, err = store.GetPrincipalForLearner(t.Context(), "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	// Force >32 healthy quanta using real rows and real one-row deletion
	// batches; only the batch size is narrowed by this adapter.
	for i := range 300 {
		if _, err := raw.Exec(`INSERT INTO affect_states(learner_id,session_id,created_at) VALUES ('L1',?,?)`, fmt.Sprint("erase-batch-", i), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	request, err := store.RequestTenantDSAR(t.Context(), p, "L1", "erase", "verified request")
	if err != nil {
		t.Fatal(err)
	}
	backend := &smallDSARBatchStore{Store: store}
	s := NewTenantDistributedScheduler(backend, quietLogger(), "consumer-test")
	done := startSaaSTestCallback(t, s)
	select {
	case result := <-done:
		if result.failed() {
			t.Fatal(result)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("healthy DSAR did not drain")
	}
	var jobStatus, requestStatus string
	var attempts int
	if err := raw.QueryRow(`SELECT j.status,r.status,j.attempt_count FROM async_jobs j JOIN tenant_dsar_requests r ON r.tenant_id=j.tenant_id AND r.id=j.idempotency_key WHERE r.id=?`, request.ID).Scan(&jobStatus, &requestStatus, &attempts); err != nil {
		t.Fatal(err)
	}
	if backend.continuations.Load() <= 32 || backend.batches.Load() <= 256 || jobStatus != "completed" || requestStatus != "completed" || attempts != 1 {
		t.Fatalf("healthy work consumed technical retries: continue=%d batches=%d states=%s/%s attempts=%d", backend.continuations.Load(), backend.batches.Load(), jobStatus, requestStatus, attempts)
	}
}

func TestSaaSPermanentPayloadAndExhaustedRetryUseDeadLetter(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	for _, kind := range []string{"invalid_payload", "unsupported_kind", "revoked", "retry_exhausted", "transport"} {
		t.Run(kind, func(t *testing.T) {
			scope := seedSaaSConsumerTenant(t, raw, store, "terminal_"+kind, 1)
			switch kind {
			case "invalid_payload":
				if _, err := raw.Exec(`UPDATE async_jobs SET payload_json='{}' WHERE tenant_id=?`, scope.TenantID); err != nil {
					t.Fatal(err)
				}
			case "unsupported_kind":
				if _, err := raw.Exec(`UPDATE async_jobs SET kind='unsupported' WHERE tenant_id=?`, scope.TenantID); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if _, err := raw.Exec(`UPDATE tenant_integrations SET status='revoked' WHERE tenant_id=?`, scope.TenantID); err != nil {
					t.Fatal(err)
				}
			case "retry_exhausted":
				if _, err := raw.Exec(`UPDATE async_jobs SET max_attempts=1 WHERE tenant_id=?`, scope.TenantID); err != nil {
					t.Fatal(err)
				}
			}
			s := NewTenantDistributedScheduler(store, quietLogger(), "consumer-test")
			s.currentTenantScope = &scope
			var sends atomic.Int32
			s.integrationClient = &http.Client{Transport: webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
				sends.Add(1)
				if kind == "transport" {
					return nil, errors.New("ambiguous transport")
				}
				return webhookResponse(503), nil
			})}
			if result := s.consumeSaaSJobs(); result.failed() {
				t.Fatal(result)
			}
			if result := s.consumeSaaSJobs(); result.failed() {
				t.Fatal(result)
			}
			var status string
			var attempts int
			if err := raw.QueryRow(`SELECT status,attempt_count FROM async_jobs WHERE tenant_id=?`, scope.TenantID).Scan(&status, &attempts); err != nil {
				t.Fatal(err)
			}
			want, wantSends := "dead_letter", int32(0)
			if kind == "transport" {
				want = "pending"
				wantSends = 1
			} else if kind == "retry_exhausted" {
				wantSends = 1
			}
			if status != want || attempts != 1 || sends.Load() != wantSends {
				t.Fatalf("terminal policy: %s attempt=%d sends=%d", status, attempts, sends.Load())
			}
		})
	}
}
