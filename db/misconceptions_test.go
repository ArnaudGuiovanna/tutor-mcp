package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testDBCounter makes migration-test DSNs and PostgreSQL schemas unique.
// Atomic because conformance subtests can provision databases concurrently.
var testDBCounter atomic.Int64

var sqliteTestDBTemplate struct {
	sync.Once
	data []byte
	err  error
}

// rb rebinds a '?'-placeholder query string for the store's dialect, so raw
// store.root.Exec/Query/QueryRow calls in tests work on both SQLite and
// Postgres. (Tests are in-package, so they can reach the store's rebind.)
func rb(s *Store, q string) string { return s.rebind(q) }

// seedLearner inserts a learner row (idempotent). Postgres enforces the
// learner_id foreign keys that SQLite leaves unenforced, so tests that create
// child rows for an ad-hoc learner must seed the learner first to exercise the
// same code path on both dialects.
func seedLearner(t *testing.T, s *Store, id string) {
	t.Helper()
	_, err := s.root.Exec(
		rb(s, `INSERT INTO learners (id, email, password_hash, objective, created_at)
		 VALUES (?, ?, 'h', 'o', ?) ON CONFLICT (id) DO NOTHING`),
		id, id+"@test.com", time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("seed learner %q: %v", id, err)
	}
	if err := s.EnsureRecoveryEnrollment(context.Background(), id); err != nil {
		t.Fatalf("seed recovery enrollment %q: %v", id, err)
	}
}

