package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
)

// PostgreSQL business tests each get their own database cloned from one
// migrated template database built once per package run. CREATE DATABASE
// ... TEMPLATE copies the catalog at file level, which costs a small fraction
// of replaying the base schema and every migration for each test. The copy
// keeps the historical isolation contract: a migrated schema owned by the
// test alone, reached through search_path. It is also stricter than sharing
// one database: catalog lookups that do not filter by schema (constraint
// names in the historical migration ledger) cannot see another test.
//
// Every copy keeps the template's schema name rather than renaming it to a
// per-test t_<n>: migrations may record current_schema() in the catalog (for
// example a SECURITY DEFINER function's SET search_path), and a rename would
// silently leave those settings pointing at a schema that no longer exists.
// Uniqueness is unnecessary because each copy is its own database; roles
// created by tests remain cluster-wide and keep their counter-based names.
//
// When the DSN is not a URL or the role may not create databases, setupTestPG
// falls back to a fresh schema per test in the configured database.

const postgresTestTemplateSchema = "t_template"

var postgresTestTemplate struct {
	sync.Once
	admin    *sql.DB  // connected to the configured database
	base     *url.URL // configured DSN, database replaced per copy
	database string   // template database; empty when unavailable
	err      error
}

// TestMain drops the package's PostgreSQL template database, if any, once
// every test has released its copy.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := dropPostgresTestTemplate(); err != nil {
		fmt.Fprintf(os.Stderr, "drop PostgreSQL test template: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// postgresTestTemplateDatabase returns the migrated template database and an
// admin pool on the configured database. database is empty when the server or
// DSN does not allow cloning; callers then provision a schema instead.
func postgresTestTemplateDatabase(baseDSN string) (admin *sql.DB, base *url.URL, database string, err error) {
	tpl := &postgresTestTemplate
	tpl.Do(func() { tpl.err = buildPostgresTestTemplate(baseDSN) })
	return tpl.admin, tpl.base, tpl.database, tpl.err
}

func buildPostgresTestTemplate(baseDSN string) error {
	tpl := &postgresTestTemplate
	ctx := context.Background()
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		return fmt.Errorf("pg admin open: %w", err)
	}
	tpl.admin = admin
	base, err := url.Parse(baseDSN)
	if err != nil || (base.Scheme != "postgres" && base.Scheme != "postgresql") {
		return nil // keyword/value DSN: schema-per-test fallback.
	}
	var canCreate bool
	if err := admin.QueryRowContext(ctx,
		`SELECT rolsuper OR rolcreatedb FROM pg_roles WHERE rolname = current_user`,
	).Scan(&canCreate); err != nil {
		return fmt.Errorf("pg inspect current role: %w", err)
	}
	if !canCreate {
		return nil
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	// Lowercase letters, digits and underscores only: a valid unquoted
	// identifier, at most 63 bytes once a copy suffix is appended.
	database := fmt.Sprintf("tutor_db_test_%d_%s", os.Getpid(), hex.EncodeToString(suffix))
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+database); err != nil {
		return fmt.Errorf("pg create template database: %w", err)
	}
	tpl.base, tpl.database = base, database
	if err := migratePostgresTestTemplate(ctx, base, database); err != nil {
		return err
	}
	// Nobody may connect to the template: CREATE DATABASE ... TEMPLATE
	// refuses to copy a database that has other sessions.
	if _, err := admin.ExecContext(ctx, `ALTER DATABASE `+database+` WITH ALLOW_CONNECTIONS false`); err != nil {
		return fmt.Errorf("pg seal template database: %w", err)
	}
	return nil
}

func migratePostgresTestTemplate(ctx context.Context, base *url.URL, database string) error {
	raw, err := sql.Open("pgx", postgresTestDatabaseDSN(base, database, postgresTestTemplateSchema))
	if err != nil {
		return fmt.Errorf("pg template open: %w", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `CREATE SCHEMA `+postgresTestTemplateSchema); err != nil {
		return fmt.Errorf("pg template schema: %w", err)
	}
	if err := migratePostgresTestSchema(ctx, raw); err != nil {
		return fmt.Errorf("pg template migrate: %w", err)
	}
	return raw.Close()
}

// postgresTestDatabaseDSN rewrites the configured URL DSN to target database
// with search_path set to schema.
func postgresTestDatabaseDSN(base *url.URL, database, schema string) string {
	target := *base
	target.Path = "/" + database
	target.RawPath = ""
	query := target.Query()
	query.Set("search_path", schema)
	target.RawQuery = query.Encode()
	return target.String()
}

// postgresTestStoreDSN returns a DSN reaching the same database and schema as
// s, for tests that open a second pool (for example under a restricted role).
func postgresTestStoreDSN(t *testing.T, s *Store) string {
	t.Helper()
	baseDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	var database, schema string
	if err := s.root.QueryRowContext(t.Context(),
		`SELECT current_database(), current_schema()`).Scan(&database, &schema); err != nil {
		t.Fatalf("pg store location: %v", err)
	}
	if base, err := url.Parse(baseDSN); err == nil && (base.Scheme == "postgres" || base.Scheme == "postgresql") {
		return postgresTestDatabaseDSN(base, database, schema)
	}
	return baseDSN + " dbname=" + database + " search_path=" + schema
}

// setupTestPGSibling provisions another migrated, seeded schema in the same
// database as s. Tests use it to prove that catalog queries filter by
// schema: setupTestDB alone may place a second store in another database.
func setupTestPGSibling(t *testing.T, s *Store) *Store {
	t.Helper()
	baseDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	_, base, template, err := postgresTestTemplateDatabase(baseDSN)
	if err != nil {
		t.Fatalf("pg test template: %v", err)
	}
	if template == "" {
		return setupTestPG(t, baseDSN) // Already one shared database.
	}
	var database string
	if err := s.root.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatalf("pg store database: %v", err)
	}
	schema := fmt.Sprintf("t_%d", testDBCounter.Add(1))
	dsn := postgresTestDatabaseDSN(base, database, schema)
	setup, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("pg sibling open: %v", err)
	}
	_, err = setup.ExecContext(t.Context(), `CREATE SCHEMA `+schema)
	if closeErr := setup.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("pg sibling schema: %v", err)
	}
	// The schema disappears with s's database; nothing else to drop.
	return openPostgresTestStore(t, dsn, true, func() error { return nil })
}

// clonePostgresTestDatabase creates a private copy number n of the template
// database and returns a DSN reaching its migrated schema. The returned drop
// function removes the copy, terminating any connection a test leaked.
func clonePostgresTestDatabase(ctx context.Context, admin *sql.DB, base *url.URL, template string, n int64) (string, func() error, error) {
	database := fmt.Sprintf("%s_%d", template, n)
	// WAL_LOG avoids the checkpoint FILE_COPY forces on every copy: on a
	// disk-backed CI server those checkpoints starve the other packages'
	// PostgreSQL tests running at the same time.
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+database+` TEMPLATE `+template+` STRATEGY WAL_LOG`); err != nil {
		return "", nil, fmt.Errorf("pg clone template database: %w", err)
	}
	drop := func() error {
		_, err := admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`)
		return err
	}
	return postgresTestDatabaseDSN(base, database, postgresTestTemplateSchema), drop, nil
}

func dropPostgresTestTemplate() error {
	tpl := &postgresTestTemplate
	if tpl.admin == nil {
		return nil
	}
	defer tpl.admin.Close()
	if tpl.database == "" {
		return nil
	}
	_, err := tpl.admin.Exec(`DROP DATABASE IF EXISTS ` + tpl.database + ` WITH (FORCE)`)
	return err
}
