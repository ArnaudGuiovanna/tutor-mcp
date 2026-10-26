// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	storeport "tutor-mcp/store"
)

type cleanupSQLFailure struct{}

func (cleanupSQLFailure) Error() string    { return "credential-bearing database error must stay private" }
func (cleanupSQLFailure) SQLState() string { return "42501" }

type cleanupStopsStore struct {
	storeport.Store
	calls int
}

func (s *cleanupStopsStore) CleanupExpiredCodes(context.Context) (int64, error) {
	s.calls++
	return 0, fmt.Errorf("wrapped: %w", cleanupSQLFailure{})
}
func (s *cleanupStopsStore) CleanupExpiredRefreshTokens(context.Context) (int64, error) {
	s.calls++
	return 0, nil
}

func TestCleanupStopsAfterFirstTransactionFailureAndLogsOnlySQLState(t *testing.T) {
	var output bytes.Buffer
	store := &cleanupStopsStore{}
	scheduler := &Scheduler{store: store, logger: slog.New(slog.NewTextHandler(&output, nil))}
	result := scheduler.cleanupExpiredData()
	if !result.failed() || store.calls != 1 {
		t.Fatalf("failure=%s calls=%d", result.FailureCode, store.calls)
	}
	if !strings.Contains(output.String(), "sqlstate=42501") {
		t.Fatal("safe SQLSTATE missing")
	}
	if strings.Contains(output.String(), "credential-bearing") {
		t.Fatal("database error detail leaked")
	}
}
