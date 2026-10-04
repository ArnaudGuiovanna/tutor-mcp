// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package adminapi

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storeport "tutor-mcp/store"

	"github.com/jackc/pgx/v5/pgconn"
)

func sqliteConstraintError(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", "file:admin_constraint?mode=memory")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id TEXT PRIMARY KEY); INSERT INTO t (id) VALUES ('a')`); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO t (id) VALUES ('a')`)
	if err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	return fmt.Errorf("create cohort: %w", err)
}

// Before the fix every unrecognized error, including a database outage or an
// expired context, became 400 invalid_request and nothing was logged.
func TestWriteStoreErrorClassifiesByType(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantLog    bool
	}{
		{"validation", errors.New("create cohort: invalid name, capacity or dates"), http.StatusBadRequest, "invalid_request", false},
		{"principal", fmt.Errorf("catalog: %w", storeport.ErrInvalidPrincipal), http.StatusForbidden, "forbidden", false},
		{"idempotency", fmt.Errorf("catalog mutation: idempotency key conflict: %w", storeport.ErrIdempotencyKeyConflict), http.StatusConflict, "idempotency_conflict", false},
		{"capacity", storeport.ErrCohortCapacityReached, http.StatusConflict, "cohort_capacity_reached", false},
		{"typed not found", fmt.Errorf("lookup: %w", storeport.ErrNotFound), http.StatusNotFound, "not_found", false},
		{"no rows", fmt.Errorf("enroll membership: cohort or membership not found: %w", sql.ErrNoRows), http.StatusNotFound, "not_found", false},
		{"message not found", errors.New("create cohort: published formation version not found"), http.StatusNotFound, "not_found", false},
		{"deadline", fmt.Errorf("list formations: %w", context.DeadlineExceeded), http.StatusInternalServerError, "internal_error", true},
		{"closed connection", fmt.Errorf("list cohorts: %w", sql.ErrConnDone), http.StatusInternalServerError, "internal_error", true},
		{"postgres shutdown", fmt.Errorf("publish: %w", &pgconn.PgError{Code: "57P01", Message: "terminating connection"}), http.StatusInternalServerError, "internal_error", true},
		{"postgres unique", fmt.Errorf("create: %w", &pgconn.PgError{Code: "23505", Message: "duplicate key"}), http.StatusConflict, "conflict", false},
		{"sqlite unique", sqliteConstraintError(t), http.StatusConflict, "conflict", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			api := New(&catalogStoreStub{}, slog.New(slog.NewTextHandler(&logs, nil)))
			rec := httptest.NewRecorder()
			api.writeStoreError(rec, tc.err)
			if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("status=%d body=%s, want %d %s", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantCode)
			}
			if logged := strings.Contains(logs.String(), "catalog admin request failed"); logged != tc.wantLog {
				t.Fatalf("logged=%v, want %v: %s", logged, tc.wantLog, logs.String())
			}
			if strings.Contains(logs.String(), "terminating connection") {
				t.Fatal("log leaked the driver message")
			}
		})
	}
}
