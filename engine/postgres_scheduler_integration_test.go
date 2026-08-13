// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
)

// TestPostgresSchedulerRecoversLeaseAndBusinessRetry exercises the scheduler
// orchestration itself on PostgreSQL. Store-only lease tests cannot detect a
// scheduler that forgets to poll, dispatch, or checkpoint the recovered job.
func TestPostgresSchedulerRecoversLeaseAndBusinessRetry(t *testing.T) {
	baseDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	if baseDSN == "" {
		t.Skip("set TUTOR_TEST_PG_DSN to run the PostgreSQL scheduler gate")
	}
	raw, store := postgresSchedulerTestStore(t, baseDSN)
	scheduler := distributedSchedulerForLeaseTest(store)
	scheduler.jobRetryDelay = 10 * time.Millisecond
	ctx := context.Background()
	now := time.Now().UTC()
	ok, err := store.AcquireJobRunLease(ctx, "pg_recovery", "window", "dead-node", now, 20*time.Millisecond, 3)
	if err != nil || !ok {
		t.Fatalf("seed crashed PostgreSQL lease: ok=%v err=%v", ok, err)
	}
	var recovered atomic.Int64
	scheduler.registeredDistributed["pg_recovery"] = func() scheduledJobResult {
		recovered.Add(1)
		return scheduledJobSucceeded()
	}
	time.Sleep(30 * time.Millisecond)
	scheduler.retryRunnableJobs()
	if recovered.Load() != 1 {
		t.Fatalf("recovered PostgreSQL executions=%d, want 1", recovered.Load())
	}
	assertPostgresSchedulerState(t, raw, "pg_recovery", "window", "succeeded", 2)

	scheduler.runDistributedJob("pg_business", "window", func() scheduledJobResult {
		return scheduledJobFailed("dispatch_partial_failure")
	})
	time.Sleep(15 * time.Millisecond)
	scheduler.runDistributedJob("pg_business", "window", scheduledJobSucceeded)
	assertPostgresSchedulerState(t, raw, "pg_business", "window", "succeeded", 2)
}

func assertPostgresSchedulerState(t *testing.T, raw *sql.DB, name, window, status string, attempts int) {
	t.Helper()
	var gotStatus string
	var gotAttempts int
	if err := raw.QueryRow(
		`SELECT status, attempts FROM scheduled_job_runs WHERE name = $1 AND window_key = $2`, name, window,
	).Scan(&gotStatus, &gotAttempts); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || gotAttempts != attempts {
		t.Fatalf("PostgreSQL job=%s/%d, want %s/%d", gotStatus, gotAttempts, status, attempts)
	}
}

func postgresSchedulerTestStore(t *testing.T, baseDSN string) (*sql.DB, *db.Store) {
	t.Helper()
	const schema = "p1_engine_scheduler"
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(baseDSN, "?") {
		separator = "&"
	}
	raw, err := db.OpenPostgres(baseDSN+separator+"search_path="+schema, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MigratePostgres(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = raw.Close()
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		_ = admin.Close()
	})
	return raw, db.NewStoreWithDialect(raw, db.DialectPostgres)
}
