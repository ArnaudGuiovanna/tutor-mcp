// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna/tutor-mcp
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed schema.sql
var schemaSQL string

func OpenDB(dbPath string) (*sql.DB, error) {
	if err := PrepareSQLitePath(dbPath); err != nil {
		return nil, err
	}

	// _txlock=immediate: every BEGIN issued by database/sql uses BEGIN
	// IMMEDIATE, which acquires the writer lock at tx start instead of
	// at first write. Required for read-modify-write patterns (see
	// applyInteraction for R008 / security-todo F-5.1) — without it,
	// two concurrent transactions both read a stale snapshot and one
	// silently overwrites the other on commit. modernc.org/sqlite
	// ignores sql.TxOptions.Isolation, so the DSN flag is the supported
	// way to control BEGIN mode (driver.go:73, conn.go:1033).
	dsn, err := sqliteFilesystemDSN(dbPath, "rwc", true,
		"journal_mode(WAL)", "busy_timeout(5000)", "foreign_keys(ON)",
	)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// SQLite is a single-writer store: even under WAL (which permits
	// concurrent readers) only one writer can hold the lock at a time.
	// database/sql's default pool is unbounded, so under load it opens
	// many connections that then serialise at the SQLite layer, turning
	// lock contention into SQLITE_BUSY errors. We pin the pool to a
	// single connection so every BEGIN IMMEDIATE (see _txlock above)
	// queues in Go rather than racing for the writer lock — busy_timeout
	// then only has to absorb the rare cross-process contention. This is
	// a single-node deployment, so serialising all access on one conn is
	// the correct, simplest choice (#5).
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// A single long-lived connection is fine for an embedded DB; recycle
	// hourly only as a hygiene measure against any per-conn state drift.
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if err := SecureSQLiteFiles(dbPath); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// alterMigrations are historical incremental schema changes. Each statement is
// recorded as its own migration in schema_migrations so future drift can be
// detected on a per-statement basis. Errors during Exec are tolerated (see
// migration.IgnoreExecErrors) because legacy databases will already have most
// of these columns from the previous "best-effort ALTER" loop.
var alterMigrations = []string{
	`ALTER TABLE learners ADD COLUMN profile_json TEXT DEFAULT '{}'`,
	`ALTER TABLE interactions ADD COLUMN error_type TEXT DEFAULT ''`,
	`ALTER TABLE interactions ADD COLUMN hints_requested INTEGER DEFAULT 0`,
	`ALTER TABLE interactions ADD COLUMN self_initiated INTEGER DEFAULT 0`,
	`ALTER TABLE interactions ADD COLUMN calibration_id TEXT DEFAULT ''`,
	`ALTER TABLE interactions ADD COLUMN is_proactive_review INTEGER DEFAULT 0`,
	`ALTER TABLE domains ADD COLUMN personal_goal TEXT DEFAULT ''`,
	`ALTER TABLE domains ADD COLUMN archived INTEGER DEFAULT 0`,
	`ALTER TABLE interactions ADD COLUMN misconception_type TEXT`,
	`ALTER TABLE interactions ADD COLUMN misconception_detail TEXT`,
	`ALTER TABLE domains ADD COLUMN value_framings_json TEXT DEFAULT ''`,
	`ALTER TABLE domains ADD COLUMN last_value_axis TEXT DEFAULT ''`,
	`ALTER TABLE oauth_codes ADD COLUMN client_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE oauth_clients ADD COLUMN client_secret_hash TEXT DEFAULT ''`,
	`ALTER TABLE domains ADD COLUMN pinned_concept TEXT DEFAULT ''`,
	// [1] GoalDecomposer — graph version + goal relevance vector storage.
	// graph_version starts at 1 for existing rows so the next add_concepts
	// makes IsGoalRelevanceStale() true vs goal_relevance_version=0.
	`ALTER TABLE domains ADD COLUMN graph_version INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE domains ADD COLUMN goal_relevance_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE domains ADD COLUMN goal_relevance_version INTEGER NOT NULL DEFAULT 0`,
	// [2] PhaseController — FSM state per domain. NULL on existing
	// rows means "pre-pipeline domain, treat as PhaseInstruction"
	// (backward-compat per OQ-2.1.b). New domains are initialised
	// to DIAGNOSTIC explicitly by tools/domain.go init_domain.
	`ALTER TABLE domains ADD COLUMN phase TEXT`,
	`ALTER TABLE domains ADD COLUMN phase_changed_at TIMESTAMP`,
	`ALTER TABLE domains ADD COLUMN phase_entry_entropy REAL`,
	// Historical chat_mode_enabled column: added then dropped after the
	// iframe surface was retired. Both statements stay in the ALTER list
	// so a fresh DB is reconciled with one that already saw the column.
	// DROP COLUMN requires SQLite >= 3.35 (modernc.org/sqlite ships above).
	`ALTER TABLE learners ADD COLUMN chat_mode_enabled INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE learners DROP COLUMN chat_mode_enabled`,
	// Issue #24: persist domain_id on interaction rows so two domains
	// sharing a concept name can be told apart in audits and downstream
	// analytics. Nullable: pre-existing rows stay NULL (origin unknown).
	`ALTER TABLE interactions ADD COLUMN domain_id TEXT`,
	// Issue #30 part 2: bind refresh_token rows to the issuing client
	// so a stolen token cannot be redeemed by a different (e.g. self-
	// registered confidential) client. Nullable: pre-existing rows
	// stay NULL and bypass the binding check (compat with v0.2 tokens
	// that were issued before the migration).
	`ALTER TABLE refresh_tokens ADD COLUMN client_id TEXT`,
	// Issue #51: log the slip/guess values that the non-canonical
	// error-type-aware heuristic (algorithms.BKTUpdateHeuristicSlipByErrorType)
	// actually fed into the BKT update on each interaction so the
	// run can be replayed deterministically. Nullable: pre-existing
	// rows have no record (NULL means "unknown / heuristic not
	// captured at write time").
	`ALTER TABLE interactions ADD COLUMN bkt_slip REAL`,
	`ALTER TABLE interactions ADD COLUMN bkt_guess REAL`,
	// Issue #55: PFA persisted state was written but never consumed —
	// engine/alert.go recomputes PFA in-memory from the interactions
	// list each call. Drop the dead columns. Forward-only: there is
	// no "down" migration. DROP COLUMN requires SQLite >= 3.35;
	// modernc.org/sqlite v1.47 ships above that.
	`ALTER TABLE concept_states DROP COLUMN pfa_successes`,
	`ALTER TABLE concept_states DROP COLUMN pfa_failures`,
	// Pin feature retired: the MCP tool that set pinned_concept
	// (pick_concept) was removed with the iframe surface, leaving the
	// column unreachable. Drop it so the schema reflects what the
	// runtime actually uses. Forward-only.
	`ALTER TABLE domains DROP COLUMN pinned_concept`,
	// Structured rubrics supplied by the LLM-side grader. Stored as
	// compact JSON strings so later pedagogical snapshots can replay the
	// scoring evidence instead of only the boolean success flag.
	`ALTER TABLE interactions ADD COLUMN rubric_json TEXT`,
	`ALTER TABLE interactions ADD COLUMN rubric_score_json TEXT`,
	// Issue #158: explicit learner-controlled domain priority. Nullable so
	// existing domains keep the historical created_at DESC routing fallback.
	`ALTER TABLE domains ADD COLUMN priority_rank INTEGER`,
}

// idempotentMigrations are CREATE TABLE/INDEX IF NOT EXISTS statements that
// were historically rerun on every startup. They are now versioned individually
// so a body change is caught as drift rather than silently re-executed.
var idempotentMigrations = []string{
	`CREATE TABLE IF NOT EXISTS oauth_codes (
				code           TEXT PRIMARY KEY,
				learner_id     TEXT NOT NULL REFERENCES learners(id),
				code_challenge TEXT NOT NULL,
				client_id      TEXT NOT NULL DEFAULT '',
				expires_at     DATETIME NOT NULL,
				created_at     DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
	`CREATE TABLE IF NOT EXISTS oauth_clients (
				client_id          TEXT PRIMARY KEY,
				client_name        TEXT DEFAULT '',
				redirect_uris      TEXT DEFAULT '[]',
				client_secret_hash TEXT DEFAULT '',
				created_at         DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
	`CREATE INDEX IF NOT EXISTS idx_concept_states_learner ON concept_states(learner_id)`,
	`CREATE INDEX IF NOT EXISTS idx_concept_states_review ON concept_states(learner_id, next_review)`,
	`CREATE INDEX IF NOT EXISTS idx_interactions_learner_created ON interactions(learner_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_interactions_learner_concept ON interactions(learner_id, concept, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_scheduled_alerts_learner_type ON scheduled_alerts(learner_id, alert_type, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_oauth_codes_expires ON oauth_codes(expires_at)`,
	`CREATE TABLE IF NOT EXISTS affect_states (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    learner_id           TEXT NOT NULL REFERENCES learners(id),
    session_id           TEXT NOT NULL,
    energy               INTEGER DEFAULT 0,
    subject_confidence   INTEGER DEFAULT 0,
    satisfaction         INTEGER DEFAULT 0,
    perceived_difficulty INTEGER DEFAULT 0,
    next_session_intent  INTEGER DEFAULT 0,
    autonomy_score       REAL DEFAULT 0,
    created_at           DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(learner_id, session_id)
)`,
	`CREATE TABLE IF NOT EXISTS calibration_records (
    prediction_id TEXT PRIMARY KEY,
    learner_id    TEXT NOT NULL REFERENCES learners(id),
    concept_id    TEXT NOT NULL,
    predicted     REAL NOT NULL,
    actual        REAL,
    delta         REAL,
    created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS transfer_records (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    learner_id   TEXT NOT NULL REFERENCES learners(id),
    concept_id   TEXT NOT NULL,
    context_type TEXT NOT NULL,
    score        REAL NOT NULL,
    session_id   TEXT DEFAULT '',
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE INDEX IF NOT EXISTS idx_affect_states_learner ON affect_states(learner_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_calibration_records_learner ON calibration_records(learner_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_transfer_records_learner_concept ON transfer_records(learner_id, concept_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_interactions_self_initiated ON interactions(learner_id, self_initiated, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_interactions_misconception ON interactions(learner_id, concept, misconception_type)`,
	`CREATE TABLE IF NOT EXISTS implementation_intentions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    learner_id     TEXT    NOT NULL REFERENCES learners(id),
    domain_id      TEXT    NOT NULL,
    trigger_text   TEXT    NOT NULL,
    action_text    TEXT    NOT NULL,
    honored        INTEGER,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    scheduled_for  DATETIME
)`,
	`CREATE TABLE IF NOT EXISTS webhook_message_queue (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    learner_id     TEXT    NOT NULL REFERENCES learners(id),
    kind           TEXT    NOT NULL,
    scheduled_for  DATETIME NOT NULL,
    expires_at     DATETIME,
    content        TEXT    NOT NULL,
    priority       INTEGER DEFAULT 0,
    status         TEXT    DEFAULT 'pending',
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sent_at        DATETIME
)`,
	`CREATE INDEX IF NOT EXISTS idx_impl_intent_learner ON implementation_intentions(learner_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_wmq_dispatch ON webhook_message_queue(learner_id, kind, status, scheduled_for)`,
	`CREATE TABLE IF NOT EXISTS pedagogical_snapshots (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    interaction_id    INTEGER NOT NULL REFERENCES interactions(id),
    learner_id        TEXT    NOT NULL REFERENCES learners(id),
    domain_id         TEXT    NOT NULL,
    concept           TEXT    NOT NULL,
    activity_type     TEXT    NOT NULL,
    before_json       TEXT    NOT NULL,
    observation_json  TEXT    NOT NULL,
    after_json        TEXT    NOT NULL,
    decision_json     TEXT    NOT NULL,
    created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE INDEX IF NOT EXISTS idx_pedagogical_snapshots_learner_created ON pedagogical_snapshots(learner_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_pedagogical_snapshots_domain_concept ON pedagogical_snapshots(learner_id, domain_id, concept, created_at)`,
	`CREATE TABLE IF NOT EXISTS webhook_push_log (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    learner_id           TEXT    NOT NULL REFERENCES learners(id),
    queue_id             INTEGER DEFAULT 0,
    kind                 TEXT    NOT NULL,
    domain_id            TEXT    DEFAULT '',
    domain_name          TEXT    DEFAULT '',
    concept              TEXT    DEFAULT '',
    trigger_text         TEXT    DEFAULT '',
    pedagogical_intent   TEXT    DEFAULT '',
    learning_gain        TEXT    DEFAULT '',
    open_loop            TEXT    DEFAULT '',
    next_action          TEXT    DEFAULT '',
    pushed_at            DATETIME NOT NULL,
    opened_session_at    DATETIME,
    concept_addressed    INTEGER DEFAULT 0,
    created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE INDEX IF NOT EXISTS idx_webhook_push_log_open ON webhook_push_log(learner_id, domain_id, concept_addressed, pushed_at)`,
	// Phase 4 distributed scheduler: per-run job lease. Each scheduled job
	// claims (name, window_key) exactly once across a fleet so only one
	// instance executes the run. Single-node (SQLite) default never claims.
	`CREATE TABLE IF NOT EXISTS scheduled_job_runs (
    name        TEXT NOT NULL,
    window_key  TEXT NOT NULL,
    claimed_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (name, window_key)
)`,
	// Opt-in shared rate-limit state (RATELIMIT_BACKEND=postgres). A single
	// fleet-wide token bucket per key replaces the independent per-process
	// bucket so throttling does not weaken with instance count. Single-node
	// (SQLite, default backend=memory) never touches this table.
	`CREATE TABLE IF NOT EXISTS rate_limit_buckets (
    bucket_key  TEXT PRIMARY KEY,
    tokens      DOUBLE PRECISION NOT NULL,
    updated_at  DATETIME NOT NULL
)`,
	// Opt-in shared login-failure log for fleet-wide per-account lockout
	// (RATELIMIT_BACKEND=postgres). One row per failed attempt; counted within
	// a sliding window. Single-node default never writes here.
	`CREATE TABLE IF NOT EXISTS login_failures (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    account_key  TEXT NOT NULL,
    attempted_at DATETIME NOT NULL
)`,
	`CREATE INDEX IF NOT EXISTS idx_login_failures_account_time ON login_failures(account_key, attempted_at)`,
}

const (
	sqliteMigrationLockTimeout = time.Minute
	sqliteMigrationRetryDelay  = 50 * time.Millisecond
)

// Migrate brings the database schema up to the version expected by this build.
//
// It runs under a single SQLite BEGIN EXCLUSIVE transaction on one reserved
// connection, so two processes starting at the same time cannot both observe a
// missing schema_migrations row and race to insert it. On first run it creates
// the schema_migrations bookkeeping table, then walks the ordered list returned
// by buildMigrations() applying any rows that are not yet recorded. On
// subsequent runs it verifies every previously applied migration's stored
// SHA-256 checksum still matches the in-source body, and returns an error
// containing "checksum mismatch" if a body has drifted — rollback is
// intentionally out of scope, drift requires manual operator intervention.
//
// The first invocation against a pre-existing database (one created before the
// schema_migrations table existed) re-executes every migration body. Each
// statement is either idempotent (CREATE TABLE/INDEX IF NOT EXISTS, the data
// UPDATEs) or is marked IgnoreExecErrors so a "duplicate column" from an ALTER
// that already ran does not abort startup; only the final INSERT into
// schema_migrations is required to succeed. Migration lock contention is
// retried for a bounded minute; callers with a lifecycle context should use
// MigrateContext.
func Migrate(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), sqliteMigrationLockTimeout)
	defer cancel()
	return MigrateContext(ctx, db)
}

