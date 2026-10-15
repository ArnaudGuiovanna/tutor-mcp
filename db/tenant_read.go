package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"time"

	"tutor-mcp/observability"
)

// NewSQLiteStore attaches an independent four-connection reader to an already
// migrated writer pool. Ownership of the writer transfers only on success.
// The paths must identify the same existing database; no reader can create it.
func NewSQLiteStore(writer *sql.DB, path string) (*Store, error) {
	if writer == nil {
		return nil, fmt.Errorf("SQLite store: missing writer")
	}
	var seq int
	var name, writerPath string
	if err := writer.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &writerPath); err != nil {
		return nil, fmt.Errorf("SQLite store: identify writer: %w", err)
	}
	writerInfo, err := os.Stat(writerPath)
	if err != nil {
		return nil, fmt.Errorf("SQLite store: writer must use an existing file: %w", err)
	}
	readerInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(writerInfo, readerInfo) {
		return nil, fmt.Errorf("SQLite store: reader and writer database mismatch")
	}
	reader, err := OpenDBReadOnly(path)
	if err != nil {
		return nil, err
	}
	reader.SetMaxOpenConns(4)
	reader.SetMaxIdleConns(4)
	s := NewStore(writer)
	s.reader = reader
	return s, nil
}

// RegisterStoreDBPools observes each physical pool once. PostgreSQL and rawDB
// embedders have one shared pool; configured SQLite has writer and reader.
// This function is lifecycle wiring, not an unscoped Store business method.
func RegisterStoreDBPools(s *Store) (func() error, error) {
	kind := "shared"
	if s.reader != nil && s.reader != s.root {
		kind = "writer"
	}
	unregisterWriter, err := observability.RegisterNamedDBPool(s.root, kind)
	if err != nil {
		return nil, err
	}
	if kind == "shared" {
		return unregisterWriter, nil
	}
	unregisterReader, err := observability.RegisterNamedDBPool(s.reader, "reader")
	if err != nil {
		_ = unregisterWriter()
		return nil, err
	}
	return func() error { return errors.Join(unregisterReader(), unregisterWriter()) }, nil
}

// Legacy NewStore callers may not have a separate reader. Enforce read-only
// on their existing connection rather than silently permitting writes. This
// mode shares writer capacity and makes no concurrency guarantee.
func beginSQLiteReadFallback(ctx context.Context, pool *sql.DB) (*sql.Tx, func() error, error) {
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	var previous int
	if err := conn.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&previous); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	cleanup := func() error {
		resetCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, resetErr := conn.ExecContext(resetCtx, fmt.Sprintf(`PRAGMA query_only=%d`, previous))
		if resetErr != nil {
			// Never return a connection with an uncertain guard to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		closeErr := conn.Close()
		return errors.Join(resetErr, closeErr)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	return tx, cleanup, nil
}