// setupTestDB returns a fresh, migrated Store seeded with learner 'L1'. By
// default it copies a closed SQLite template so each test remains isolated
// without replaying every migration under the race detector. Dedicated
// migration tests still build their own databases. If TUTOR_TEST_PG_DSN is
// set, this instead provisions an isolated PostgreSQL schema and returns a
// Postgres-dialect Store for cross-dialect equivalence.
func setupTestDB(t *testing.T) *Store {
	t.Helper()
	if pgDSN := os.Getenv("TUTOR_TEST_PG_DSN"); pgDSN != "" {
		return setupTestPG(t, pgDSN)
	}
	template, err := sqliteTestDBTemplateBytes()
	if err != nil {
		t.Fatalf("build sqlite test database template: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "db-test.db")
	if err := os.WriteFile(dbPath, template, 0o600); err != nil {
		t.Fatalf("copy sqlite test database template: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite test database: %v", err)
	}
	// Mirror production OpenDB: a single connection serializes writers so
	// concurrent BEGIN IMMEDIATE transactions queue instead of deadlocking.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		db.Close()
		t.Fatalf("enable SQLite foreign keys: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func sqliteTestDBTemplateBytes() ([]byte, error) {
	sqliteTestDBTemplate.Do(func() {
		dir, err := os.MkdirTemp("", "tutor-mcp-db-test-template-")
		if err != nil {
			sqliteTestDBTemplate.err = err
			return
		}
		defer os.RemoveAll(dir)

		path := filepath.Join(dir, "template.db")
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			sqliteTestDBTemplate.err = err
			return
		}
		if err := Migrate(raw); err != nil {
			_ = raw.Close()
			sqliteTestDBTemplate.err = err
			return
		}
		now := time.Now().UTC()
		if _, err := raw.Exec(`INSERT INTO learners (id, email, password_hash, objective, created_at, email_verified_at)
			VALUES ('L1', 'test@test.com', 'hash', 'test', ?, ?)`, now, now); err != nil {
			_ = raw.Close()
			sqliteTestDBTemplate.err = err
			return
		}
		if err := NewStore(raw).EnsureRecoveryEnrollment(context.Background(), "L1"); err != nil {
			_ = raw.Close()
			sqliteTestDBTemplate.err = err
			return
		}
		if err := raw.Close(); err != nil {
			sqliteTestDBTemplate.err = err
			return
		}
		sqliteTestDBTemplate.data, sqliteTestDBTemplate.err = os.ReadFile(path)
	})
	return sqliteTestDBTemplate.data, sqliteTestDBTemplate.err
}

// setupTestPG provisions a uniquely-named Postgres schema, migrates it, seeds
// learner 'L1', and returns a Postgres-dialect Store scoped to that schema via
// search_path. The schema is dropped on cleanup. The schema name is digits-only
// (t_<n>) so it is always a valid, injection-safe identifier.
func setupTestPG(t *testing.T, baseDSN string) *Store {
	t.Helper()
	n := testDBCounter.Add(1)
	schema := fmt.Sprintf("t_%d", n)

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("pg admin open: %v", err)
	}
	if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
		admin.Close()
		t.Fatalf("pg drop schema: %v", err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		admin.Close()
		t.Fatalf("pg create schema: %v", err)
	}

	sep := "?"
	if strings.Contains(baseDSN, "?") {
		sep = "&"
	}
	db, err := sql.Open("pgx", baseDSN+sep+"search_path="+schema)
	if err != nil {
		admin.Close()
		t.Fatalf("pg open: %v", err)
	}
	// Match OpenPostgres rather than database/sql's unbounded default. This
	// makes the contention tests exercise queueing and connection reuse without
	// exhausting PostgreSQL's global max_connections setting.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := migratePostgresTestSchema(context.Background(), db); err != nil {
		db.Close()
		admin.Close()
		t.Fatalf("pg migrate: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO learners (id, email, password_hash, objective, created_at, email_verified_at) VALUES ('L1', 'test@test.com', 'hash', 'test', $1, $1)`, now); err != nil {
		db.Close()
		admin.Close()
		t.Fatalf("pg seed L1: %v", err)
	}
	store := NewStoreWithDialect(db, DialectPostgres)
	if err := store.EnsureRecoveryEnrollment(context.Background(), "L1"); err != nil {
		db.Close()
		admin.Close()
		t.Fatalf("pg seed L1 recovery enrollment: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		admin.Close()
	})
	return store
}

// migratePostgresTestSchema materializes the exact current PostgreSQL schema
// and checksum ledger in one transaction. setupTestDB calls this for every
// isolated business-test schema; replaying the production migrator's advisory
// lock, lookup and transaction boundaries hundreds of times made the race CI
// spend most of its budget provisioning fixtures. Dedicated migration tests
// still call MigratePostgres and therefore retain coverage of those boundaries.
func migratePostgresTestSchema(ctx context.Context, database *sql.DB) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range splitSQLStatements(postgresSchema) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply PostgreSQL test base schema: %w", err)
		}
	}
	baseHash := sha256.Sum256([]byte(postgresSchema))
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
		"postgres_schema", hex.EncodeToString(baseHash[:]),
	); err != nil {
		return fmt.Errorf("record PostgreSQL test base schema: %w", err)
	}
	for _, migration := range postgresMigrations {
		for _, statement := range splitSQLStatements(migration.Body) {
			if _, err := tx.ExecContext(ctx, statement); err != nil && !migration.IgnoreExecErrors {
				return fmt.Errorf("apply PostgreSQL test migration %s: %w", migration.Version, err)
			}
		}
		hash := sha256.Sum256([]byte(migration.Body))
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
			migration.Version, hex.EncodeToString(hash[:]),
		); err != nil {
			return fmt.Errorf("record PostgreSQL test migration %s: %w", migration.Version, err)
		}
	}
	return tx.Commit()
}

