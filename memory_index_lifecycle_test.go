package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"

	"modernc.org/sqlite"
)

type narrativeIndexBlockingDriver struct {
	base                       driver.Driver
	armed                      atomic.Bool
	active                     atomic.Int32
	entered, canceled, release chan struct{}
	enterOnce, cancelOnce      sync.Once
}

func (d *narrativeIndexBlockingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &narrativeIndexBlockingConn{Conn: conn, state: d}, nil
}

type narrativeIndexBlockingConn struct {
	driver.Conn
	state *narrativeIndexBlockingDriver
}

func (c *narrativeIndexBlockingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.state.armed.Load() && strings.Contains(query, "SELECT id FROM tenants") {
		c.state.active.Add(1)
		defer c.state.active.Add(-1)
		c.state.enterOnce.Do(func() { close(c.state.entered) })
		<-ctx.Done()
		c.state.cancelOnce.Do(func() { close(c.state.canceled) })
		// Deliberately do not finish when canceled: Stop must join the real
		// in-flight operation, rather than merely signal its cancellation.
		<-c.state.release
		return nil, ctx.Err()
	}
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		return queryer.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}
func (c *narrativeIndexBlockingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if execer, ok := c.Conn.(driver.ExecerContext); ok {
		return execer.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}
func (c *narrativeIndexBlockingConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, options)
	}
	return nil, errors.New("test driver requires ConnBeginTx")
}

func TestNarrativeIndexRepairStopJoinsInFlightDatabaseWork(t *testing.T) {
	state := &narrativeIndexBlockingDriver{base: &sqlite.Driver{}, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	driverName := fmt.Sprintf("narrative-index-repair-%d", time.Now().UnixNano())
	sql.Register(driverName, state)
	database, err := sql.Open(driverName, filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	persistence := db.NewStore(database)
	keyring, err := db.NewIntegrationSecretKeyring("primary:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "primary")
	if err != nil {
		t.Fatal(err)
	}
	persistence.SetIntegrationSecretKeyring(keyring)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(state.release) }) }
	defer release()
	stop := startNarrativeIndexRepair(context.Background(), persistence, slog.New(slog.NewTextHandler(io.Discard, nil)), true, 10*time.Millisecond)
	defer func() { release(); stop() }()
	state.armed.Store(true)
	select {
	case <-state.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not reach the database")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-state.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel the in-flight query")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned before database work actually finished")
	default:
	}
	if state.active.Load() != 1 {
		t.Fatal("expected one unfinished query")
	}
	release()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not join the finished query")
	}
	if state.active.Load() != 0 {
		t.Fatal("database work remained after Stop")
	}
	if err := database.PingContext(context.Background()); err != nil {
		t.Fatalf("Stop unexpectedly closed the database: %v", err)
	}
}
