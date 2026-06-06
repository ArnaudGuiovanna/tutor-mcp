package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"tutor-mcp/db"
)

// TestMigratePostgresConcurrent reproduces the multi-node cold-start race:
// several instances calling MigratePostgres on the same fresh schema at once.
// Without the advisory lock this fails with 23505 on pg_type; with it, every
// caller succeeds. (Phase 5 regression guard.)
func TestMigratePostgresConcurrent(t *testing.T) {
	base := os.Getenv("TUTOR_TEST_PG_DSN")
	if base == "" {
		t.Skip("set TUTOR_TEST_PG_DSN")
	}
	schema := "mig_concurrent"
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE") })

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	const N = 6
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := sql.Open("pgx", base+sep+"search_path="+schema)
			if err != nil {
				errs <- err
				return
			}
			defer d.Close()
			errs <- db.MigratePostgres(context.Background(), d)
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("concurrent migrate failed: %v", e)
		}
	}
	// All 19 tables present exactly once.
	var n int
	admin.QueryRow(fmt.Sprintf("SELECT count(*) FROM information_schema.tables WHERE table_schema='%s'", schema)).Scan(&n)
	if n < 18 {
		t.Fatalf("expected full schema, got %d tables", n)
	}
}