func insertInteraction(t *testing.T, store *Store, concept string, success bool, miscType, miscDetail string, createdAt time.Time) {
	t.Helper()
	succInt := 0
	if success {
		succInt = 1
	}
	_, err := store.root.Exec(
		rb(store, `INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, notes, misconception_type, misconception_detail, created_at)
		 VALUES ('L1', ?, 'RECALL_EXERCISE', ?, 60, 0.5, '', ?, ?, ?)`),
		concept, succInt, nullString(miscType), nullString(miscDetail), createdAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// TestGetMisconceptionGroups_Basic inserts 2 "confusion goroutine/thread" and
// 1 "missing sync" misconception on "Goroutines", then verifies group counts
// and last_error_detail.
func TestGetMisconceptionGroups_Basic(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "thought goroutines are OS threads", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "mixed up scheduler", now.Add(-1*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "missing sync", "forgot to use WaitGroup", now.Add(-2*time.Hour))

	groups, err := store.GetMisconceptionGroups(context.Background(), "L1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// First group should be "confusion goroutine/thread" with count=2 (ordered by count DESC)
	if groups[0].MisconceptionType != "confusion goroutine/thread" {
		t.Errorf("expected first group type 'confusion goroutine/thread', got %q", groups[0].MisconceptionType)
	}
	if groups[0].Count != 2 {
		t.Errorf("expected first group count=2, got %d", groups[0].Count)
	}
	if groups[0].LastErrorDetail != "mixed up scheduler" {
		t.Errorf("expected last_error_detail 'mixed up scheduler', got %q", groups[0].LastErrorDetail)
	}

	// Second group should be "missing sync" with count=1
	if groups[1].MisconceptionType != "missing sync" {
		t.Errorf("expected second group type 'missing sync', got %q", groups[1].MisconceptionType)
	}
	if groups[1].Count != 1 {
		t.Errorf("expected second group count=1, got %d", groups[1].Count)
	}
}

// TestMisconceptionStatus_Active verifies that a misconception appearing in the
// last 3 interactions is reported as "active".
func TestMisconceptionStatus_Active(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	// Chronological: fail, success, fail (same misconception)
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "detail1", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-2*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "detail2", now.Add(-1*time.Hour))

	groups, err := store.GetMisconceptionGroups(context.Background(), "L1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Status != "active" {
		t.Errorf("expected status 'active', got %q", groups[0].Status)
	}
}

// TestMisconceptionStatus_Resolved verifies that a misconception NOT appearing
// in the last 3 interactions is reported as "resolved".
func TestMisconceptionStatus_Resolved(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	// Old fail, then 3 successes
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "old detail", now.Add(-4*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-2*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-1*time.Hour))

	groups, err := store.GetMisconceptionGroups(context.Background(), "L1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Status != "resolved" {
		t.Errorf("expected status 'resolved', got %q", groups[0].Status)
	}
}

// TestGetActiveMisconceptions inserts an active misconception on "Goroutines"
// and a resolved one on "Interfaces", then verifies that GetActiveMisconceptions
// for "Goroutines" returns only the active one.
func TestGetActiveMisconceptions(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	// Active misconception on Goroutines (recent fail)
	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "recent", now.Add(-1*time.Hour))

	// Resolved misconception on Interfaces (old fail + 3 successes)
	insertInteraction(t, store, "Interfaces", false, "type assertion error", "old", now.Add(-5*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-4*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-2*time.Hour))

	active, err := store.GetActiveMisconceptions(context.Background(), "L1", "Goroutines")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active misconception, got %d", len(active))
	}
	if active[0].MisconceptionType != "confusion goroutine/thread" {
		t.Errorf("expected type 'confusion goroutine/thread', got %q", active[0].MisconceptionType)
	}
	if active[0].Status != "active" {
		t.Errorf("expected status 'active', got %q", active[0].Status)
	}
}

// TestGetDistinctMisconceptionTypes inserts 2 different misconception types and
// 1 success (no misconception), then verifies exactly 2 types are returned.
func TestGetDistinctMisconceptionTypes(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "d1", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "missing sync", "d2", now.Add(-2*time.Hour))
	insertInteraction(t, store, "Goroutines", true, "", "", now.Add(-1*time.Hour)) // no misconception

	types, err := store.GetDistinctMisconceptionTypes(context.Background(), "L1", "Goroutines")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(types))
	}

	// Alphabetical order
	if types[0] != "confusion goroutine/thread" {
		t.Errorf("expected first type 'confusion goroutine/thread', got %q", types[0])
	}
	if types[1] != "missing sync" {
		t.Errorf("expected second type 'missing sync', got %q", types[1])
	}
}

// TestConceptFilter inserts misconceptions on 2 different concepts and verifies
// that filtering to 1 concept returns only that concept's groups.
func TestConceptFilter(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now()

	insertInteraction(t, store, "Goroutines", false, "confusion goroutine/thread", "d1", now.Add(-2*time.Hour))
	insertInteraction(t, store, "Interfaces", false, "type assertion error", "d2", now.Add(-1*time.Hour))

	filter := map[string]bool{"Goroutines": true}
	groups, err := store.GetMisconceptionGroups(context.Background(), "L1", filter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Concept != "Goroutines" {
		t.Errorf("expected concept 'Goroutines', got %q", groups[0].Concept)
	}
}
