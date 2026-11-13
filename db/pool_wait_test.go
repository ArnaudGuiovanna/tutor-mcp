// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A transaction that queues behind the SQLite writer is reported with its
// pool, while a tenant read transaction uses the separate reader pool and
// does not wait for the writer.
func TestTransactionStartWaitIsObservedPerPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	writer, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(writer); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(writer, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedLearner(t, store, "L1")
	if got := store.poolKind(store.reader); got != "reader" {
		t.Fatalf("reader pool kind=%q", got)
	}
	if got := store.poolKind(store.root); got != "writer" {
		t.Fatalf("writer pool kind=%q", got)
	}

	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.inTx(context.Background(), nil, func(*Store) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	scope := models.LegacyPrincipal("L1").TenantScope()
	readStarted := time.Now()
	if err := store.WithTenantReadTx(context.Background(), scope, func(ctx context.Context, scoped storeport.Store) error {
		_, err := scoped.GetLearnerByID(ctx, "L1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(readStarted); elapsed >= slowPoolAcquireThreshold {
		t.Fatalf("reader transaction waited %s behind the writer", elapsed)
	}

	go func() {
		time.Sleep(2 * slowPoolAcquireThreshold)
		close(release)
	}()
	if err := store.inTx(context.Background(), nil, func(*Store) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	if !strings.Contains(output, "database transaction start was slow") || !strings.Contains(output, "pool=writer") {
		t.Fatalf("queued writer start was not reported: %q", output)
	}
	if strings.Contains(output, "pool=reader") {
		t.Fatalf("reader start reported as slow: %q", output)
	}
}