// MigrateContext is Migrate with caller-controlled cancellation. SQLite's
// busy_timeout is intentionally only five seconds for ordinary OLTP calls;
// schema migration is different because another healthy process may hold the
// exclusive lock for longer under cold-start or race-detector load. Retrying
// only SQLITE_BUSY/SQLITE_LOCKED keeps startup bounded without making every
// application query wait for a minute.
func MigrateContext(ctx context.Context, db *sql.DB) (resultErr error) {
	if ctx == nil {
		return fmt.Errorf("migrate: nil context")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: reserve connection: %w", err)
	}
	defer conn.Close()
	var rebuild bool
	for {
		rebuild, err = sqliteCurriculumRebuildPending(ctx, conn)
		if err == nil {
			break
		}
		if !isSQLiteLockContention(err) {
			return fmt.Errorf("migrate: inspect pending curriculum rebuild: %w", err)
		}
		timer := time.NewTimer(sqliteMigrationRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	var foreignKeys int
	if rebuild {
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			return err
		}
		if foreignKeys != 0 {
			if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
				return err
			}
			defer func() {
				if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`); err != nil {
					resultErr = errors.Join(resultErr, fmt.Errorf("migrate: restore foreign keys: %w", err))
				}
			}()
		}
	}

	for {
		if _, err = conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err == nil {
			break
		}
		if !isSQLiteLockContention(err) {
			return fmt.Errorf("migrate: begin exclusive transaction: %w", err)
		}
		timer := time.NewTimer(sqliteMigrationRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("migrate: wait for exclusive transaction: %w", ctx.Err())
		case <-timer.C:
		}
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	if err := ensureSchemaMigrationsTableInTx(ctx, conn); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for _, m := range buildMigrations() {
		if err := applyMigrationInTx(ctx, conn, m); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if rebuild && foreignKeys != 0 {
		if err := checkSQLiteForeignKeys(ctx, conn); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("migrate: commit exclusive transaction: %w", err)
	}
	committed = true
	return nil
}

func isSQLiteLockContention(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	primaryCode := sqliteErr.Code() & 0xff
	return primaryCode == sqlite3.SQLITE_BUSY || primaryCode == sqlite3.SQLITE_LOCKED
}
