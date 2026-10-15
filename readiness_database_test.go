package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"tutor-mcp/db"
)

func TestReadinessDetectsDatabaseAndLedgerFailuresAfterCachedSuccess(t *testing.T) {
	database, err := db.OpenDB(filepath.Join(t.TempDir(), "readiness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	ready := readinessHandler(db.NewStore(database))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", ready)
	mux.HandleFunc("GET /health", ready)
	check := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	waitFor := func(path string, want int) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for {
			w := check(path)
			if w.Code == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s status=%d, want %d after cache expiry: %s", path, w.Code, want, w.Body.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if w := check("/ready"); w.Code != http.StatusOK {
		t.Fatalf("healthy database status=%d: %s", w.Code, w.Body.String())
	}
	var version, checksum string
	if err := database.QueryRow(`SELECT version, checksum FROM schema_migrations ORDER BY version LIMIT 1`).Scan(&version, &checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE schema_migrations SET checksum = 'drift' WHERE version = ?`, version); err != nil {
		t.Fatal(err)
	}
	waitFor("/health", http.StatusServiceUnavailable)
	if _, err := database.Exec(`UPDATE schema_migrations SET checksum = ? WHERE version = ?`, checksum, version); err != nil {
		t.Fatal(err)
	}
	waitFor("/ready", http.StatusOK)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor("/health", http.StatusServiceUnavailable)
}
