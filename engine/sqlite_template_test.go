// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// migratedSQLiteCopy opens a private copy of the package's migrated schema
// template. Migrating once per package instead of once per test keeps the
// engine suite inside its CI budget under the race detector. Pragmas are
// applied as given (foreign keys stay off unless requested, as with the
// former per-test in-memory databases).
func migratedSQLiteCopy(t *testing.T, pragmas ...string) *sql.DB {
	t.Helper()
	template, err := saasConsumerTemplateBytes()
	if err != nil {
		t.Fatalf("build SQLite schema template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "engine.db")
	if err := os.WriteFile(path, template, 0o600); err != nil {
		t.Fatalf("copy SQLite schema template: %v", err)
	}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	for _, pragma := range pragmas {
		query.Add("_pragma", pragma)
	}
	raw, err := sql.Open("sqlite", "file:"+path+"?"+query.Encode())
	if err != nil {
		t.Fatalf("open SQLite copy: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}
