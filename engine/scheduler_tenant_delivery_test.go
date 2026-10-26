// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type failingTenantCompletionStore struct{ *db.Store }

func (s failingTenantCompletionStore) WithTenantTx(ctx context.Context, scope models.TenantScope, fn func(context.Context, storeport.Store) error) error {
	return s.Store.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		return fn(txCtx, completeWebhookFailStore{Store: scoped})
	})
}

func TestTenantSchedulerWebhookBoundaryAndRecovery(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			allowAnyURL(t)
			raw, fixture, runtime := tenantDeliveryTestStore(t, backend)
			id := enqueueSchedulerDelivery(t, fixture, "L1", models.WebhookKindDailyMotivation)
			// A real tenant scheduler, with only the completion statement failing.
			// The registered production callback must commit its boundary first.
			scheduler := NewTenantDistributedScheduler(failingTenantCompletionStore{runtime}, quietLogger(), "boundary-test")
			var requests atomic.Int32
			scheduler.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				// SQLite has one connection; PostgreSQL's runtime pool has one too.
				// An HTTP call retaining its transaction makes this probe time out.
				if err := runtime.RawDB().PingContext(ctx); err != nil {
					t.Errorf("SQL connection retained during HTTP: %v", err)
				}
				var status string
				query := `SELECT status FROM webhook_message_queue WHERE id = ?`
				if backend == "postgres" {
					query = `SELECT status FROM webhook_message_queue WHERE id = $1`
				}
				if err := raw.QueryRowContext(ctx, query, id).Scan(&status); err != nil {
					t.Error(err)
				}
				if status != models.WebhookStatusDispatching {
					t.Errorf("uncommitted HTTP boundary: %s", status)
				}
				// Prove that the queue row is not locked throughout the network
				// wait. This is a separate SQL transaction/connection on PostgreSQL.
				update := `UPDATE webhook_message_queue SET priority=priority WHERE id=?`
				if backend == "postgres" {
					update = `UPDATE webhook_message_queue SET priority=priority WHERE id=$1`
				}
				if _, err := raw.ExecContext(ctx, update, id); err != nil {
					t.Errorf("SQL row locked during HTTP: %v", err)
				}
				return webhookResponse(http.StatusNoContent), nil
			})
			if err := scheduler.Start(); err != nil {
				t.Fatal(err)
			}
			defer scheduler.Stop()
			scheduler.runDistributedJob("motivation", "boundary-test-window", scheduler.registeredDistributed["motivation"])
			if requests.Load() != 1 {
				t.Fatalf("HTTP requests=%d", requests.Load())
			}
			// Completion failed after 204. Reconciliation must quarantine the
			// committed boundary instead of making this message runnable again.
			worker := models.WorkerPrincipal{ActorID: "boundary-test"}
			scopes, _, err := fixture.ListWorkerTenantScopes(context.Background(), worker, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range scopes {
				if err := runtime.WithTenantTx(context.Background(), scope, func(ctx context.Context, scoped storeport.Store) error {
					_, err := scoped.RequeueStaleWebhookClaims(ctx, time.Now().UTC().Add(time.Hour), time.Now().UTC().Add(time.Hour))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			scheduler.runDistributedJob("motivation", "next-test-window", scheduler.registeredDistributed["motivation"])
			if requests.Load() != 1 {
				t.Fatalf("ambiguous delivery replayed: %d requests", requests.Load())
			}
			query := `SELECT status FROM webhook_message_queue WHERE id=?`
			if backend == "postgres" {
				query = `SELECT status FROM webhook_message_queue WHERE id=$1`
			}
			var status string
			if err := raw.QueryRow(query, id).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != models.WebhookStatusDeliveryUnknown {
				t.Fatalf("after recovery status=%s", status)
			}
		})
	}
}

func TestSchedulerRejectsHTTPInsideTenantDatabasePhase(t *testing.T) {
	_, store, _ := rawTestSetup(t, "")
	scheduler := NewTenantDistributedScheduler(store, quietLogger(), "phase-test")
	var requests atomic.Int32
	scheduler.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) { requests.Add(1); return webhookResponse(204), nil })
	var phases int
	assertRejected := func(ctx context.Context, child *Scheduler) error {
		phases++
		err := child.dispatchDurableWebhook(ctx, "L1", "test", "", false, []int64{1}, discordPayload{}, time.Now().UTC())
		if !child.tenantDBPhase || err == nil || requests.Load() != 0 {
			t.Errorf("database-phase dispatch phase=%v err=%v requests=%d", child.tenantDBPhase, err, requests.Load())
		}
		return nil
	}
	scheduler.runAcrossTenants("guard-regression", func(child *Scheduler) scheduledJobResult {
		_ = assertRejected(context.Background(), child)
		return scheduledJobSucceeded()
	})
	scopes, _, err := store.ListWorkerTenantScopes(context.Background(), scheduler.worker, "", 100)
	if err != nil || len(scopes) != 1 {
		t.Fatalf("worker scopes=%v err=%v", scopes, err)
	}
	scheduler.currentTenantScope = &scopes[0]
	if err := scheduler.withTenantStore(context.Background(), assertRejected); err != nil {
		t.Fatal(err)
	}
	if phases != 2 {
		t.Fatalf("guard checked %d phases, want2", phases)
	}
}

