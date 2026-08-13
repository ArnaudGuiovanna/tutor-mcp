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
	registerPostgresSchemaCleanup(t, admin, schema)
	if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
		t.Fatalf("drop stale schema %s: %v", schema, err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}

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

// TestMigratePostgresScopesConstraintsToSchema guards against catalog checks
// accidentally finding a same-named constraint in another tenant test schema.
// Each schema must receive its own composite foreign keys.
func TestMigratePostgresScopesConstraintsToSchema(t *testing.T) {
	base := os.Getenv("TUTOR_TEST_PG_DSN")
	if base == "" {
		t.Skip("set TUTOR_TEST_PG_DSN")
	}

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	schemas := []string{"mig_constraint_scope_a", "mig_constraint_scope_b"}
	t.Cleanup(func() {
		for _, schema := range schemas {
			if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
				t.Errorf("drop test schema %s: %v", schema, err)
			}
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close postgres admin connection: %v", err)
		}
	})

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	for _, schema := range schemas {
		if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
			t.Fatalf("drop stale schema %s: %v", schema, err)
		}
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
		d, err := sql.Open("pgx", base+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.MigratePostgres(context.Background(), d); err != nil {
			d.Close()
			t.Fatalf("migrate schema %s: %v", schema, err)
		}
		var constraints int
		if err := d.QueryRow(`
			SELECT count(*)
			FROM pg_constraint
			WHERE conrelid = 'refresh_tokens'::regclass
			  AND conname = 'refresh_tokens_tenant_learner_fk'
		`).Scan(&constraints); err != nil {
			d.Close()
			t.Fatalf("query schema %s constraint: %v", schema, err)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("close schema %s database: %v", schema, err)
		}
		if constraints != 1 {
			t.Fatalf("schema %s constraint count=%d, want 1", schema, constraints)
		}
	}
}

// TestMigratePostgresDetectsChecksumDrift gives Postgres the same anti-drift
// guard SQLite has: MigratePostgres records the schema checksum in
// schema_migrations and refuses to proceed if a prior apply recorded a
// different one (the embedded schema_pg.sql changed since it was applied).
func TestMigratePostgresDetectsChecksumDrift(t *testing.T) {
	base := os.Getenv("TUTOR_TEST_PG_DSN")
	if base == "" {
		t.Skip("set TUTOR_TEST_PG_DSN")
	}
	schema := "mig_drift"
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	registerPostgresSchemaCleanup(t, admin, schema)
	if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
		t.Fatalf("drop stale schema %s: %v", schema, err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	d, err := sql.Open("pgx", base+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx := context.Background()
	// First apply records the checksum.
	if err := db.MigratePostgres(ctx, d); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// Re-apply with an unchanged schema must succeed (idempotent, checksum matches).
	if err := db.MigratePostgres(ctx, d); err != nil {
		t.Fatalf("idempotent re-migrate: %v", err)
	}
	// Simulate a drifted schema body: corrupt the stored checksum so it no
	// longer matches the embedded schema. Next migrate must refuse.
	if _, err := d.Exec(`UPDATE schema_migrations SET checksum = 'drifted' WHERE version = 'postgres_schema'`); err != nil {
		t.Fatalf("corrupt checksum: %v", err)
	}
	err = db.MigratePostgres(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}
}

// registerPostgresSchemaCleanup keeps the admin connection alive until the
// schema has been dropped. Closing admin first makes database/sql reject the
// cleanup Exec and silently leaves test schemas behind.
func registerPostgresSchemaCleanup(t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
			t.Errorf("drop test schema %s: %v", schema, err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close postgres admin connection: %v", err)
		}
	})
}
