// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/engine"
)

// PROCESS_ROLE=all on PostgreSQL must run tenant jobs in per-tenant RLS
// scopes, like the dedicated worker. An unscoped scheduler sees no tenant row
// under FORCE ROW LEVEL SECURITY and silently does nothing. SQLite keeps the
// legacy unscoped scheduler.
func TestProcessSchedulerIsTenantScopedOnPostgresOnly(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		driver, mode string
		tenant       bool
	}{
		{"sqlite", "inprocess", false},
		{"postgres", "inprocess", true},
		{"postgres", "distributed", true},
	} {
		store := db.NewStore(nil)
		if tc.driver == "postgres" {
			store = db.NewStoreWithDialect(nil, db.DialectPostgres)
		}
		if got := newProcessScheduler(store, logger, tc.driver, tc.mode).TenantScoped(); got != tc.tenant {
			t.Errorf("%s/%s tenant scoped=%v, want %v", tc.driver, tc.mode, got, tc.tenant)
		}
	}
}

func TestStartSchedulerOnPostgresUsesTenantWorker(t *testing.T) {
	baseDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	if baseDSN == "" {
		t.Skip("set TUTOR_TEST_PG_DSN for the PostgreSQL scheduler wiring regression")
	}
	admin, err := db.OpenPostgres(baseDSN, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("process_scheduler_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("schema cleanup: %v", err)
		}
	}()
	separator := "?"
	if strings.Contains(baseDSN, "?") {
		separator = "&"
	}
	database, err := db.OpenPostgres(baseDSN+separator+"search_path="+schema, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.MigratePostgres(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	store := db.NewStoreWithDialect(database, db.DialectPostgres)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scheduler, err := startScheduler(store, logger, "postgres", "inprocess", engine.SchedulerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	if !scheduler.TenantScoped() {
		t.Fatal("PROCESS_ROLE=all scheduler on PostgreSQL is not tenant scoped")
	}
}
