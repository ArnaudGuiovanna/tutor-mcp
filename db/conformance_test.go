// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"testing"

	"tutor-mcp/store"
	"tutor-mcp/store/conformance"
)

// TestSQLiteConformance runs the shared store.Store contract suite against the
// SQLite implementation. When Phase 2 adds PostgresStore it will call
// conformance.RunConformance with its own newStore factory — same suite,
// different backend.
func TestSQLiteConformance(t *testing.T) {
	conformance.RunConformance(t, func(t *testing.T) store.Store {
		t.Helper()
		return setupTestDB(t)
	})
}