func TestIndependentSchedulerJobsKeepLocalFallback(t *testing.T) {
	store, _ := newJobsTestStore(t, "")
	scheduler := NewScheduler(store, quietLogger())
	var calls int
	result := scheduler.scheduledIndependentTenantJob("local", func(child *Scheduler) scheduledJobResult {
		calls++
		if child != scheduler || child.tenantDBPhase {
			t.Error("local scheduler acquired a tenant database phase")
		}
		return scheduledJobSucceeded()
	})()
	if result.failed() || calls != 1 {
		t.Fatalf("fallback result=%+v calls=%d", result, calls)
	}
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	// Start's first entry is OLM. With no targets this must succeed locally;
	// a broken independent registration reports worker_identity_invalid.
	for _, entry := range scheduler.cron.Entries() {
		entry.Job.Run()
	}
}

// The PostgreSQL variant uses a non-owning, NOBYPASSRLS role and one SQL
// connection. Grants mirror only the scheduler's existing worker privileges.
func tenantDeliveryTestStore(t *testing.T, backend string) (*sql.DB, *db.Store, *db.Store) {
	t.Helper()
	if backend == "sqlite" {
		raw, store, _ := rawTestSetup(t, "https://example.invalid/hook")
		if err := store.EnsureRecoveryEnrollment(context.Background(), "L1"); err != nil {
			t.Fatal(err)
		}
		assertTenantDeliveryIsolation(t, store, store)
		return raw, store, store
	}
	dsn := os.Getenv("TUTOR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set TUTOR_TEST_PG_DSN to run restricted worker PostgreSQL regression")
	}
	raw, fixture := postgresSchedulerTestStore(t, dsn)
	now := time.Now().UTC()
	if _, err := raw.Exec(`INSERT INTO learners(id,email,password_hash,objective,webhook_url,created_at,last_active,email_verified_at)
		VALUES ('L1','tenant-delivery@example.test','hash','objective','https://example.invalid/hook',$1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if err := fixture.EnsureRecoveryEnrollment(context.Background(), "L1"); err != nil {
		t.Fatal(err)
	}
	optInSchedulerNotifications(t, fixture, "L1")
	const role = "p1_engine_scheduler_worker"
	if _, err := raw.Exec(`DROP ROLE IF EXISTS ` + role + `; CREATE ROLE ` + role + ` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
		GRANT USAGE ON SCHEMA p1_engine_scheduler TO ` + role + `;
		GRANT SELECT ON tenants,learners,availability,domains,concept_states,interactions,enrollments,legacy_concept_mappings,
		learner_concept_states,assessment_attempts TO ` + role + `;
		GRANT UPDATE ON learners TO ` + role + `;
		GRANT SELECT,INSERT,UPDATE,DELETE ON worker_tenant_runs,webhook_message_queue,webhook_delivery_transitions,
		webhook_push_log,scheduled_alerts,scheduled_job_runs TO ` + role + `;
		GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA p1_engine_scheduler TO ` + role); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = "p1_engine_scheduler"
	runtimeDB := stdlib.OpenDB(*config, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE `+role)
		return err
	}))
	runtimeDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = runtimeDB.Close()
		if _, err := raw.Exec(`DROP OWNED BY ` + role + `; DROP ROLE ` + role); err != nil {
			t.Errorf("worker role cleanup: %v", err)
		}
	})
	runtime := db.NewStoreWithDialect(runtimeDB, db.DialectPostgres)
	var super, bypass bool
	if err := runtimeDB.QueryRow(`SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatal("runtime worker bypasses RLS")
	}
	var visible int
	if err := runtimeDB.QueryRow(`SELECT COUNT(*) FROM learners`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("unscoped worker sees %d learners", visible)
	}
	if _, err := runtimeDB.Exec(`SELECT password_hash FROM users`); err == nil {
		t.Fatal("worker can read global account credentials")
	}
	assertTenantDeliveryIsolation(t, fixture, runtime)
	return raw, fixture, runtime
}

func assertTenantDeliveryIsolation(t *testing.T, fixture, runtime *db.Store) {
	t.Helper()
	ctx := context.Background()
	other, err := fixture.ProvisionTenant(ctx, models.ControlPlanePrincipal{
		ActorID: "test-operator", Roles: []string{models.RolePlatformAdmin}, Reason: "isolation regression", RequestID: "isolation-regression",
	}, "foreign-scheduler-tenant", "Foreign scheduler tenant", "test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	foreignScope := models.TenantScope{TenantID: other.ID, UserID: "worker_boundary-test", MembershipID: "worker_process"}
	if err := runtime.WithTenantTx(ctx, foreignScope, func(txCtx context.Context, scoped storeport.Store) error {
		if _, err := scoped.GetWebhookDispatchURL(txCtx, "L1"); err == nil {
			t.Error("foreign tenant could read learner webhook credential")
		}
		targets, err := scoped.ListWebhookDispatchTargetsPage(txCtx, "", 100)
		if err != nil {
			return err
		}
		if len(targets) != 0 {
			t.Errorf("foreign tenant enumerated %d learner targets", len(targets))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Erasure mechanics require an active successor when their victim is an owner.
// Use the membership service so the fixture obeys the production invariant.
func ensureDSARReplacementOwner(t *testing.T, store *db.Store, owner models.Principal) {
	t.Helper()
	next, err := store.CreateLearner(t.Context(), "dsar-successor@example.test", "hash", "Institution successor", "")
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.GetPrincipalForLearner(t.Context(), next.ID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if member.TenantID != owner.TenantID {
		t.Fatal("DSAR fixture successor belongs to another institution")
	}
	if err := store.UpdateTenantMember(t.Context(), owner, member.MembershipID, models.MembershipStatusActive, []string{models.RoleOwner, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
}

func TestSaaSWorkerDSARHoldParksAndResumesThroughQueue(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			raw, store, _ := tenantDeliveryTestStore(t, backend)
			ctx := context.Background()
			p, err := store.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetMembershipAuthorization(ctx, p.TenantScope(), models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.RecordMembershipMFAVerification(ctx, p.TenantScope(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			owner, err := store.GetPrincipalForLearner(ctx, "L1", []string{models.OAuthScopeLearner})
			if err != nil {
				t.Fatal(err)
			}
			ensureDSARReplacementOwner(t, store, owner)
			request, err := store.RequestTenantDSAR(ctx, owner, "L1", "erase", "verified request")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CreateRetentionLegalHold(ctx, db.RetentionLegalHold{HoldID: "queue-hold", LearnerID: "L1", Reason: "litigation", CreatedBy: "legal", CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			scheduler := NewTenantDistributedScheduler(store, quietLogger(), "dsar-test")
			scope := owner.TenantScope()
			scope.UserID = "worker_dsar-test"
			scope.MembershipID = "worker_process"
			scheduler.currentTenantScope = &scope
			for i := 0; i < 4; i++ {
				if result := scheduler.consumeSaaSJobs(); result.failed() {
					t.Fatalf("hold treated as technical failure: %+v", result)
				}
			}
			stateQuery := `SELECT r.status,j.status,j.attempt_count FROM tenant_dsar_requests r
				JOIN async_jobs j ON j.tenant_id=r.tenant_id AND j.idempotency_key=r.id WHERE r.id=?`
			if backend == "postgres" {
				stateQuery = `SELECT r.status,j.status,j.attempt_count FROM tenant_dsar_requests r
				JOIN async_jobs j ON j.tenant_id=r.tenant_id AND j.idempotency_key=r.id WHERE r.id=$1`
			}
			var requestStatus, jobStatus string
			var attempts int
			if err := raw.QueryRow(stateQuery, request.ID).Scan(&requestStatus, &jobStatus, &attempts); err != nil {
				t.Fatal(err)
			}
			if requestStatus != "blocked" || jobStatus != "pending" || attempts != 0 {
				t.Fatalf("suspended request/job/attempts=%s/%s/%d", requestStatus, jobStatus, attempts)
			}
			job, err := store.ClaimAsyncJob(ctx, scope, "poller", time.Now().UTC().Add(48*time.Hour), time.Minute)
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("blocked request was claimable job=%+v err=%v", job, err)
			}
			if err := store.ResumeTenantDSAR(ctx, owner, request.ID); !errors.Is(err, storeport.ErrTenantDSARBlocked) {
				t.Fatalf("active hold resume=%v", err)
			}
			if _, err := store.ReleaseRetentionLegalHold(ctx, "queue-hold", "legal", "matter closed", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := store.ResumeTenantDSAR(ctx, owner, request.ID); err != nil {
				t.Fatal(err)
			}
			if result := scheduler.consumeSaaSJobs(); result.failed() {
				t.Fatalf("resumed queue failed: %+v", result)
			}
			// Erasure is complete only if both durable states converge. Worker
			// success alone could otherwise hide a suspended or abandoned job.
			completed, _, err := store.ProcessTenantDSARErasureBatch(ctx, scope, scheduler.worker, request.ID, 500, time.Now().UTC())
			if err != nil || !completed {
				t.Fatalf("erasure request completed=%v err=%v", completed, err)
			}
			if err := raw.QueryRow(stateQuery, request.ID).Scan(&requestStatus, &jobStatus, &attempts); err != nil {
				t.Fatal(err)
			}
			if requestStatus != "completed" || jobStatus != "completed" {
				t.Fatalf("resumed request/job=%s/%s", requestStatus, jobStatus)
			}
			if _, err := store.ClaimAsyncJob(ctx, scope, "poller", time.Now().UTC().Add(48*time.Hour), time.Minute); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("completed erasure reclaimed: %v", err)
			}
		})
	}
}
