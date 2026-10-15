package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var errTenantReadResetFailure = errors.New("injected query_only reset failure")

type tenantReadDriver struct {
	base                                 driver.Driver
	opened, closed, active, resets       atomic.Int32
	failReset                            atomic.Bool
	badResetContext, resetBeforeRollback atomic.Bool
}

func (d *tenantReadDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	d.opened.Add(1)
	return &tenantReadConn{Conn: c, state: d}, nil
}

type tenantReadConn struct {
	driver.Conn
	state *tenantReadDriver
}

func (c *tenantReadConn) Close() error                           { c.state.closed.Add(1); return c.Conn.Close() }
func (c *tenantReadConn) ResetSession(ctx context.Context) error { return ctx.Err() }
func (c *tenantReadConn) IsValid() bool                          { return true }
func (c *tenantReadConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if v, ok := c.Conn.(driver.QueryerContext); ok {
		return v.QueryContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}
func (c *tenantReadConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if strings.EqualFold(strings.TrimSpace(q), "PRAGMA query_only=0") {
		c.state.resets.Add(1)
		if ctx.Err() != nil {
			c.state.badResetContext.Store(true)
		}
		if _, ok := ctx.Deadline(); !ok {
			c.state.badResetContext.Store(true)
		}
		if c.state.active.Load() != 0 {
			c.state.resetBeforeRollback.Store(true)
		}
		if c.state.failReset.Swap(false) {
			return nil, errTenantReadResetFailure
		}
	}
	if v, ok := c.Conn.(driver.ExecerContext); ok {
		return v.ExecContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}
func (c *tenantReadConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	v, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("test requires actual modernc BeginTx")
	}
	tx, err := v.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.state.active.Add(1)
	return &tenantReadTx{Tx: tx, state: c.state}, nil
}

type tenantReadTx struct {
	driver.Tx
	state *tenantReadDriver
}

func (tx *tenantReadTx) Commit() error   { defer tx.state.active.Add(-1); return tx.Tx.Commit() }
func (tx *tenantReadTx) Rollback() error { defer tx.state.active.Add(-1); return tx.Tx.Rollback() }

func tenantReadFallback(t *testing.T) (*Store, *tenantReadDriver) {
	t.Helper()
	d := &tenantReadDriver{base: &sqlite.Driver{}}
	name := fmt.Sprintf("tenant-read-%d", time.Now().UnixNano())
	sql.Register(name, d)
	dsn, err := sqliteFilesystemDSN(filepath.Join(t.TempDir(), "fallback.db"), "rwc", true)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Exec(`CREATE TABLE read_probe(id INTEGER PRIMARY KEY, value TEXT); INSERT INTO read_probe VALUES (1,'initial')`); err != nil {
		t.Fatal(err)
	}
	return NewStore(database), d
}

func tenantReadRequireReadOnly(t *testing.T, err error) {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&255 != 8 {
		t.Fatalf("expected SQLITE_READONLY, got %v", err)
	}
}

func TestTenantReadReadOnlyIsActualDatabaseRejection(t *testing.T) {
	s := sqliteReaderTestStore(t)
	if _, err := s.root.Exec(`CREATE TABLE read_probe(id INTEGER PRIMARY KEY, value TEXT); INSERT INTO read_probe VALUES (1,'initial')`); err != nil {
		t.Fatal(err)
	}
	for _, target := range []*Store{s, NewStore(s.root)} {
		err := target.WithTenantReadTx(t.Context(), models.LegacyPrincipal("L1").TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
			_, err := target.exec(ctx, `UPDATE read_probe SET value='forbidden' WHERE id=1`)
			tenantReadRequireReadOnly(t, err)
			var id int
			err = scoped.(*Store).queryRow(ctx, `INSERT INTO read_probe VALUES (2,'forbidden') RETURNING id`).Scan(&id)
			tenantReadRequireReadOnly(t, err)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var value string
	if err := s.root.QueryRow(`SELECT COUNT(*), MIN(value) FROM read_probe`).Scan(&count, &value); err != nil || count != 1 || value != "initial" {
		t.Fatalf("data changed: count=%d value=%q err=%v", count, value, err)
	}
}

func TestTenantReadFallbackCleanupCancellationAndDiscard(t *testing.T) {
	for _, mode := range []string{"cancel", "reset_failure", "error_and_reset_failure", "panic_and_reset_failure"} {
		t.Run(mode, func(t *testing.T) {
			s, d := tenantReadFallback(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var err error
			var panicked bool
			func() {
				defer func() {
					if p := recover(); p != nil {
						panicked = true
						if p != "read test panic" {
							t.Fatalf("unexpected panic: %v", p)
						}
					}
				}()
				err = s.WithTenantReadTx(ctx, models.LegacyPrincipal("L1").TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
					var value string
					if err := scoped.(*Store).queryRow(txCtx, `SELECT value FROM read_probe WHERE id=1`).Scan(&value); err != nil {
						return err
					}
					if mode == "cancel" {
						cancel()
						return ctx.Err()
					}
					d.failReset.Store(true)
					if mode == "panic_and_reset_failure" {
						panic("read test panic")
					}
					if mode == "error_and_reset_failure" {
						return errors.New("read callback failure")
					}
					return nil
				})
			}()
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			} else if mode == "panic_and_reset_failure" {
				if !panicked {
					t.Fatal("panic swallowed")
				}
			} else if !errors.Is(err, errTenantReadResetFailure) {
				t.Fatalf("cleanup failure lost: %v", err)
			}
			if d.active.Load() != 0 || d.resetBeforeRollback.Load() {
				t.Fatal("guard reset before transaction finished")
			}
			if d.resets.Load() != 1 || d.badResetContext.Load() {
				t.Fatalf("cleanup did not use independent bounded context: resets=%d", d.resets.Load())
			}
			if mode != "cancel" && d.closed.Load() != 1 {
				t.Fatalf("uncertain guarded connection not discarded: closed=%d", d.closed.Load())
			}
			if _, err := s.root.ExecContext(t.Context(), `UPDATE read_probe SET value='writer still works' WHERE id=1`); err != nil {
				t.Fatalf("writer poisoned: %v", err)
			}
			if mode != "cancel" && d.opened.Load() != 2 {
				t.Fatalf("writer reused discarded connection: opens=%d", d.opened.Load())
			}
		})
	}
}
