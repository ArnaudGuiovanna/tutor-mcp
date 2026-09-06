// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
)

// Foreign keys must be disabled BEFORE BEGIN for SQLite's table-rebuild
// procedure, then checked before commit and restored on the same connection.
// See https://www.sqlite.org/lang_altertable.html#otheralter
func sqliteCurriculumRebuildPending(ctx context.Context, conn *sql.Conn) (bool, error) {
	var tables int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('curriculum_versions','schema_migrations')`).Scan(&tables); err != nil {
		return false, err
	}
	if tables != 2 {
		return false, nil
	} // fresh schema: no referenced rows to move
	var applied int
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = '0065_curriculum_reconciliation'`).Scan(&applied)
	return applied == 0, err
}

func checkSQLiteForeignKeys(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("foreign_key_check failed after curriculum table rebuild; migration rolled back")
	}
	return rows.Err()
}
