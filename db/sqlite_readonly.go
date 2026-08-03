// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"
)

// OpenDBReadOnly opens an existing SQLite database without the journal_mode
// pragma used by OpenDB. mode=ro prevents a TOCTOU disappearance from silently
// creating a new file; query_only adds a second database-level write guard.
// This path is intended for diagnostics and retention previews, not migrations.
func OpenDBReadOnly(dbPath string) (*sql.DB, error) {
	dsn, err := sqliteFilesystemDSN(dbPath, "ro", false,
		"busy_timeout(5000)", "foreign_keys(ON)", "query_only(ON)",
	)
	if err != nil {
		return nil, err
	}

	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only db: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(time.Hour)
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("ping read-only db: %w", err)
	}
	return database, nil
}

// sqliteFilesystemDSN treats dbPath strictly as a filesystem path. Encoding it
// as a file URI prevents '?' or '#' in a valid filename from being interpreted
// as attacker-controlled SQLite connection parameters.
func sqliteFilesystemDSN(dbPath, mode string, immediate bool, pragmas ...string) (string, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve SQLite db path: %w", err)
	}
	dsnURL := &url.URL{Scheme: "file", Path: absPath}
	params := url.Values{}
	params.Set("mode", mode)
	for _, pragma := range pragmas {
		params.Add("_pragma", pragma)
	}
	if immediate {
		params.Set("_txlock", "immediate")
	}
	dsnURL.RawQuery = params.Encode()
	return dsnURL.String(), nil
}
