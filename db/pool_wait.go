// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// slowPoolAcquireThreshold is the transaction start latency from which a
// debug log line is emitted. Ordinary acquisitions take well under a
// millisecond; a value above this means the caller queued behind another
// holder of the pool (or, on SQLite, behind the writer lock).
const slowPoolAcquireThreshold = 50 * time.Millisecond

var poolAcquireHistogram = sync.OnceValue(func() metric.Float64Histogram {
	histogram, err := otel.Meter("tutor-mcp/database").Float64Histogram("tutor.db.acquire.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time to start a database transaction, including the wait for a pooled connection"),
		metric.WithExplicitBucketBoundaries(.0005, .001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5))
	if err != nil {
		return nil
	}
	return histogram
})

// poolKind names the physical pool behind database, matching the pool
// attribute of the tutor.db.connections gauges.
func (s *Store) poolKind(database *sql.DB) string {
	switch {
	case s.reader != nil && database == s.reader && s.reader != s.root:
		return "reader"
	case s.reader != nil && s.reader != s.root:
		return "writer"
	default:
		return "shared"
	}
}

// beginObservedTx starts a transaction on database and records how long the
// start took: the wait for a free pooled connection plus BEGIN itself (on
// SQLite, BEGIN IMMEDIATE also waits for the single writer lock). The
// histogram tutor.db.acquire.duration{pool} complements the cumulative
// tutor.db.wait.duration counter; slow starts are also logged at debug level
// with the pool occupancy observed at that moment.
func (s *Store) beginObservedTx(ctx context.Context, database *sql.DB, opts *sql.TxOptions) (*sql.Tx, error) {
	started := time.Now()
	tx, err := database.BeginTx(ctx, opts)
	observePoolAcquire(ctx, s.poolKind(database), database, time.Since(started), err)
	return tx, err
}

func observePoolAcquire(ctx context.Context, pool string, database *sql.DB, elapsed time.Duration, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	if histogram := poolAcquireHistogram(); histogram != nil {
		histogram.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
			attribute.String("pool", pool), attribute.String("outcome", outcome)))
	}
	if elapsed < slowPoolAcquireThreshold {
		return
	}
	stats := database.Stats()
	slog.Default().Debug("database transaction start was slow",
		"pool", pool, "outcome", outcome, "wait_ms", elapsed.Milliseconds(),
		"in_use", stats.InUse, "max_open", stats.MaxOpenConnections,
		"cumulative_waits", stats.WaitCount)
}
