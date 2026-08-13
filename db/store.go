// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
	"tutor-mcp/store"
	"tutor-mcp/webhookurl"
)

// Compile-time proof that *Store satisfies the persistence port. If a db method
// signature drifts from store.Store (or the interface was mis-transcribed), this
// line fails to build — caught here, never at runtime.
var _ store.Store = (*Store)(nil)

// sqlExecutor is implemented by both *sql.DB and *sql.Tx, so Store methods run
// transparently on a connection or inside a WithTx transaction.
type sqlExecutor interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Dialect selects the SQL flavor a Store targets. SQLite is the default; the
// Postgres dialect rebinds '?' placeholders to $N and uses RETURNING for
// auto-increment inserts.
type Dialect int

const (
	DialectSQLite Dialect = iota
	DialectPostgres
)

type Store struct {
	db                      sqlExecutor // *sql.DB normally; *sql.Tx inside WithTx — query methods use this unchanged
	root                    *sql.DB     // the real pool. For WithTx/Ping/Close/Migrate + internal BeginTx. nil in a tx-scoped Store.
	dialect                 Dialect     // SQL flavor; propagated into tx-scoped Stores by WithTx.
	secretKeyring           *IntegrationSecretKeyring
	tenantScope             *models.TenantScope
	integrationAllowedHosts map[string]struct{}
}

type tenantTransactionContext struct {
	root  *sql.DB
	tx    *sql.Tx
	scope models.TenantScope
}

type tenantTransactionContextKey struct{}

// RawDB returns the underlying *sql.DB. Intended for tests that need
// to insert with explicit timestamps (e.g. simulating older
// interactions for diagnostic-window assertions). Not for use in
// production code paths — use the typed Store methods instead.
func (s *Store) RawDB() *sql.DB { return s.root }

// VerifySchemaCurrent is the live readiness compatibility gate. It is
// read-only and intentionally accepts additive N+1 ledger entries while
// requiring every migration known by this binary to retain its checksum.
func (s *Store) VerifySchemaCurrent(ctx context.Context) error {
	if s == nil || s.root == nil {
		return fmt.Errorf("schema compatibility: root database is unavailable")
	}
	if s.dialect == DialectPostgres {
		return VerifyPostgresSchemaCurrent(ctx, s.root)
	}
	return verifyMigrationLedger(ctx, s.root, buildMigrations())
}

func NewStore(database *sql.DB) *Store {
	return &Store{db: database, root: database, dialect: DialectSQLite}
}

// NewStoreWithDialect builds a Store targeting the given SQL dialect.
func NewStoreWithDialect(database *sql.DB, d Dialect) *Store {
	return &Store{db: database, root: database, dialect: d}
}

// Ping verifies a live connection to the database.
func (s *Store) Ping(ctx context.Context) error { return s.root.PingContext(ctx) }

// Close closes the underlying connection pool.
func (s *Store) Close() error { return s.root.Close() }

// Migrate runs the schema migrations. (Wraps the package-level Migrate.)
func (s *Store) Migrate(ctx context.Context) error { return MigrateContext(ctx, s.root) }

// rebind converts '?' placeholders to PostgreSQL's $1,$2,... when the dialect
// is Postgres. For SQLite it returns the query unchanged. (Queries never embed
// a literal '?' in string literals, so a positional rewrite is safe.)
func (s *Store) rebind(query string) string {
	if s.dialect != DialectPostgres {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.executor(ctx).ExecContext(ctx, s.rebind(query), args...)
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.executor(ctx).QueryContext(ctx, s.rebind(query), args...)
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.executor(ctx).QueryRowContext(ctx, s.rebind(query), args...)
}

func (s *Store) executor(ctx context.Context) sqlExecutor {
	if scoped, ok := ctx.Value(tenantTransactionContextKey{}).(*tenantTransactionContext); ok &&
		s.root != nil && scoped.root == s.root {
		return scoped.tx
	}
	return s.db
}

// insertReturningID runs an INSERT and returns the new row's integer id. For
// SQLite it uses LastInsertId; for Postgres it appends "RETURNING id" and scans.
// The supplied query MUST NOT already contain a RETURNING clause.
func (s *Store) insertReturningID(ctx context.Context, query string, args ...any) (int64, error) {
	if s.dialect == DialectPostgres {
		var id int64
		err := s.queryRow(ctx, query+" RETURNING id", args...).Scan(&id)
		return id, err
	}
	res, err := s.exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// utcDateExpr returns a dialect-specific SQL expression that renders the given
// timestamp column as a 'YYYY-MM-DD' UTC calendar-date string. SQLite stores
// timestamps as ISO-8601 text whose first 10 chars are the date, so substr is
// both correct and avoids modernc-sqlite's DATE() not parsing the 'T' separator.
// On Postgres the column is TIMESTAMPTZ, so substr is invalid; to_char on the
// value converted to UTC yields the same calendar-date text. Both branches
// produce identical strings for the consecutive-day streak/session logic.
func (s *Store) utcDateExpr(col string) string {
	if s.dialect == DialectPostgres {
		return "to_char(" + col + " AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
	}
	return "substr(" + col + ", 1, 10)"
}

// flexTime is a dialect-neutral timestamp scan target. Direct column scans on
// modernc.org/sqlite (and pgx) yield a time.Time, but aggregate result columns
// such as MIN(created_at)/MAX(created_at) lose type affinity on SQLite and come
// back as a TEXT string. flexTime accepts time.Time, []byte, or string (parsing
// the latter two with the formats SQLite stores) so the same query works on
// both dialects. Valid is false for a SQL NULL.
type flexTime struct {
	Time  time.Time
	Valid bool
}

var flexTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999 +0000 UTC",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
}

func (f *flexTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		f.Time, f.Valid = time.Time{}, false
		return nil
	case time.Time:
		f.Time, f.Valid = v, true
		return nil
	case []byte:
		return f.parse(string(v))
	case string:
		return f.parse(v)
	default:
		return fmt.Errorf("flexTime: unsupported scan type %T", src)
	}
}

func (f *flexTime) parse(s string) error {
	for _, layout := range flexTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			f.Time, f.Valid = t, true
			return nil
		}
	}
	// Unparseable timestamp (e.g. a Go time.Time String() carrying a monotonic
	// "m=..." suffix written by a raw test insert). Match the previous
	// behavior, which discarded the parse error and left a zero time, so a
	// scan never fails on a malformed stored value.
	f.Time, f.Valid = time.Time{}, false
	return nil
}

// WithTx runs fn inside a single serializable transaction (BEGIN IMMEDIATE on
// SQLite). The Store passed to fn routes every query through the tx.
//
// Serializable isolation translates to BEGIN IMMEDIATE on modernc.org/sqlite,
// which acquires the writer lock at tx start instead of at first write. Required
// for read-modify-write patterns where concurrent goroutines would otherwise
// both read a stale snapshot and one overwrite the other on commit (R008).
func (s *Store) WithTx(ctx context.Context, fn func(s store.Store) error) error {
	if scoped, ok := ctx.Value(tenantTransactionContextKey{}).(*tenantTransactionContext); ok &&
		s.root != nil && scoped.root == s.root {
		copyScope := scoped.scope
		return fn(&Store{db: scoped.tx, root: nil, dialect: s.dialect, secretKeyring: s.secretKeyring, tenantScope: &copyScope})
	}
	// Isolation differs by dialect on purpose:
	//   SQLite   — SERIALIZABLE maps to BEGIN IMMEDIATE (DSN _txlock=immediate):
	//              the single writer lock is taken at tx start, so concurrent
	//              writers queue on busy_timeout rather than racing.
	//   Postgres — READ COMMITTED (the default). Row-level exclusivity is
	//              provided explicitly by SELECT ... FOR UPDATE [SKIP LOCKED],
	//              which blocks rather than aborting. Using SERIALIZABLE here
	//              would surface 40001 serialization failures that callers would
	//              have to retry; READ COMMITTED + FOR UPDATE gives the same
	//              lost-update protection without retries.
	iso := sql.LevelSerializable
	if s.dialect == DialectPostgres {
		iso = sql.LevelReadCommitted
	}
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: iso})
	if err != nil {
		return err
	}
	// Rollback is intentionally deferred immediately after BeginTx so the
	// connection and any locks are released even when fn panics. Rollback is
	// harmless after a successful Commit (it returns sql.ErrTxDone).
	defer func() { _ = tx.Rollback() }()
	if err := fn(&Store{db: tx, root: nil, dialect: s.dialect, secretKeyring: s.secretKeyring, tenantScope: s.tenantScope}); err != nil {
		return err
	}
	return tx.Commit()
}

// WithTenantTx binds one validated tenant principal to one database
// transaction. PostgreSQL settings are transaction-local, so commit, rollback,
// cancellation and connection-pool reuse cannot leak a tenant to the next
// request. SQLite receives the same typed scope for compatibility tests.
func (s *Store) WithTenantTx(ctx context.Context, scope models.TenantScope, fn func(context.Context, store.Store) error) error {
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("tenant transaction: %w", err)
	}
	if scoped, ok := ctx.Value(tenantTransactionContextKey{}).(*tenantTransactionContext); ok {
		if s.root == nil || scoped.root != s.root || scoped.scope != scope {
			return fmt.Errorf("tenant transaction: nested scope mismatch")
		}
		copyScope := scope
		return fn(ctx, &Store{db: scoped.tx, root: nil, dialect: s.dialect, secretKeyring: s.secretKeyring, tenantScope: &copyScope})
	}
	if s.root == nil {
		if s.tenantScope == nil || *s.tenantScope != scope {
			return fmt.Errorf("tenant transaction: nested scope mismatch")
		}
		return fn(ctx, s)
	}
	iso := sql.LevelSerializable
	if s.dialect == DialectPostgres {
		iso = sql.LevelReadCommitted
	}
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: iso})
	if err != nil {
		return fmt.Errorf("tenant transaction: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == DialectPostgres {
		for setting, value := range map[string]string{
			"app.current_tenant":     scope.TenantID,
			"app.current_user":       scope.UserID,
			"app.current_membership": scope.MembershipID,
		} {
			if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, setting, value); err != nil {
				return fmt.Errorf("tenant transaction: set %s: %w", setting, err)
			}
		}
	}
	copyScope := scope
	txStore := &Store{db: tx, root: nil, dialect: s.dialect, secretKeyring: s.secretKeyring, tenantScope: &copyScope}
	txCtx := context.WithValue(ctx, tenantTransactionContextKey{}, &tenantTransactionContext{root: s.root, tx: tx, scope: scope})
	if err := fn(txCtx, txStore); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("tenant transaction: commit: %w", err)
	}
	return nil
}

// inTx runs fn against a transaction-scoped Store. When s is the root store it
// opens a new transaction with the given options, commits on success and rolls
// back on error. When s is ALREADY transaction-scoped (inside a WithTx
// callback, root == nil), it runs fn on the current transaction so the
// multi-statement methods below (DeleteDomain, ConsumeAuthCode, …) compose
// inside WithTx instead of dereferencing a nil root. In that nested case the
// outer WithTx owns commit/rollback, so inTx neither commits nor rolls back.
func (s *Store) inTx(ctx context.Context, opts *sql.TxOptions, fn func(txs *Store) error) error {
	if scoped, ok := ctx.Value(tenantTransactionContextKey{}).(*tenantTransactionContext); ok &&
		s.root != nil && scoped.root == s.root {
		copyScope := scoped.scope
		return fn(&Store{db: scoped.tx, root: nil, dialect: s.dialect, secretKeyring: s.secretKeyring, tenantScope: &copyScope})
	}
	if s.root == nil {
		return fn(s)
	}
	tx, err := s.root.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	// Keep the transaction cleanup panic-safe for every internal transaction,
	// just as WithTx does for caller-provided callbacks.
	defer func() { _ = tx.Rollback() }()
	txs := &Store{db: tx, dialect: s.dialect, secretKeyring: s.secretKeyring, tenantScope: s.tenantScope}
	if err := fn(txs); err != nil {
		return err
	}
	return tx.Commit()
}

// GetConceptStateForUpdate reads the concept state with a row lock so a
// concurrent read-modify-write on the same (learner, concept) serializes.
// On Postgres it emits SELECT ... FOR UPDATE (must run inside WithTx). On
// SQLite the lock is implicit: WithTx opens BEGIN IMMEDIATE, which already
// holds the single writer lock for the whole transaction.
func (s *Store) GetConceptStateForUpdate(ctx context.Context, learnerID, concept string) (*models.ConceptState, error) {
	return s.getConceptState(ctx, learnerID, "", concept, true, false)
}

func (s *Store) GetConceptStateForUpdateInDomain(ctx context.Context, learnerID, domainID, concept string) (*models.ConceptState, error) {
	return s.getConceptState(ctx, learnerID, domainID, concept, true, true)
}

// GetOrCreateConceptStateForUpdate returns the row-locked concept state for
// (learner, concept), materializing a default row first when none exists. Must
// run inside WithTx.
//
// The materialize-then-lock order is load-bearing for first-touch concurrency
// on Postgres: a bare SELECT ... FOR UPDATE locks nothing when the row is
// absent, so two concurrent first interactions would both read no row, both
// bootstrap a fresh state, and the second UpsertConceptState would overwrite
// the first — silently discarding an entire BKT/FSRS/IRT update. Inserting with
// ON CONFLICT DO NOTHING first guarantees the row exists, so the subsequent
// FOR UPDATE serializes the read-modify-write. SQLite is already safe via
// BEGIN IMMEDIATE; the extra insert is a cheap no-op once the row exists.
func (s *Store) GetOrCreateConceptStateForUpdate(ctx context.Context, learnerID, concept string) (*models.ConceptState, error) {
	cs, err := s.getConceptState(ctx, learnerID, "", concept, true, false)
	if err == nil {
		return cs, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	newState := models.NewConceptState(learnerID, concept)
	if err := s.InsertConceptStateIfNotExists(ctx, newState); err != nil {
		return nil, fmt.Errorf("bootstrap concept state: %w", err)
	}
	if newState.DomainID != "" {
		return s.getConceptState(ctx, learnerID, newState.DomainID, concept, true, true)
	}
	return s.getConceptState(ctx, learnerID, "", concept, true, true)
}

// GetOrCreateConceptStateForUpdateInDomain is the production first-touch path.
// The materialize-then-lock sequence serializes concurrent updates on the
// composite (learner, domain, concept) identity under PostgreSQL.
func (s *Store) GetOrCreateConceptStateForUpdateInDomain(ctx context.Context, learnerID, domainID, concept string) (*models.ConceptState, error) {
	cs, err := s.getConceptState(ctx, learnerID, domainID, concept, true, true)
	if err == nil {
		return cs, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := s.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(learnerID, domainID, concept)); err != nil {
		return nil, fmt.Errorf("bootstrap domain concept state: %w", err)
	}
	return s.getConceptState(ctx, learnerID, domainID, concept, true, true)
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullFloat64 returns nil when the input pointer is nil and the dereferenced
// value otherwise. Used to write columns that distinguish "absent" from
// "literal zero" (e.g. interactions.bkt_slip / bkt_guess on pre-issue-#51
// rows).
func nullFloat64(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ─── Learners ────────────────────────────────────────────────────────────────

func (s *Store) CreateLearner(ctx context.Context, email, passwordHash, objective, webhookURL string) (*models.Learner, error) {
	now := time.Now().UTC()
	return s.createLearner(ctx, email, passwordHash, objective, webhookURL, &now)
}

func (s *Store) CreateUnverifiedLearner(ctx context.Context, email, passwordHash, objective, webhookURL string) (*models.Learner, error) {
	return s.createLearner(ctx, email, passwordHash, objective, webhookURL, nil)
}

func (s *Store) createLearner(ctx context.Context, email, passwordHash, objective, webhookURL string, verifiedAt *time.Time) (*models.Learner, error) {
	if webhookURL != "" && !webhookurl.IsSafeWebhookURL(webhookURL) {
		return nil, fmt.Errorf("invalid webhook_url: must use an allowed Discord HTTPS endpoint")
	}
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	tenantScope := models.LegacyPrincipal(id).TenantScope()
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var learner *models.Learner
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, _ store.Store) error {
			var innerErr error
			learner, innerErr = s.createLearnerWithID(txCtx, id, email, passwordHash, objective, webhookURL, verifiedAt)
			return innerErr
		})
		return learner, err
	}
	return s.createLearnerWithID(ctx, id, email, passwordHash, objective, webhookURL, verifiedAt)
}

func (s *Store) createLearnerWithID(ctx context.Context, id, email, passwordHash, objective, webhookURL string, verifiedAt *time.Time) (*models.Learner, error) {
	now := time.Now().UTC()
	storedWebhookURL, err := s.encryptIntegrationSecret(id, webhookURL)
	if err != nil {
		return nil, fmt.Errorf("encrypt webhook URL: %w", err)
	}
	_, err = s.exec(ctx,
		`INSERT INTO learners
		    (id, email, password_hash, objective, webhook_url, created_at, email_verified_at,
		     tenant_id, user_id, membership_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, email, passwordHash, objective, storedWebhookURL, now, verifiedAt,
		models.LegacyTenantID, id, "membership_legacy_"+id,
	)
	if err != nil {
		return nil, fmt.Errorf("create learner: %w", err)
	}
	if err := s.ensureRecoveryEnrollment(ctx, id); err != nil {
		return nil, fmt.Errorf("provision learner recovery enrollment: %w", err)
	}
	return &models.Learner{
		ID:              id,
		Email:           email,
		PasswordHash:    passwordHash,
		Objective:       objective,
		CreatedAt:       now,
		EmailVerifiedAt: verifiedAt,
	}, nil
}

func (s *Store) GetLearnerByID(ctx context.Context, id string) (*models.Learner, error) {
	row := s.queryRow(ctx,
		`SELECT id, email, password_hash, objective, profile_json, created_at, last_active, email_verified_at
		 FROM learners WHERE id = ?`, id,
	)
	return s.scanLearner(row)
}

func (s *Store) GetLearnerByEmail(ctx context.Context, email string) (*models.Learner, error) {
	row := s.queryRow(ctx,
		`SELECT id, email, password_hash, objective, profile_json, created_at, last_active, email_verified_at
		 FROM learners WHERE email = ?`, email,
	)
	return s.scanLearner(row)
}

func (s *Store) scanLearner(row *sql.Row) (*models.Learner, error) {
	l := &models.Learner{}
	var lastActive sql.NullTime
	var emailVerifiedAt sql.NullTime
	var profileJSON sql.NullString
	err := row.Scan(
		&l.ID, &l.Email, &l.PasswordHash, &l.Objective, &profileJSON, &l.CreatedAt, &lastActive, &emailVerifiedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan learner: %w", err)
	}
	if lastActive.Valid {
		l.LastActive = lastActive.Time
	}
	if emailVerifiedAt.Valid {
		ts := emailVerifiedAt.Time
		l.EmailVerifiedAt = &ts
	}
	if profileJSON.Valid {
		l.ProfileJSON = profileJSON.String
	} else {
		l.ProfileJSON = "{}"
	}
	return l, nil
}

func (s *Store) UpdateLastActive(ctx context.Context, id string) error {
	_, err := s.exec(ctx,
		`UPDATE learners SET last_active = ? WHERE id = ?`,
		time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update last active: %w", err)
	}
	return nil
}

// ListWebhookDispatchTargetsPage is the bounded scheduler read model. Keyset
// pagination keeps latency independent of skipped rows; the projection omits
// credentials and PII and joins availability so dispatch does not issue an
// additional policy query per learner. The encrypted credential is used only
// as an existence filter and is never selected into this read model.
func (s *Store) ListWebhookDispatchTargetsPage(ctx context.Context, afterLearnerID string, limit int) ([]models.WebhookDispatchTarget, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("list webhook dispatch targets: limit must be between 1 and 1000")
	}
	rows, err := s.query(ctx,
		`SELECT l.id,
		        COALESCE(a.timezone, 'UTC'), COALESCE(a.windows_json, '[]'),
		        COALESCE(a.avg_duration, 30), COALESCE(a.sessions_week, 3),
		        COALESCE(a.do_not_disturb, 0), COALESCE(a.notification_consent, 0),
		        COALESCE(a.notification_frequency, 'daily'),
		        COALESCE(a.max_notifications_per_day, 1),
		        COALESCE(a.accessibility_json, '{}'), COALESCE(a.version, 1),
		        a.updated_at
		 FROM learners l
		 LEFT JOIN availability a ON a.learner_id = l.id
		 WHERE l.webhook_url <> '' AND l.email_verified_at IS NOT NULL AND l.id > ?
		 ORDER BY l.id
		 LIMIT ?`, afterLearnerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list webhook dispatch targets: %w", err)
	}
	defer rows.Close()
	targets := make([]models.WebhookDispatchTarget, 0, limit)
	for rows.Next() {
		var learnerID string
		availability := &models.Availability{}
		var dnd, consent int
		var updatedAt sql.NullTime
		if err := rows.Scan(
			&learnerID,
			&availability.Timezone, &availability.WindowsJSON,
			&availability.AvgDuration, &availability.SessionsWeek,
			&dnd, &consent, &availability.NotificationFrequency,
			&availability.MaxNotificationsPerDay, &availability.AccessibilityJSON,
			&availability.Version, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan webhook dispatch target: %w", err)
		}
		availability.LearnerID = learnerID
		availability.DoNotDisturb = dnd != 0
		availability.NotificationConsent = consent != 0
		if updatedAt.Valid {
			availability.UpdatedAt = updatedAt.Time
		}
		targets = append(targets, models.WebhookDispatchTarget{LearnerID: learnerID, Availability: availability})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webhook dispatch targets: %w", err)
	}
	return targets, nil
}

// GetWebhookDispatchURL is the only runtime credential read. Callers invoke it
// after payload creation and durable claim revalidation, immediately before
// entering the HTTP delivery boundary.
func (s *Store) GetWebhookDispatchURL(ctx context.Context, learnerID string) (string, error) {
	var stored string
	err := s.queryRow(ctx,
		`SELECT webhook_url FROM learners
		 WHERE id = ? AND email_verified_at IS NOT NULL AND webhook_url <> ''`,
		learnerID,
	).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("webhook dispatch credential is unavailable")
	}
	if err != nil {
		return "", fmt.Errorf("load webhook dispatch credential: %w", err)
	}
	plaintext, err := s.decryptIntegrationSecret(learnerID, stored)
	if err != nil {
		return "", fmt.Errorf("decrypt webhook dispatch credential: %w", err)
	}
	return plaintext, nil
}

func (s *Store) UpdateLearnerProfile(ctx context.Context, learnerID, profileJSON string, objective *string) error {
	query := `UPDATE learners SET profile_json = ? WHERE id = ?`
	args := []any{profileJSON, learnerID}
	if objective != nil {
		query = `UPDATE learners SET profile_json = ?, objective = ? WHERE id = ?`
		args = []any{profileJSON, *objective, learnerID}
	}
	result, err := s.exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update learner profile: %w", err)
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ─── Refresh Tokens ───────────────────────────────────────────────────────────

const refreshTokenHashPrefix = "sha256:"

func newRefreshToken(learnerID, clientID, resource, scope, familyID string) (*models.RefreshToken, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	if familyID == "" {
		var err error
		familyID, err = generateID()
		if err != nil {
			return nil, fmt.Errorf("generate refresh token family: %w", err)
		}
	}
	now := time.Now().UTC()
	return &models.RefreshToken{
		Token:     base64.RawURLEncoding.EncodeToString(b),
		LearnerID: learnerID,
		ClientID:  clientID,
		Resource:  resource,
		Scope:     scope,
		FamilyID:  familyID,
		ExpiresAt: now.Add(30 * 24 * time.Hour),
		CreatedAt: now,
	}, nil
}

// refreshTokenHash is the only database credential accepted for redemption.
// Pre-hash plaintext rows are revoked by migration and never participate in
// the active lookup path.
func refreshTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return refreshTokenHashPrefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

const refreshTokenInspectionPredicate = `(token = ? OR (token = ? AND token NOT LIKE 'sha256:%'))`

func (s *Store) insertRefreshToken(ctx context.Context, rt *models.RefreshToken) error {
	if strings.TrimSpace(rt.ClientID) == "" || strings.TrimSpace(rt.Resource) == "" {
		return fmt.Errorf("refresh token client_id and resource are required")
	}
	canonicalScope, err := models.CanonicalOAuthScope(rt.Scope)
	if err != nil || canonicalScope != rt.Scope {
		return fmt.Errorf("refresh token scope is invalid: %w", store.ErrInvalidOAuthScope)
	}
	principal := models.Principal{
		UserID:       rt.UserID,
		TenantID:     rt.TenantID,
		MembershipID: rt.MembershipID,
		LearnerID:    rt.LearnerID,
		Roles:        []string{models.RoleLearner},
		Scopes:       strings.Fields(rt.Scope),
		TokenVersion: rt.MembershipVersion,
	}
	if err := principal.TenantScope().Validate(); err != nil || rt.MembershipVersion < 1 {
		return fmt.Errorf("refresh token tenant principal is invalid: %w", store.ErrInvalidPrincipal)
	}
	_, err = s.exec(ctx,
		`INSERT INTO refresh_tokens
		    (token, tenant_id, user_id, membership_id, membership_version, learner_id,
		     client_id, resource, scope, family_id, expires_at, created_at, used_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		refreshTokenHash(rt.Token), rt.TenantID, rt.UserID, rt.MembershipID, rt.MembershipVersion,
		rt.LearnerID, nullString(rt.ClientID), rt.Resource, rt.Scope, rt.FamilyID,
		rt.ExpiresAt, rt.CreatedAt, rt.UsedAt, rt.RevokedAt,
	)
	if err != nil {
		return err
	}
	return s.insertCredentialRoute(ctx, credentialKindRefreshToken, rt.Token, principal.TenantScope(), rt.ExpiresAt, rt.CreatedAt)
}

// CreateRefreshToken issues a refresh token bound to
// (learnerID, clientID, resource).
// Unbound refresh tokens are never issued: allowing a NULL client to be
// adopted during rotation lets any newly registered client redeem a stolen
// pre-upgrade credential.
func (s *Store) CreateRefreshToken(ctx context.Context, learnerID, clientID, resource string) (*models.RefreshToken, error) {
	return s.CreateRefreshTokenWithScope(ctx, learnerID, clientID, resource, models.OAuthScopeLearner)
}

func (s *Store) CreateRefreshTokenWithScope(ctx context.Context, learnerID, clientID, resource, scope string) (*models.RefreshToken, error) {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(resource) == "" {
		return nil, fmt.Errorf("create refresh token: client_id and resource are required")
	}
	canonicalScope, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		return nil, fmt.Errorf("create refresh token: %w", store.ErrInvalidOAuthScope)
	}
	principal, err := s.GetPrincipalForLearner(ctx, learnerID, strings.Fields(canonicalScope))
	if err != nil {
		return nil, fmt.Errorf("create refresh token: %w", store.ErrInvalidPrincipal)
	}
	return s.CreateRefreshTokenForPrincipal(ctx, principal, clientID, resource, canonicalScope)
}

func (s *Store) CreateRefreshTokenForPrincipal(ctx context.Context, principal models.Principal, clientID, resource, scope string) (*models.RefreshToken, error) {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(resource) == "" {
		return nil, fmt.Errorf("create refresh token: client_id and resource are required")
	}
	canonicalScope, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		return nil, fmt.Errorf("create refresh token: %w", store.ErrInvalidOAuthScope)
	}
	principal.Scopes = strings.Fields(canonicalScope)
	if err := principal.Validate(); err != nil {
		return nil, fmt.Errorf("create refresh token: %w", store.ErrInvalidPrincipal)
	}
	if !s.inTenantTransaction(ctx, principal.TenantScope()) && s.root != nil {
		var refreshToken *models.RefreshToken
		err := s.WithTenantTx(ctx, principal.TenantScope(), func(txCtx context.Context, scoped store.Store) error {
			var innerErr error
			refreshToken, innerErr = scoped.CreateRefreshTokenForPrincipal(txCtx, principal, clientID, resource, canonicalScope)
			return innerErr
		})
		return refreshToken, err
	}
	rt, err := newRefreshToken(principal.LearnerID, clientID, resource, canonicalScope, "")
	if err != nil {
		return nil, err
	}
	rt.UserID = principal.UserID
	rt.TenantID = principal.TenantID
	rt.MembershipID = principal.MembershipID
	rt.MembershipVersion = principal.TokenVersion
	rt.LearnerID = principal.LearnerID
	if err := s.insertRefreshToken(ctx, rt); err != nil {
		return nil, fmt.Errorf("create refresh token: %w", err)
	}
	return rt, nil
}

func (s *Store) GetRefreshToken(ctx context.Context, token string) (*models.RefreshToken, error) {
	scope, routeErr := s.credentialScope(ctx, credentialKindRefreshToken, token)
	if routeErr != nil {
		return nil, fmt.Errorf("get refresh token: %w", store.ErrInvalidRefreshToken)
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var refreshToken *models.RefreshToken
		err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped store.Store) error {
			var innerErr error
			refreshToken, innerErr = scoped.GetRefreshToken(txCtx, token)
			return innerErr
		})
		return refreshToken, err
	}
	rt := &models.RefreshToken{Token: token}
	var clientID sql.NullString
	var usedAt, revokedAt sql.NullTime
	err := s.queryRow(ctx,
		`SELECT user_id, tenant_id, membership_id, membership_version, learner_id,
		        client_id, resource, scope, family_id, expires_at, created_at, used_at, revoked_at
		 FROM refresh_tokens
		 WHERE token = ? AND tenant_id = ? AND user_id = ? AND membership_id = ? AND learner_id = ?
		   AND expires_at > ? AND used_at IS NULL AND revoked_at IS NULL`,
		refreshTokenHash(token), scope.TenantID, scope.UserID, scope.MembershipID, scope.LearnerID, time.Now().UTC(),
	).Scan(&rt.UserID, &rt.TenantID, &rt.MembershipID, &rt.MembershipVersion, &rt.LearnerID,
		&clientID, &rt.Resource, &rt.Scope, &rt.FamilyID, &rt.ExpiresAt, &rt.CreatedAt, &usedAt, &revokedAt)
	if err != nil {
		return nil, fmt.Errorf("get refresh token: %w", err)
	}
	if clientID.Valid {
		rt.ClientID = clientID.String
	}
	if usedAt.Valid {
		ts := usedAt.Time
		rt.UsedAt = &ts
	}
	if revokedAt.Valid {
		ts := revokedAt.Time
		rt.RevokedAt = &ts
	}
	return rt, nil
}

func (s *Store) DeleteRefreshToken(ctx context.Context, token string) error {
	scope, routeErr := s.credentialScope(ctx, credentialKindRefreshToken, token)
	if routeErr != nil {
		return fmt.Errorf("delete refresh token: %w", store.ErrInvalidRefreshToken)
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped store.Store) error {
			return scoped.DeleteRefreshToken(txCtx, token)
		})
	}
	_, err := s.exec(ctx,
		`DELETE FROM refresh_tokens WHERE token = ? AND tenant_id = ?`,
		refreshTokenHash(token), scope.TenantID,
	)
	if err != nil {
		return fmt.Errorf("delete refresh token: %w", err)
	}
	return s.deleteCredentialRoute(ctx, credentialKindRefreshToken, token)
}

// RotateRefreshToken consumes token and creates its successor in one
// transaction. Consumed rows are retained so a later replay can identify the
// token family and revoke every descendant. UPDATE ... RETURNING is the
// concurrency boundary: under PostgreSQL two simultaneous uses serialize on
// the row; under SQLite BEGIN IMMEDIATE provides the same ordering. If
// successor insertion fails, rollback restores the old token to active state.
func (s *Store) RotateRefreshToken(ctx context.Context, token, clientID, resource string) (*models.RefreshToken, error) {
	return s.RotateRefreshTokenWithScope(ctx, token, clientID, resource, "")
}

func (s *Store) RotateRefreshTokenWithScope(ctx context.Context, token, clientID, resource, requestedScope string) (*models.RefreshToken, error) {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(resource) == "" {
		return nil, fmt.Errorf("rotate refresh token: %w", store.ErrInvalidRefreshToken)
	}
	if requestedScope != "" {
		canonical, scopeErr := models.CanonicalOAuthScope(requestedScope)
		if scopeErr != nil || canonical != requestedScope {
			return nil, fmt.Errorf("rotate refresh token: %w", store.ErrInvalidOAuthScope)
		}
	}
	tenantScope, routeErr := s.credentialScope(ctx, credentialKindRefreshToken, token)
	if routeErr != nil {
		// Compatibility quarantine for a pre-routing plaintext row. The exact
		// credential is revoked but never redeemed. PostgreSQL RLS makes this a
		// no-op for an unscoped runtime role; the migration already revoked all
		// such rows before enabling routed credentials.
		_, _ = s.exec(ctx, `UPDATE refresh_tokens
		    SET revoked_at = COALESCE(revoked_at, ?)
		    WHERE token = ? AND token NOT LIKE 'sha256:%'`, time.Now().UTC(), token)
		return nil, fmt.Errorf("rotate refresh token: %w", store.ErrInvalidRefreshToken)
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var refreshToken *models.RefreshToken
		var rotationErr error
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped store.Store) error {
			refreshToken, rotationErr = scoped.RotateRefreshTokenWithScope(txCtx, token, clientID, resource, requestedScope)
			// Replay and malformed legacy families are revoked before the domain
			// error is returned. Commit those security side effects, then surface
			// the captured error after the tenant transaction closes.
			if errors.Is(rotationErr, store.ErrRefreshTokenReuse) || errors.Is(rotationErr, store.ErrInvalidRefreshToken) {
				return nil
			}
			return rotationErr
		})
		if err != nil {
			return nil, err
		}
		return refreshToken, rotationErr
	}
	successor, err := newRefreshToken("", clientID, resource, models.OAuthScopeLearner, "")
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	reuseDetected := false
	legacyRejected := false
	invalidScopeRejected := false
	err = s.inTx(ctx, nil, func(txs *Store) error {
		var storedClientID sql.NullString
		var storedResource, storedScope string
		err := txs.queryRow(ctx,
			`UPDATE refresh_tokens
			 SET used_at = ?,
			     family_id = CASE WHEN family_id = '' THEN ? ELSE family_id END
			 WHERE token = ?
			   AND expires_at > ?
			   AND used_at IS NULL
			   AND revoked_at IS NULL
			   AND client_id = ?
			   AND resource = ?
			 RETURNING user_id, tenant_id, membership_id, membership_version, learner_id,
			           client_id, resource, scope, family_id`,
			now, successor.FamilyID, refreshTokenHash(token), now, clientID, resource,
		).Scan(&successor.UserID, &successor.TenantID, &successor.MembershipID,
			&successor.MembershipVersion, &successor.LearnerID, &storedClientID,
			&storedResource, &storedScope, &successor.FamilyID)
		if errors.Is(err, sql.ErrNoRows) {
			var familyID string
			var existingClientID sql.NullString
			var existingResource, existingScope string
			var usedAt, revokedAt sql.NullTime
			lookupErr := txs.queryRow(ctx,
				`SELECT family_id, client_id, resource, scope, used_at, revoked_at
				 FROM refresh_tokens WHERE `+refreshTokenInspectionPredicate,
				refreshTokenHash(token), token,
			).Scan(&familyID, &existingClientID, &existingResource, &existingScope, &usedAt, &revokedAt)
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return store.ErrInvalidRefreshToken
			}
			if lookupErr != nil {
				return fmt.Errorf("inspect rejected refresh token: %w", lookupErr)
			}
			existingCanonicalScope, existingScopeErr := models.CanonicalOAuthScope(existingScope)
			if !existingClientID.Valid || existingClientID.String == "" || strings.TrimSpace(existingResource) == "" ||
				existingScopeErr != nil || existingCanonicalScope != existingScope {
				// Pre-binding tokens cannot prove which OAuth client originally
				// received them. Revoke rather than assigning attacker-selected
				// ownership on first use.
				if familyID != "" {
					if _, revokeErr := txs.exec(ctx,
						`UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE family_id = ?`,
						now, familyID,
					); revokeErr != nil {
						return fmt.Errorf("revoke unbound refresh token family: %w", revokeErr)
					}
				} else if _, revokeErr := txs.exec(ctx,
					`UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE `+refreshTokenInspectionPredicate,
					now, refreshTokenHash(token), token,
				); revokeErr != nil {
					return fmt.Errorf("revoke unbound refresh token: %w", revokeErr)
				}
				legacyRejected = true
				return nil
			}
			if existingClientID.Valid && existingClientID.String != "" && existingClientID.String != clientID {
				return store.ErrInvalidRefreshToken
			}
			if existingResource != resource {
				return store.ErrInvalidRefreshToken
			}
			if !usedAt.Valid && !revokedAt.Valid {
				// Expired-but-never-used tokens and other inactive states are not
				// evidence of a rotated-token replay.
				return store.ErrInvalidRefreshToken
			}

			if familyID != "" {
				if _, revokeErr := txs.exec(ctx,
					`UPDATE refresh_tokens
					 SET revoked_at = COALESCE(revoked_at, ?)
					 WHERE family_id = ?`,
					now, familyID,
				); revokeErr != nil {
					return fmt.Errorf("revoke refresh token family: %w", revokeErr)
				}
			} else if _, revokeErr := txs.exec(ctx,
				`UPDATE refresh_tokens
				 SET revoked_at = COALESCE(revoked_at, ?)
				 WHERE `+refreshTokenInspectionPredicate,
				now, refreshTokenHash(token), token,
			); revokeErr != nil {
				return fmt.Errorf("revoke legacy refresh token: %w", revokeErr)
			}
			reuseDetected = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("consume refresh token: %w", err)
		}
		canonicalStoredScope, scopeErr := models.CanonicalOAuthScope(storedScope)
		if scopeErr != nil || canonicalStoredScope != storedScope {
			if _, revokeErr := txs.exec(ctx,
				`UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE family_id = ?`,
				now, successor.FamilyID,
			); revokeErr != nil {
				return fmt.Errorf("revoke invalid-scope refresh token family: %w", revokeErr)
			}
			invalidScopeRejected = true
			return nil
		}
		successor.Scope = canonicalStoredScope
		if requestedScope != "" {
			if !models.OAuthScopeCanNarrow(canonicalStoredScope, requestedScope) {
				return store.ErrInvalidOAuthScope
			}
			successor.Scope = requestedScope
		}

		if err := txs.insertRefreshToken(ctx, successor); err != nil {
			return fmt.Errorf("create rotated refresh token: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rotate refresh token: %w", err)
	}
	if reuseDetected {
		return nil, fmt.Errorf("rotate refresh token: %w", store.ErrRefreshTokenReuse)
	}
	if legacyRejected {
		return nil, fmt.Errorf("rotate refresh token: %w", store.ErrInvalidRefreshToken)
	}
	if invalidScopeRejected {
		return nil, fmt.Errorf("rotate refresh token: %w", store.ErrInvalidRefreshToken)
	}
	return successor, nil
}

// ─── Domains ──────────────────────────────────────────────────────────────────

func (s *Store) CreateDomain(ctx context.Context, learnerID, name, personalGoal string, graph models.KnowledgeSpace) (*models.Domain, error) {
	return s.CreateDomainWithValueFramings(ctx, learnerID, name, personalGoal, graph, "")
}

// CreateDomainWithValueFramings creates a domain and optionally persists a JSON-encoded
// set of value framings (4 axes: financial, employment, intellectual, innovation).
func (s *Store) CreateDomainWithValueFramings(ctx context.Context, learnerID, name, personalGoal string, graph models.KnowledgeSpace, valueFramingsJSON string) (*models.Domain, error) {
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	graphJSON, err := json.Marshal(graph)
	if err != nil {
		return nil, fmt.Errorf("marshal graph: %w", err)
	}

	domain := &models.Domain{
		ID:                id,
		LearnerID:         learnerID,
		Name:              name,
		PersonalGoal:      personalGoal,
		Graph:             graph,
		ValueFramingsJSON: valueFramingsJSON,
		GraphVersion:      1,
		CreatedAt:         now,
	}
	err = s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx,
			`INSERT INTO domains (id, learner_id, name, personal_goal, graph_json, value_framings_json, graph_version, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
			id, learnerID, name, personalGoal, string(graphJSON), valueFramingsJSON, now,
		); err != nil {
			return fmt.Errorf("create domain: %w", err)
		}
		if err := txs.provisionDomainEnrollment(ctx, domain); err != nil {
			return err
		}
		baseline, err := buildCurriculumBaseline(domain, models.CurriculumOperationCreate, learnerID, now)
		if err != nil {
			return err
		}
		if _, err := txs.insertCurriculumSnapshot(ctx, learnerID, baseline, false); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return domain, nil
}

const domainCols = `id, learner_id, name, personal_goal, graph_json, value_framings_json, last_value_axis, archived, high_stakes, priority_rank, graph_version, goal_relevance_json, goal_relevance_version, phase, phase_changed_at, phase_entry_entropy, created_at, deleted_at`

// scanDomainFields is the shared row decoder for both *sql.Row and *sql.Rows
// callers — Scan has the same signature on both so we factor through a small
// interface to avoid duplicating the column ordering between scanDomainRow
// and scanDomainRows.
type domainScanner interface {
	Scan(dest ...any) error
}

func scanDomainFields(s domainScanner) (*models.Domain, error) {
	d := &models.Domain{}
	var graphJSON, goalRelevanceJSON string
	var valueFramings, lastAxis, phase sql.NullString
	var phaseChangedAt sql.NullTime
	var deletedAt sql.NullTime
	var phaseEntryEntropy sql.NullFloat64
	var priorityRank sql.NullInt64
	var archived, highStakes int
	err := s.Scan(
		&d.ID, &d.LearnerID, &d.Name, &d.PersonalGoal, &graphJSON,
		&valueFramings, &lastAxis, &archived, &highStakes,
		&priorityRank, &d.GraphVersion, &goalRelevanceJSON, &d.GoalRelevanceVersion,
		&phase, &phaseChangedAt, &phaseEntryEntropy,
		&d.CreatedAt, &deletedAt,
	)
	if err != nil {
		return nil, err
	}
	d.Archived = archived != 0
	d.HighStakes = highStakes != 0
	if priorityRank.Valid {
		rank := int(priorityRank.Int64)
		d.PriorityRank = &rank
	}
	if valueFramings.Valid {
		d.ValueFramingsJSON = valueFramings.String
	}
	if lastAxis.Valid {
		d.LastValueAxis = lastAxis.String
	}
	d.GoalRelevanceJSON = goalRelevanceJSON
	if phase.Valid {
		d.Phase = models.Phase(phase.String)
	}
	if phaseChangedAt.Valid {
		d.PhaseChangedAt = phaseChangedAt.Time
	}
	if phaseEntryEntropy.Valid {
		d.PhaseEntryEntropy = phaseEntryEntropy.Float64
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		d.DeletedAt = &t
	}
	if err := json.Unmarshal([]byte(graphJSON), &d.Graph); err != nil {
		return nil, fmt.Errorf("unmarshal graph: %w", err)
	}
	return d, nil
}

func scanDomainRow(row *sql.Row) (*models.Domain, error) {
	return scanDomainFields(row)
}

func scanDomainRows(rows *sql.Rows) (*models.Domain, error) {
	return scanDomainFields(rows)
}

func (s *Store) GetDomainByLearner(ctx context.Context, learnerID string) (*models.Domain, error) {
	row := s.queryRow(ctx,
		`SELECT `+domainCols+` FROM domains WHERE learner_id = ? AND archived = 0 AND deleted_at IS NULL
		 ORDER BY CASE WHEN priority_rank IS NULL THEN 1 ELSE 0 END, priority_rank ASC, created_at DESC LIMIT 1`,
		learnerID,
	)
	d, err := scanDomainRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = store.WrapNotFound(err)
		}
		return nil, fmt.Errorf("get domain by learner: %w", err)
	}
	return d, nil
}

func (s *Store) GetDomainByID(ctx context.Context, id string) (*models.Domain, error) {
	row := s.queryRow(ctx, `SELECT `+domainCols+` FROM domains WHERE id = ? AND deleted_at IS NULL`, id)
	d, err := scanDomainRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = store.WrapNotFound(err)
		}
		return nil, fmt.Errorf("get domain by id: %w", err)
	}
	return d, nil
}

func (s *Store) GetDomainsByLearner(ctx context.Context, learnerID string, includeArchived bool) ([]*models.Domain, error) {
	query := `SELECT ` + domainCols + ` FROM domains WHERE learner_id = ? AND deleted_at IS NULL`
	if !includeArchived {
		query += ` AND archived = 0`
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.query(ctx, query, learnerID)
	if err != nil {
		return nil, fmt.Errorf("get domains by learner: %w", err)
	}
	defer rows.Close()

	var domains []*models.Domain
	for rows.Next() {
		d, err := scanDomainRows(rows)
		if err != nil {
			return nil, fmt.Errorf("scan domain row: %w", err)
		}
		domains = append(domains, d)
	}
	return domains, rows.Err()
}

func (s *Store) SetDomainPriority(ctx context.Context, domainID, learnerID string, rank int) error {
	if rank < 1 {
		return fmt.Errorf("rank must be >= 1")
	}
	result, err := s.exec(ctx,
		`UPDATE domains SET priority_rank = ? WHERE id = ? AND learner_id = ? AND archived = 0`,
		rank, domainID, learnerID,
	)
	if err != nil {
		return fmt.Errorf("set domain priority: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("domain not found")
	}
	return nil
}

// MarkDomainHighStakes is intentionally one-way on the public persistence
// port. Downgrading safety classification requires an operator-controlled
// process that is outside the learner-facing MCP trust boundary.
func (s *Store) MarkDomainHighStakes(ctx context.Context, domainID, learnerID string) error {
	result, err := s.exec(ctx,
		`UPDATE domains SET high_stakes = 1
		 WHERE id = ? AND learner_id = ? AND archived = 0 AND deleted_at IS NULL`,
		domainID, learnerID,
	)
	if err != nil {
		return fmt.Errorf("mark domain high stakes: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark domain high stakes rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("domain not found")
	}
	return nil
}

// UpdateDomainValueFramings stores the JSON-encoded value framings for a domain.
func (s *Store) UpdateDomainValueFramings(ctx context.Context, domainID, valueFramingsJSON string) error {
	_, err := s.exec(ctx,
		`UPDATE domains SET value_framings_json = ? WHERE id = ?`,
		valueFramingsJSON, domainID,
	)
	if err != nil {
		return fmt.Errorf("update domain value framings: %w", err)
	}
	return nil
}

// UpdateDomainLastValueAxis records which axis was surfaced most recently (used for rotation).
func (s *Store) UpdateDomainLastValueAxis(ctx context.Context, domainID, axis string) error {
	_, err := s.exec(ctx,
		`UPDATE domains SET last_value_axis = ? WHERE id = ?`,
		axis, domainID,
	)
	if err != nil {
		return fmt.Errorf("update domain last value axis: %w", err)
	}
	return nil
}

func (s *Store) UpdateDomainGraph(ctx context.Context, domainID string, graph models.KnowledgeSpace) error {
	domain, err := s.GetDomainByID(ctx, domainID)
	if err != nil {
		return fmt.Errorf("update domain graph: %w", err)
	}
	current, err := s.EnsureCurriculumBaseline(ctx, domain.LearnerID, domainID)
	if err != nil {
		return fmt.Errorf("update domain graph baseline: %w", err)
	}
	next, err := reconcileLegacyGraph(current, graph, domain.LearnerID)
	if err != nil {
		return fmt.Errorf("update domain graph revision: %w", err)
	}
	if err := s.CompareAndSwapCurriculum(ctx, domain.LearnerID, domainID, current.Version, next); err != nil {
		return fmt.Errorf("update domain graph: %w", err)
	}
	return nil
}

func (s *Store) ArchiveDomain(ctx context.Context, domainID, learnerID string) error {
	return s.inTx(ctx, nil, func(txs *Store) error {
		result, err := txs.exec(ctx,
			`UPDATE domains SET archived = 1 WHERE id = ? AND learner_id = ? AND deleted_at IS NULL`,
			domainID, learnerID,
		)
		if err != nil {
			return fmt.Errorf("archive domain: %w", err)
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return fmt.Errorf("domain not found")
		}
		if err := txs.expireWebhookMessagesForDomain(ctx, learnerID, domainID); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) UnarchiveDomain(ctx context.Context, domainID, learnerID string) error {
	result, err := s.exec(ctx,
		`UPDATE domains SET archived = 0 WHERE id = ? AND learner_id = ? AND deleted_at IS NULL`,
		domainID, learnerID,
	)
	if err != nil {
		return fmt.Errorf("unarchive domain: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("domain not found")
	}
	return nil
}

// ActiveDomainConceptSet returns the set of concepts that belong to at least
// one non-archived domain owned by the learner. Used by readers to filter out
// concept_states / interactions attached to a delete_domain tombstone (which
// intentionally preserves both the domain identity and its history).
func (s *Store) ActiveDomainConceptSet(ctx context.Context, learnerID string) (map[string]bool, error) {
	domains, err := s.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return nil, fmt.Errorf("active domain concept set: %w", err)
	}
	set := make(map[string]bool)
	for _, d := range domains {
		for _, c := range d.Graph.Concepts {
			set[c] = true
		}
	}
	return set, nil
}

func (s *Store) DeleteDomain(ctx context.Context, domainID, learnerID string) error {
	return s.inTx(ctx, nil, func(txs *Store) error {
		// Upgraded installations can still contain a legacy domain that has
		// never been read through the curriculum API. Materialize its immutable
		// baseline before hiding the domain; after the tombstone runtime domain
		// readers intentionally cannot reconstruct it.
		if _, err := txs.EnsureCurriculumBaseline(ctx, learnerID, domainID); err != nil {
			return fmt.Errorf("preserve curriculum before tombstone: %w", err)
		}
		now := time.Now().UTC()
		result, err := txs.exec(ctx,
			`UPDATE domains SET archived = 1, deleted_at = ?
			 WHERE id = ? AND learner_id = ? AND deleted_at IS NULL`,
			now, domainID, learnerID,
		)
		if err != nil {
			return fmt.Errorf("tombstone domain: %w", err)
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return fmt.Errorf("domain not found")
		}

		// Intentions, terminal delivery records and all learning evidence remain
		// attached to the tombstoned domain. Pending intentions are resolved as
		// cancelled rather than deleted so they stop routing but keep their audit.
		if _, err := txs.exec(ctx,
			`UPDATE implementation_intentions
			 SET status = ?, honored = 0, resolved_at = ?, updated_at = ?
			 WHERE learner_id = ? AND domain_id = ? AND status = ?`,
			models.IntentionStatusCancelled, now, now, learnerID, domainID, models.IntentionStatusPending,
		); err != nil {
			return fmt.Errorf("cancel deleted domain intentions: %w", err)
		}

		if err := txs.expireWebhookMessagesForDomain(ctx, learnerID, domainID); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) expireWebhookMessagesForDomain(ctx context.Context, learnerID, domainID string) error {
	if _, err := s.exec(ctx,
		`UPDATE webhook_message_queue
		 SET status = 'expired', claimed_at = NULL, next_attempt_at = NULL,
		     last_error = 'domain_inactive', dead_lettered_at = NULL
		 WHERE learner_id = ? AND domain_id = ?
		   AND status IN ('pending', 'processing')`,
		learnerID, domainID,
	); err != nil {
		return fmt.Errorf("expire inactive domain webhook queue: %w", err)
	}
	return nil
}

func (s *Store) InsertConceptStateIfNotExists(ctx context.Context, cs *models.ConceptState) error {
	if err := s.inferConceptStateDomain(ctx, cs); err != nil {
		return err
	}
	scope, err := s.resolveLearningScope(ctx, cs.LearnerID, cs.DomainID, cs.Concept)
	if err != nil {
		return err
	}
	cs.UpdatedAt = time.Now().UTC()
	_, err = s.exec(ctx,
		`INSERT INTO concept_states
		    (learner_id, domain_id, concept, tenant_id, enrollment_id, formation_concept_id,
		     stability, difficulty, elapsed_days, scheduled_days,
		     reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
		     p_slip, p_guess, theta, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (learner_id, domain_id, concept) DO NOTHING`,
		cs.LearnerID, cs.DomainID, cs.Concept, scope.TenantID, scope.EnrollmentID, scope.FormationConceptID,
		cs.Stability, cs.Difficulty, cs.ElapsedDays, cs.ScheduledDays,
		cs.Reps, cs.Lapses, cs.CardState, cs.LastReview, cs.NextReview,
		cs.PMastery, cs.PLearn, cs.PForget, cs.PSlip, cs.PGuess,
		cs.Theta, cs.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert concept state if not exists: %w", err)
	}
	return nil
}

// ─── Concept States ───────────────────────────────────────────────────────────

func (s *Store) GetConceptState(ctx context.Context, learnerID, concept string) (*models.ConceptState, error) {
	return s.getConceptState(ctx, learnerID, "", concept, false, false)
}

func (s *Store) GetConceptStateInDomain(ctx context.Context, learnerID, domainID, concept string) (*models.ConceptState, error) {
	return s.getConceptState(ctx, learnerID, domainID, concept, false, true)
}

func (s *Store) getConceptState(ctx context.Context, learnerID, domainID, concept string, forUpdate, exactDomain bool) (*models.ConceptState, error) {
	cs := &models.ConceptState{}
	var lastReview, nextReview sql.NullTime
	query := `SELECT id, learner_id, domain_id, concept, stability, difficulty, elapsed_days, scheduled_days,
		        reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
		        p_slip, p_guess, theta, updated_at
		 FROM concept_states WHERE learner_id = ? AND concept = ?`
	args := []any{learnerID, concept}
	if exactDomain {
		query += ` AND domain_id = ?`
		args = append(args, domainID)
	} else {
		// Compatibility path for callers that have not selected a domain:
		// prefer a legacy unscoped row, then return a deterministic scoped
		// row. Learning decisions must use GetConceptStateInDomain.
		query += ` ORDER BY CASE WHEN domain_id = '' THEN 0 ELSE 1 END, domain_id LIMIT 1`
	}
	if forUpdate && s.dialect == DialectPostgres {
		query += ` FOR UPDATE`
	}
	err := s.queryRow(ctx, query, args...).Scan(
		&cs.ID, &cs.LearnerID, &cs.DomainID, &cs.Concept, &cs.Stability, &cs.Difficulty,
		&cs.ElapsedDays, &cs.ScheduledDays, &cs.Reps, &cs.Lapses, &cs.CardState,
		&lastReview, &nextReview,
		&cs.PMastery, &cs.PLearn, &cs.PForget, &cs.PSlip, &cs.PGuess,
		&cs.Theta, &cs.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get concept state: %w", err)
	}
	if lastReview.Valid {
		cs.LastReview = &lastReview.Time
	}
	if nextReview.Valid {
		cs.NextReview = &nextReview.Time
	}
	return cs, nil
}

func (s *Store) UpsertConceptState(ctx context.Context, cs *models.ConceptState) error {
	if err := s.inferConceptStateDomain(ctx, cs); err != nil {
		return err
	}
	scope, err := s.resolveLearningScope(ctx, cs.LearnerID, cs.DomainID, cs.Concept)
	if err != nil {
		return err
	}
	cs.UpdatedAt = time.Now().UTC()
	_, err = s.exec(ctx,
		`INSERT INTO concept_states
		    (learner_id, domain_id, concept, tenant_id, enrollment_id, formation_concept_id,
		     stability, difficulty, elapsed_days, scheduled_days,
		     reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
		     p_slip, p_guess, theta, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(learner_id, domain_id, concept) DO UPDATE SET
		    stability      = excluded.stability,
		    difficulty     = excluded.difficulty,
		    elapsed_days   = excluded.elapsed_days,
		    scheduled_days = excluded.scheduled_days,
		    reps           = excluded.reps,
		    lapses         = excluded.lapses,
		    card_state     = excluded.card_state,
		    last_review    = excluded.last_review,
		    next_review    = excluded.next_review,
		    p_mastery      = excluded.p_mastery,
		    p_learn        = excluded.p_learn,
		    p_forget       = excluded.p_forget,
		    p_slip         = excluded.p_slip,
		    p_guess        = excluded.p_guess,
		    theta          = excluded.theta,
		    updated_at     = excluded.updated_at`,
		cs.LearnerID, cs.DomainID, cs.Concept, scope.TenantID, scope.EnrollmentID, scope.FormationConceptID,
		cs.Stability, cs.Difficulty, cs.ElapsedDays, cs.ScheduledDays,
		cs.Reps, cs.Lapses, cs.CardState, cs.LastReview, cs.NextReview,
		cs.PMastery, cs.PLearn, cs.PForget, cs.PSlip, cs.PGuess,
		cs.Theta, cs.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert concept state: %w", err)
	}
	return nil
}

// inferConceptStateDomain keeps the Go persistence API backward compatible
// without reviving cross-domain collisions. A missing DomainID is filled only
// when exactly one active domain owned by the learner contains the concept. If
// the label is ambiguous, the write is rejected and the caller must choose.
func (s *Store) inferConceptStateDomain(ctx context.Context, cs *models.ConceptState) error {
	if cs == nil {
		return fmt.Errorf("concept state is required")
	}
	if cs.DomainID != "" || cs.Concept == "" {
		return nil
	}
	domainID, err := s.inferUniqueConceptDomain(ctx, cs.LearnerID, cs.Concept)
	if err != nil {
		return err
	}
	cs.DomainID = domainID
	return nil
}

func (s *Store) inferUniqueConceptDomain(ctx context.Context, learnerID, concept string) (string, error) {
	domains, err := s.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return "", fmt.Errorf("infer concept domain: %w", err)
	}
	match := ""
	for _, domain := range domains {
		found := false
		for _, candidate := range domain.Graph.Concepts {
			if candidate == concept {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if match != "" && match != domain.ID {
			return "", fmt.Errorf("domain_id is required: concept %q exists in multiple domains", concept)
		}
		match = domain.ID
	}
	return match, nil
}

func (s *Store) GetConceptStatesByLearner(ctx context.Context, learnerID string) ([]*models.ConceptState, error) {
	return s.getConceptStates(ctx, learnerID, "")
}

func (s *Store) GetConceptStatesByDomain(ctx context.Context, learnerID, domainID string) ([]*models.ConceptState, error) {
	return s.getConceptStates(ctx, learnerID, domainID)
}

func (s *Store) getConceptStates(ctx context.Context, learnerID, domainID string) ([]*models.ConceptState, error) {
	query := `SELECT id, learner_id, domain_id, concept, stability, difficulty, elapsed_days, scheduled_days,
		        reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
		        p_slip, p_guess, theta, updated_at
		 FROM concept_states WHERE learner_id = ?`
	args := []any{learnerID}
	if domainID != "" {
		query += ` AND domain_id = ?`
		args = append(args, domainID)
	}
	rows, err := s.query(ctx,
		query,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("get concept states by learner: %w", err)
	}
	defer rows.Close()

	var states []*models.ConceptState
	for rows.Next() {
		cs := &models.ConceptState{}
		var lastReview, nextReview sql.NullTime
		if err := rows.Scan(
			&cs.ID, &cs.LearnerID, &cs.DomainID, &cs.Concept, &cs.Stability, &cs.Difficulty,
			&cs.ElapsedDays, &cs.ScheduledDays, &cs.Reps, &cs.Lapses, &cs.CardState,
			&lastReview, &nextReview,
			&cs.PMastery, &cs.PLearn, &cs.PForget, &cs.PSlip, &cs.PGuess,
			&cs.Theta, &cs.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan concept state row: %w", err)
		}
		if lastReview.Valid {
			cs.LastReview = &lastReview.Time
		}
		if nextReview.Valid {
			cs.NextReview = &nextReview.Time
		}
		states = append(states, cs)
	}
	return states, rows.Err()
}

// ─── Interactions ─────────────────────────────────────────────────────────────

const interactionCols = `id, learner_id, session_id, assessment_attempt_id, concept, activity_type, success, response_time, confidence, error_type, notes, hints_requested, self_initiated, calibration_id, is_proactive_review, misconception_type, misconception_detail, domain_id, bkt_slip, bkt_guess, rubric_json, rubric_score_json, created_at`

func (s *Store) CreateInteraction(ctx context.Context, i *models.Interaction) error {
	return createInteractionWithStore(ctx, s, i)
}

func createInteractionWithStore(ctx context.Context, s *Store, i *models.Interaction) error {
	if i == nil {
		return fmt.Errorf("interaction is required")
	}
	if i.DomainID == "" {
		domainID, err := s.inferUniqueConceptDomain(ctx, i.LearnerID, i.Concept)
		if err != nil {
			return err
		}
		i.DomainID = domainID
	}
	if i.CreatedAt.IsZero() {
		i.CreatedAt = time.Now().UTC()
	} else {
		// Preserve the authoritative event time supplied by internal replay,
		// assessment and migration paths. Learner-facing tools never accept this
		// field directly; they pass the server clock through applyInteraction.
		i.CreatedAt = i.CreatedAt.UTC()
	}
	if i.SessionID != "" {
		if _, err := s.TouchLearningSession(ctx, i.LearnerID, i.SessionID, i.CreatedAt); err != nil {
			return fmt.Errorf("validate interaction session: %w", err)
		}
	}
	scope, err := s.resolveLearningScope(ctx, i.LearnerID, i.DomainID, i.Concept)
	if err != nil {
		return err
	}
	id, err := s.insertReturningID(ctx,
		`INSERT INTO interactions (learner_id, session_id, assessment_attempt_id, concept, activity_type, success, response_time, confidence, error_type, notes, hints_requested, self_initiated, calibration_id, is_proactive_review, misconception_type, misconception_detail, domain_id, tenant_id, enrollment_id, formation_concept_id, bkt_slip, bkt_guess, rubric_json, rubric_score_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.LearnerID, nullString(i.SessionID), nullString(i.AssessmentAttemptID), i.Concept, i.ActivityType, boolToInt(i.Success),
		i.ResponseTime, i.Confidence, i.ErrorType, i.Notes,
		i.HintsRequested, boolToInt(i.SelfInitiated), i.CalibrationID, boolToInt(i.IsProactiveReview),
		nullString(i.MisconceptionType), nullString(i.MisconceptionDetail),
		nullString(i.DomainID),
		scope.TenantID, scope.EnrollmentID, scope.FormationConceptID,
		nullFloat64(i.BKTSlip), nullFloat64(i.BKTGuess),
		nullString(i.RubricJSON), nullString(i.RubricScoreJSON),
		i.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create interaction: %w", err)
	}
	i.ID = id
	return nil
}

func (s *Store) GetRecentInteractions(ctx context.Context, learnerID, concept string, limit int) ([]*models.Interaction, error) {
	return s.getRecentInteractions(ctx, learnerID, "", concept, limit, false)
}

func (s *Store) GetRecentInteractionsInDomain(ctx context.Context, learnerID, domainID, concept string, limit int) ([]*models.Interaction, error) {
	return s.getRecentInteractions(ctx, learnerID, domainID, concept, limit, true)
}

func (s *Store) getRecentInteractions(ctx context.Context, learnerID, domainID, concept string, limit int, exactDomain bool) ([]*models.Interaction, error) {
	query := `SELECT ` + interactionCols + ` FROM interactions WHERE learner_id = ? AND concept = ?`
	args := []any{learnerID, concept}
	if exactDomain {
		query += ` AND domain_id = ?`
		args = append(args, domainID)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.query(ctx,
		query,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("get recent interactions: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetRecentInteractionsByLearner(ctx context.Context, learnerID string, limit int) ([]*models.Interaction, error) {
	rows, err := s.query(ctx,
		`SELECT `+interactionCols+` FROM interactions WHERE learner_id = ?
		 ORDER BY created_at DESC LIMIT ?`,
		learnerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get recent interactions by learner: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetRecentInteractionsByDomain(ctx context.Context, learnerID, domainID string, limit int) ([]*models.Interaction, error) {
	rows, err := s.query(ctx,
		`SELECT `+interactionCols+` FROM interactions
		 WHERE learner_id = ? AND domain_id = ?
		 ORDER BY created_at DESC LIMIT ?`,
		learnerID, domainID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get recent interactions by domain: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetSessionInteractions(ctx context.Context, learnerID string) ([]*models.Interaction, error) {
	var sessionID string
	err := s.queryRow(ctx,
		`SELECT id FROM learning_sessions WHERE learner_id = ? AND status = ? LIMIT 1`,
		learnerID, models.LearningSessionStatusOpen,
	).Scan(&sessionID)
	if err == nil {
		return s.GetInteractionsBySession(ctx, learnerID, sessionID)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("get active learning session: %w", err)
	}
	// Compatibility path: only pre-session rows participate in the legacy
	// two-hour window. Explicitly closed sessions never leak into a new one.
	cutoff := time.Now().UTC().Add(-2 * time.Hour)
	rows, err := s.query(ctx,
		`SELECT `+interactionCols+` FROM interactions WHERE learner_id = ? AND session_id IS NULL AND created_at > ?
		 ORDER BY created_at DESC`,
		learnerID, cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("get session interactions: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetSessionInteractionsInDomain(ctx context.Context, learnerID, domainID string) ([]*models.Interaction, error) {
	var sessionID string
	err := s.queryRow(ctx,
		`SELECT id FROM learning_sessions WHERE learner_id = ? AND status = ? LIMIT 1`,
		learnerID, models.LearningSessionStatusOpen,
	).Scan(&sessionID)
	if err == nil {
		return s.GetInteractionsBySessionInDomain(ctx, learnerID, sessionID, domainID)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("get active learning session: %w", err)
	}
	cutoff := time.Now().UTC().Add(-2 * time.Hour)
	rows, err := s.query(ctx,
		`SELECT `+interactionCols+` FROM interactions
		 WHERE learner_id = ? AND domain_id = ? AND session_id IS NULL AND created_at > ?
		 ORDER BY created_at DESC`,
		learnerID, domainID, cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("get domain session interactions: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetInteractionsBySession(ctx context.Context, learnerID, sessionID string) ([]*models.Interaction, error) {
	rows, err := s.query(ctx,
		`SELECT `+interactionCols+` FROM interactions
		 WHERE learner_id = ? AND session_id = ? ORDER BY created_at DESC`,
		learnerID, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("get interactions by session: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetInteractionsBySessionInDomain(ctx context.Context, learnerID, sessionID, domainID string) ([]*models.Interaction, error) {
	rows, err := s.query(ctx,
		`SELECT `+interactionCols+` FROM interactions
		 WHERE learner_id = ? AND session_id = ? AND domain_id = ? ORDER BY created_at DESC`,
		learnerID, sessionID, domainID,
	)
	if err != nil {
		return nil, fmt.Errorf("get domain interactions by session: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func scanInteractions(rows *sql.Rows) ([]*models.Interaction, error) {
	var interactions []*models.Interaction
	for rows.Next() {
		i := &models.Interaction{}
		var successInt, selfInitInt, proactiveInt int
		var responseTime sql.NullInt64
		var confidence sql.NullFloat64
		var sessionID, assessmentAttemptID, errorType, notes, calibrationID, misconceptionType, misconceptionDetail, domainID, rubricJSON, rubricScoreJSON sql.NullString
		var bktSlip, bktGuess sql.NullFloat64
		if err := rows.Scan(
			&i.ID, &i.LearnerID, &sessionID, &assessmentAttemptID, &i.Concept, &i.ActivityType,
			&successInt, &responseTime, &confidence, &errorType, &notes,
			&i.HintsRequested, &selfInitInt, &calibrationID, &proactiveInt,
			&misconceptionType, &misconceptionDetail, &domainID,
			&bktSlip, &bktGuess,
			&rubricJSON, &rubricScoreJSON,
			&i.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan interaction row: %w", err)
		}
		if responseTime.Valid {
			i.ResponseTime = int(responseTime.Int64)
		}
		if sessionID.Valid {
			i.SessionID = sessionID.String
		}
		if assessmentAttemptID.Valid {
			i.AssessmentAttemptID = assessmentAttemptID.String
		}
		if confidence.Valid {
			i.Confidence = confidence.Float64
		}
		i.Success = successInt != 0
		i.SelfInitiated = selfInitInt != 0
		i.IsProactiveReview = proactiveInt != 0
		if errorType.Valid {
			i.ErrorType = errorType.String
		}
		if notes.Valid {
			i.Notes = notes.String
		}
		if calibrationID.Valid {
			i.CalibrationID = calibrationID.String
		}
		if misconceptionType.Valid {
			i.MisconceptionType = misconceptionType.String
		}
		if misconceptionDetail.Valid {
			i.MisconceptionDetail = misconceptionDetail.String
		}
		if domainID.Valid {
			i.DomainID = domainID.String
		}
		if bktSlip.Valid {
			v := bktSlip.Float64
			i.BKTSlip = &v
		}
		if bktGuess.Valid {
			v := bktGuess.Float64
			i.BKTGuess = &v
		}
		if rubricJSON.Valid {
			i.RubricJSON = rubricJSON.String
		}
		if rubricScoreJSON.Valid {
			i.RubricScoreJSON = rubricScoreJSON.String
		}
		interactions = append(interactions, i)
	}
	return interactions, rows.Err()
}

func (s *Store) GetInteractionsSince(ctx context.Context, learnerID string, since time.Time) ([]*models.Interaction, error) {
	rows, err := s.query(ctx,
		`SELECT `+interactionCols+` FROM interactions WHERE learner_id = ? AND created_at >= ? ORDER BY created_at ASC`,
		learnerID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("get interactions since: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetInteractionsSinceInDomain(ctx context.Context, learnerID, domainID string, since time.Time) ([]*models.Interaction, error) {
	rows, err := s.query(ctx,
		`SELECT `+interactionCols+` FROM interactions
		 WHERE learner_id = ? AND domain_id = ? AND created_at >= ?
		 ORDER BY created_at ASC`,
		learnerID, domainID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("get domain interactions since: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetSessionStart(ctx context.Context, learnerID string) (time.Time, error) {
	var startedAt time.Time
	err := s.queryRow(ctx,
		`SELECT started_at FROM learning_sessions WHERE learner_id = ? AND status = ? LIMIT 1`,
		learnerID, models.LearningSessionStatusOpen,
	).Scan(&startedAt)
	if err == nil {
		return startedAt, nil
	}
	if err != sql.ErrNoRows {
		return time.Time{}, fmt.Errorf("get active learning session start: %w", err)
	}
	cutoff := time.Now().UTC().Add(-2 * time.Hour)
	var sessionStart flexTime
	err = s.queryRow(ctx,
		`SELECT MIN(created_at) FROM interactions WHERE learner_id = ? AND session_id IS NULL AND created_at > ?`,
		learnerID, cutoff,
	).Scan(&sessionStart)
	if err != nil {
		return time.Time{}, fmt.Errorf("get session start: %w", err)
	}
	if !sessionStart.Valid {
		return time.Now().UTC(), nil
	}
	return sessionStart.Time, nil
}

// ─── Availability ─────────────────────────────────────────────────────────────

func (s *Store) GetAvailability(ctx context.Context, learnerID string) (*models.Availability, error) {
	a := &models.Availability{}
	var dndInt, consentInt int
	var updatedAt sql.NullTime
	err := s.queryRow(ctx,
		`SELECT learner_id, timezone, windows_json, avg_duration, sessions_week,
		        do_not_disturb, notification_consent, notification_frequency,
		        max_notifications_per_day, accessibility_json, version, updated_at
		 FROM availability WHERE learner_id = ?`,
		learnerID,
	).Scan(
		&a.LearnerID, &a.Timezone, &a.WindowsJSON, &a.AvgDuration, &a.SessionsWeek,
		&dndInt, &consentInt, &a.NotificationFrequency,
		&a.MaxNotificationsPerDay, &a.AccessibilityJSON, &a.Version, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return models.DefaultAvailability(learnerID), nil
	}
	if err != nil {
		return nil, fmt.Errorf("get availability: %w", err)
	}
	a.DoNotDisturb = dndInt != 0
	a.NotificationConsent = consentInt != 0
	if updatedAt.Valid {
		a.UpdatedAt = updatedAt.Time
	}
	if err := a.NormalizeAndValidate(); err != nil {
		return nil, fmt.Errorf("get availability: invalid stored policy: %w", err)
	}
	return a, nil
}

func (s *Store) UpsertAvailability(ctx context.Context, a *models.Availability) error {
	if err := a.NormalizeAndValidate(); err != nil {
		return fmt.Errorf("upsert availability: %w", err)
	}
	if err := s.requireLearner(ctx, a.LearnerID); err != nil {
		return fmt.Errorf("upsert availability: %w", err)
	}
	now := time.Now().UTC()
	_, err := s.exec(ctx,
		`INSERT INTO availability
		    (learner_id, timezone, windows_json, avg_duration, sessions_week,
		     do_not_disturb, notification_consent, notification_frequency,
		     max_notifications_per_day, accessibility_json, version, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
		 ON CONFLICT(learner_id) DO UPDATE SET
		    timezone                 = excluded.timezone,
		    windows_json             = excluded.windows_json,
		    avg_duration             = excluded.avg_duration,
		    sessions_week            = excluded.sessions_week,
		    do_not_disturb           = excluded.do_not_disturb,
		    notification_consent     = excluded.notification_consent,
		    notification_frequency   = excluded.notification_frequency,
		    max_notifications_per_day = excluded.max_notifications_per_day,
		    accessibility_json       = excluded.accessibility_json,
		    version                  = availability.version + 1,
		    updated_at               = excluded.updated_at`,
		a.LearnerID, a.Timezone, a.WindowsJSON, a.AvgDuration, a.SessionsWeek,
		boolToInt(a.DoNotDisturb), boolToInt(a.NotificationConsent), a.NotificationFrequency,
		a.MaxNotificationsPerDay, a.AccessibilityJSON, now,
	)
	if err != nil {
		return fmt.Errorf("upsert availability: %w", err)
	}
	return nil
}

// UpdateAvailability is the optimistic-concurrency path exposed to MCP
// clients. Version zero means "create only"; existing rows require the exact
// version returned by get_availability_model.
func (s *Store) UpdateAvailability(ctx context.Context, a *models.Availability, expectedVersion int) (*models.Availability, error) {
	if expectedVersion < 0 {
		return nil, fmt.Errorf("update availability: expected_version must be non-negative")
	}
	if err := a.NormalizeAndValidate(); err != nil {
		return nil, fmt.Errorf("update availability: %w", err)
	}
	if err := s.requireLearner(ctx, a.LearnerID); err != nil {
		return nil, fmt.Errorf("update availability: %w", err)
	}
	now := time.Now().UTC()
	var updated *models.Availability
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		var result sql.Result
		var err error
		if expectedVersion == 0 {
			result, err = txs.exec(ctx,
				`INSERT INTO availability
				    (learner_id, timezone, windows_json, avg_duration, sessions_week,
				     do_not_disturb, notification_consent, notification_frequency,
				     max_notifications_per_day, accessibility_json, version, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
				 ON CONFLICT(learner_id) DO NOTHING`,
				a.LearnerID, a.Timezone, a.WindowsJSON, a.AvgDuration, a.SessionsWeek,
				boolToInt(a.DoNotDisturb), boolToInt(a.NotificationConsent), a.NotificationFrequency,
				a.MaxNotificationsPerDay, a.AccessibilityJSON, now,
			)
		} else {
			result, err = txs.exec(ctx,
				`UPDATE availability
				 SET timezone = ?, windows_json = ?, avg_duration = ?, sessions_week = ?,
				     do_not_disturb = ?, notification_consent = ?, notification_frequency = ?,
				     max_notifications_per_day = ?, accessibility_json = ?,
				     version = version + 1, updated_at = ?
				 WHERE learner_id = ? AND version = ?`,
				a.Timezone, a.WindowsJSON, a.AvgDuration, a.SessionsWeek,
				boolToInt(a.DoNotDisturb), boolToInt(a.NotificationConsent), a.NotificationFrequency,
				a.MaxNotificationsPerDay, a.AccessibilityJSON, now, a.LearnerID, expectedVersion,
			)
		}
		if err != nil {
			return fmt.Errorf("persist availability: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("availability update rows affected: %w", err)
		}
		if rows != 1 {
			return store.ErrAvailabilityVersionConflict
		}
		updated, err = txs.GetAvailability(ctx, a.LearnerID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) requireLearner(ctx context.Context, learnerID string) error {
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM learners WHERE id = ?`, learnerID).Scan(&count); err != nil {
		return fmt.Errorf("validate learner: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("learner not found")
	}
	return nil
}

// ─── Scheduled Alerts ─────────────────────────────────────────────────────────

func (s *Store) CreateScheduledAlert(ctx context.Context, learnerID, alertType, concept string, scheduledAt time.Time) error {
	_, err := s.exec(ctx,
		`INSERT INTO scheduled_alerts (learner_id, alert_type, concept, scheduled_at) VALUES (?, ?, ?, ?)`,
		learnerID, alertType, concept, scheduledAt,
	)
	if err != nil {
		return fmt.Errorf("create scheduled alert: %w", err)
	}
	return nil
}

func (s *Store) WasAlertSentToday(ctx context.Context, learnerID, alertType string) (bool, error) {
	availability, err := s.GetAvailability(ctx, learnerID)
	if err != nil {
		return false, fmt.Errorf("check alert sent today: load availability: %w", err)
	}
	bounds, err := availability.NotificationBounds(time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("check alert sent today: derive local day: %w", err)
	}
	var count int
	err = s.queryRow(ctx,
		`SELECT COUNT(*) FROM scheduled_alerts
		 WHERE learner_id = ? AND alert_type = ?
		   AND created_at >= ? AND created_at < ?`,
		learnerID, alertType, bounds.DayStart, bounds.DayEnd,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check alert sent today: %w", err)
	}
	return count > 0, nil
}

// ─── Stats for Scheduler ─────────────────────────────────────────────────────

// GetDailyStreak returns how many consecutive days the learner has had interactions.
func (s *Store) GetDailyStreak(ctx context.Context, learnerID string) (int, error) {
	rows, err := s.query(ctx,
		`SELECT DISTINCT `+s.utcDateExpr("created_at")+` as d FROM interactions
		 WHERE learner_id = ? ORDER BY d DESC`,
		learnerID,
	)
	if err != nil {
		return 0, fmt.Errorf("get daily streak: %w", err)
	}
	defer rows.Close()

	streak := 0
	expected := time.Now().UTC().Truncate(24 * time.Hour)
	for rows.Next() {
		var dateStr string
		if err := rows.Scan(&dateStr); err != nil {
			return 0, fmt.Errorf("scan daily streak: %w", err)
		}
		d, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			return 0, fmt.Errorf("parse daily streak date %q: %w", dateStr, err)
		}
		// Allow today or yesterday to start the streak
		if streak == 0 {
			diff := expected.Sub(d).Hours() / 24
			if diff > 1 {
				return 0, nil // last activity was more than 1 day ago
			}
			streak = 1
			expected = d.AddDate(0, 0, -1)
			continue
		}
		if d.Equal(expected) {
			streak++
			expected = d.AddDate(0, 0, -1)
		} else {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate daily streak: %w", err)
	}
	return streak, nil
}

// GetTodayInteractionCount returns the number of interactions today.
func (s *Store) GetTodayInteractionCount(ctx context.Context, learnerID string) (int, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var count int
	err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM interactions WHERE learner_id = ? AND created_at >= ?`,
		learnerID, today,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get today interactions: %w", err)
	}
	return count, nil
}

// GetTodaySuccessRate returns success rate for today's interactions.
func (s *Store) GetTodaySuccessRate(ctx context.Context, learnerID string) (float64, int, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var total, successes int
	err := s.queryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(success), 0) FROM interactions
		 WHERE learner_id = ? AND created_at >= ?`,
		learnerID, today,
	).Scan(&total, &successes)
	if err != nil {
		return 0, 0, fmt.Errorf("get today success rate: %w", err)
	}
	if total == 0 {
		return 0, 0, nil
	}
	return float64(successes) / float64(total), total, nil
}

// GetConceptsDueForReview returns concepts where next_review is in the past.
func (s *Store) GetConceptsDueForReview(ctx context.Context, learnerID string) ([]string, error) {
	return s.getConceptsDueForReview(ctx, learnerID, "")
}

func (s *Store) GetConceptsDueForReviewByDomain(ctx context.Context, learnerID, domainID string) ([]string, error) {
	return s.getConceptsDueForReview(ctx, learnerID, domainID)
}

func (s *Store) getConceptsDueForReview(ctx context.Context, learnerID, domainID string) ([]string, error) {
	now := time.Now().UTC()
	query := `SELECT concept FROM concept_states
		 WHERE learner_id = ? AND next_review IS NOT NULL AND next_review <= ? AND card_state != 'new'`
	args := []any{learnerID, now}
	if domainID != "" {
		query += ` AND domain_id = ?`
		args = append(args, domainID)
	}
	query += ` ORDER BY next_review ASC`
	rows, err := s.query(ctx,
		query,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("get concepts due for review: %w", err)
	}
	defer rows.Close()

	var concepts []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan concept due for review: %w", err)
		}
		concepts = append(concepts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate concepts due for review: %w", err)
	}

	if domainID != "" {
		// Ownership/archive validation is done by the caller when resolving
		// the domain; an exact domain query cannot collide on a shared label.
		return concepts, nil
	}

	// Filter out concepts whose domain is archived or deleted.
	// ActiveDomainConceptSet returns concepts from non-archived, non-deleted domains only.
	activeSet, err := s.ActiveDomainConceptSet(ctx, learnerID)
	if err != nil {
		return nil, fmt.Errorf("get concepts due for review: filter active: %w", err)
	}
	filtered := concepts[:0]
	for _, c := range concepts {
		if activeSet[c] {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

// ─── OAuth Persistence ──────────────────────────────────────────────────────

// CreateAuthCodeWithBinding persists the redirect URI and PKCE method used at
// authorization time. Empty method/redirect values are accepted only so tests
// can exercise rows created before those columns existed.
func (s *Store) CreateAuthCodeWithBinding(ctx context.Context, code, learnerID, codeChallenge, codeChallengeMethod, clientID, redirectURI, resource string, expiresAt time.Time) error {
	return s.CreateAuthCodeWithBindingAndScope(
		ctx, code, learnerID, codeChallenge, codeChallengeMethod, clientID, redirectURI,
		resource, models.OAuthScopeLearner, expiresAt,
	)
}

func (s *Store) CreateAuthCodeWithBindingAndScope(ctx context.Context, code, learnerID, codeChallenge, codeChallengeMethod, clientID, redirectURI, resource, scope string, expiresAt time.Time) error {
	canonicalScope, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		return fmt.Errorf("create auth code: %w", store.ErrInvalidOAuthScope)
	}
	principal, err := s.credentialPrincipalForLearner(ctx, learnerID, strings.Fields(canonicalScope))
	if err != nil {
		return fmt.Errorf("create auth code: %w", store.ErrInvalidPrincipal)
	}
	return s.CreateAuthCodeForPrincipal(ctx, code, principal, codeChallenge, codeChallengeMethod, clientID, redirectURI, resource, canonicalScope, expiresAt)
}

func (s *Store) CreateAuthCodeForPrincipal(ctx context.Context, code string, principal models.Principal, codeChallenge, codeChallengeMethod, clientID, redirectURI, resource, scope string, expiresAt time.Time) error {
	if strings.TrimSpace(resource) == "" {
		return fmt.Errorf("create auth code: resource is required")
	}
	canonicalScope, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		return fmt.Errorf("create auth code: %w", store.ErrInvalidOAuthScope)
	}
	principal.Scopes = strings.Fields(canonicalScope)
	if err := principal.Validate(); err != nil {
		return fmt.Errorf("create auth code: %w", store.ErrInvalidPrincipal)
	}
	if !s.inTenantTransaction(ctx, principal.TenantScope()) && s.root != nil {
		return s.WithTenantTx(ctx, principal.TenantScope(), func(txCtx context.Context, scoped store.Store) error {
			return scoped.CreateAuthCodeForPrincipal(txCtx, code, principal, codeChallenge, codeChallengeMethod,
				clientID, redirectURI, resource, canonicalScope, expiresAt)
		})
	}
	createdAt := time.Now().UTC()
	_, err = s.exec(ctx,
		`INSERT INTO oauth_codes
		    (code, tenant_id, user_id, membership_id, membership_version, learner_id,
		     code_challenge, code_challenge_method, client_id, redirect_uri, resource, scope, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		code, principal.TenantID, principal.UserID, principal.MembershipID, principal.TokenVersion,
		principal.LearnerID, codeChallenge, codeChallengeMethod, clientID, redirectURI,
		resource, canonicalScope, expiresAt, createdAt,
	)
	if err != nil {
		return fmt.Errorf("create auth code: %w", err)
	}
	if err := s.insertCredentialRoute(ctx, credentialKindAuthorizationCode, code, principal.TenantScope(), expiresAt, createdAt); err != nil {
		return fmt.Errorf("create auth code: %w", err)
	}
	return nil
}

// GetAuthCode reads a still-valid code without consuming it. The token handler
// uses this view to validate PKCE and redirect binding before performing the
// atomic single-use deletion, so a bad verifier cannot burn a legitimate code.
func (s *Store) GetAuthCode(ctx context.Context, code, clientID string) (*models.AuthCode, error) {
	scope, routeErr := s.credentialScope(ctx, credentialKindAuthorizationCode, code)
	if routeErr != nil {
		return nil, fmt.Errorf("get auth code: %w", store.ErrInvalidAuthCode)
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var authCode *models.AuthCode
		err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped store.Store) error {
			var innerErr error
			authCode, innerErr = scoped.GetAuthCode(txCtx, code, clientID)
			return innerErr
		})
		return authCode, err
	}
	ac := &models.AuthCode{}
	err := s.queryRow(ctx,
		`SELECT code, user_id, tenant_id, membership_id, membership_version, learner_id,
		        code_challenge, code_challenge_method, client_id, redirect_uri, resource, scope, expires_at
		 FROM oauth_codes
		 WHERE code = ? AND tenant_id = ? AND user_id = ? AND membership_id = ?
		   AND learner_id = ? AND client_id = ? AND expires_at > ?
		   AND EXISTS (
		       SELECT 1 FROM learners l
		       JOIN tenant_memberships tm
		         ON tm.tenant_id = l.tenant_id AND tm.id = l.membership_id
		        AND tm.user_id = l.user_id AND tm.status = 'active'
		       WHERE l.tenant_id = oauth_codes.tenant_id
		         AND l.id = oauth_codes.learner_id
		         AND l.email_verified_at IS NOT NULL
		   )`,
		code, scope.TenantID, scope.UserID, scope.MembershipID, scope.LearnerID,
		clientID, time.Now().UTC(),
	).Scan(&ac.Code, &ac.UserID, &ac.TenantID, &ac.MembershipID, &ac.MembershipVersion,
		&ac.LearnerID, &ac.CodeChallenge, &ac.CodeChallengeMethod, &ac.ClientID,
		&ac.RedirectURI, &ac.Resource, &ac.Scope, &ac.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get auth code: %w", store.ErrInvalidAuthCode)
	}
	if err != nil {
		return nil, fmt.Errorf("get auth code: %w", err)
	}
	return ac, nil
}

// ConsumeAuthCode retrieves and deletes an auth code in one SQL statement.
// DELETE ... RETURNING is the single-use boundary: concurrent exchanges cannot
// both observe the code under PostgreSQL READ COMMITTED (or SQLite).
// Binds the code to the requesting client_id: returns invalid_grant if mismatch.
func (s *Store) ConsumeAuthCode(ctx context.Context, code, clientID string) (*models.AuthCode, error) {
	scope, routeErr := s.credentialScope(ctx, credentialKindAuthorizationCode, code)
	if routeErr != nil {
		return nil, fmt.Errorf("consume auth code: %w", store.ErrInvalidAuthCode)
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var authCode *models.AuthCode
		err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped store.Store) error {
			var innerErr error
			authCode, innerErr = scoped.ConsumeAuthCode(txCtx, code, clientID)
			return innerErr
		})
		return authCode, err
	}
	ac := &models.AuthCode{}
	err := s.queryRow(ctx,
		`DELETE FROM oauth_codes
		 WHERE code = ? AND tenant_id = ? AND user_id = ? AND membership_id = ?
		   AND learner_id = ? AND client_id = ? AND expires_at > ?
		   AND EXISTS (
		       SELECT 1 FROM learners l
		       JOIN tenant_memberships tm
		         ON tm.tenant_id = l.tenant_id AND tm.id = l.membership_id
		        AND tm.user_id = l.user_id AND tm.status = 'active'
		       WHERE l.tenant_id = oauth_codes.tenant_id
		         AND l.id = oauth_codes.learner_id
		         AND l.email_verified_at IS NOT NULL
		   )
		 RETURNING code, user_id, tenant_id, membership_id, membership_version, learner_id,
		           code_challenge, code_challenge_method, client_id, redirect_uri, resource, scope, expires_at`,
		code, scope.TenantID, scope.UserID, scope.MembershipID, scope.LearnerID,
		clientID, time.Now().UTC(),
	).Scan(&ac.Code, &ac.UserID, &ac.TenantID, &ac.MembershipID, &ac.MembershipVersion,
		&ac.LearnerID, &ac.CodeChallenge, &ac.CodeChallengeMethod, &ac.ClientID,
		&ac.RedirectURI, &ac.Resource, &ac.Scope, &ac.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("consume auth code: %w", store.ErrInvalidAuthCode)
	}
	if err != nil {
		return nil, fmt.Errorf("consume auth code: %w", err)
	}
	if err := s.deleteCredentialRoute(ctx, credentialKindAuthorizationCode, code); err != nil {
		return nil, fmt.Errorf("consume auth code: %w", err)
	}
	return ac, nil
}

// ExchangeAuthCodeForRefreshToken consumes a validated authorization code,
// creates its client-bound refresh token, and activates a persisted dynamic
// client in one transaction. A persistence failure therefore leaves the code
// redeemable and the client expiry unchanged. The activation UPDATE accepts
// zero rows because CIMD client metadata is not persisted locally.
func (s *Store) ExchangeAuthCodeForRefreshToken(ctx context.Context, code, clientID string) (*models.AuthCode, *models.RefreshToken, error) {
	if strings.TrimSpace(clientID) == "" {
		return nil, nil, fmt.Errorf("exchange auth code: client_id is required")
	}
	scope, routeErr := s.credentialScope(ctx, credentialKindAuthorizationCode, code)
	if routeErr != nil {
		return nil, nil, fmt.Errorf("exchange auth code: %w", store.ErrInvalidAuthCode)
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var authCode *models.AuthCode
		var refreshToken *models.RefreshToken
		err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped store.Store) error {
			var innerErr error
			authCode, refreshToken, innerErr = scoped.ExchangeAuthCodeForRefreshToken(txCtx, code, clientID)
			return innerErr
		})
		return authCode, refreshToken, err
	}
	rt, err := newRefreshToken("", clientID, "", models.OAuthScopeLearner, "")
	if err != nil {
		return nil, nil, err
	}
	var authCode *models.AuthCode
	err = s.inTx(ctx, nil, func(txs *Store) error {
		var consumeErr error
		authCode, consumeErr = txs.ConsumeAuthCode(ctx, code, clientID)
		if consumeErr != nil {
			return consumeErr
		}
		rt.LearnerID = authCode.LearnerID
		rt.UserID = authCode.UserID
		rt.TenantID = authCode.TenantID
		rt.MembershipID = authCode.MembershipID
		rt.MembershipVersion = authCode.MembershipVersion
		rt.Resource = authCode.Resource
		rt.Scope = authCode.Scope
		if err := txs.insertRefreshToken(ctx, rt); err != nil {
			return fmt.Errorf("create refresh token: %w", err)
		}
		// Zero rows is valid for a CIMD client, whose metadata is deliberately
		// not persisted in oauth_clients. Static/already-active rows and live
		// DCR rows are equally safe, making this mutation idempotent.
		_, err := txs.exec(ctx,
			`UPDATE oauth_clients SET expires_at = NULL WHERE client_id = ?`,
			clientID,
		)
		if err != nil {
			return fmt.Errorf("activate oauth client: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("exchange auth code: %w", err)
	}
	return authCode, rt, nil
}

func (s *Store) CreateOAuthClient(ctx context.Context, clientID, clientName, redirectURIs string) error {
	return s.CreateOAuthClientWithSecret(ctx, clientID, clientName, redirectURIs, "")
}

// CreateOAuthClientWithSecret persists a confidential client when secretHash != "".
// secretHash should be a bcrypt digest of the issued secret; pass "" for public (PKCE) clients.
func (s *Store) CreateOAuthClientWithSecret(ctx context.Context, clientID, clientName, redirectURIs, secretHash string) error {
	_, err := s.exec(ctx,
		`INSERT INTO oauth_clients (client_id, client_name, redirect_uris, client_secret_hash) VALUES (?, ?, ?, ?)`,
		clientID, clientName, redirectURIs, secretHash,
	)
	if err != nil {
		return fmt.Errorf("create oauth client: %w", err)
	}
	return nil
}

// CreateOAuthClientWithSecretCapped persists a client only if the current row
// count is still below maxClients. maxClients <= 0 disables the cap.
func (s *Store) CreateOAuthClientWithSecretCapped(ctx context.Context, clientID, clientName, redirectURIs, secretHash string, maxClients int) error {
	return s.createOAuthClientWithSecretCapped(ctx, clientID, clientName, redirectURIs, secretHash, maxClients, nil)
}

func (s *Store) CreateOAuthClientWithSecretCappedTTL(ctx context.Context, clientID, clientName, redirectURIs, secretHash string, maxClients int, expiresAt time.Time) error {
	if expiresAt.IsZero() {
		return fmt.Errorf("create oauth client: expires_at is required")
	}
	return s.createOAuthClientWithSecretCapped(ctx, clientID, clientName, redirectURIs, secretHash, maxClients, &expiresAt)
}

func (s *Store) createOAuthClientWithSecretCapped(ctx context.Context, clientID, clientName, redirectURIs, secretHash string, maxClients int, expiresAt *time.Time) error {
	if maxClients <= 0 {
		_, err := s.exec(ctx,
			`INSERT INTO oauth_clients (client_id, client_name, redirect_uris, client_secret_hash, expires_at)
			 VALUES (?, ?, ?, ?, ?)`,
			clientID, clientName, redirectURIs, secretHash, expiresAt,
		)
		return err
	}
	return s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		if txs.dialect == DialectPostgres {
			// COUNT + INSERT must share a serialized critical section. Under
			// PostgreSQL READ COMMITTED, concurrent INSERT...WHERE COUNT
			// statements can otherwise all observe the same remaining slot.
			if _, err := txs.exec(ctx, `SELECT pg_advisory_xact_lock(?)`, int64(0x7475746f72444352)); err != nil {
				return fmt.Errorf("lock oauth client registration: %w", err)
			}
		}
		result, err := txs.exec(ctx,
			`INSERT INTO oauth_clients (client_id, client_name, redirect_uris, client_secret_hash, expires_at)
			 SELECT ?, ?, ?, ?, ?
			 WHERE (SELECT COUNT(*) FROM oauth_clients) < ?`,
			clientID, clientName, redirectURIs, secretHash, expiresAt, maxClients,
		)
		if err != nil {
			return fmt.Errorf("create oauth client: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("create oauth client rows affected: %w", err)
		}
		if rows == 0 {
			return store.ErrOAuthClientLimitReached
		}
		return nil
	})
}

func (s *Store) CountOAuthClients(ctx context.Context) (int, error) {
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count oauth clients: %w", err)
	}
	return count, nil
}

func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (*models.OAuthClient, error) {
	c := &models.OAuthClient{}
	var secretHash sql.NullString
	var expiresAt sql.NullTime
	err := s.queryRow(ctx,
		`SELECT client_id, client_name, redirect_uris, client_secret_hash, expires_at
		 FROM oauth_clients
		 WHERE client_id = ? AND (expires_at IS NULL OR expires_at > ?)`,
		clientID, time.Now().UTC(),
	).Scan(&c.ClientID, &c.ClientName, &c.RedirectURIs, &secretHash, &expiresAt)
	if err != nil {
		return nil, fmt.Errorf("get oauth client: %w", err)
	}
	if secretHash.Valid {
		c.ClientSecretHash = secretHash.String
	}
	if expiresAt.Valid {
		ts := expiresAt.Time
		c.ExpiresAt = &ts
	}
	return c, nil
}

// CleanupExpiredOAuthClients removes only ephemeral DCR clients. Long-lived
// preregistered clients have NULL expires_at and are never selected. Related
// credentials are invalidated first so no orphan can remain redeemable.
func (s *Store) CleanupExpiredOAuthClients(ctx context.Context) (int64, error) {
	var removed int64
	now := time.Now().UTC()
	err := s.inTx(ctx, nil, func(txs *Store) error {
		rows, err := txs.query(ctx,
			`SELECT client_id, registration_token_id
			 FROM oauth_clients WHERE expires_at IS NOT NULL AND expires_at <= ?
			 ORDER BY client_id`, now,
		)
		if err != nil {
			return fmt.Errorf("list expired oauth clients: %w", err)
		}
		type expiredClient struct{ clientID, tokenID string }
		var expired []expiredClient
		for rows.Next() {
			var item expiredClient
			if err := rows.Scan(&item.clientID, &item.tokenID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan expired oauth client: %w", err)
			}
			expired = append(expired, item)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close expired oauth clients: %w", err)
		}
		for _, item := range expired {
			if err := txs.appendDCRAudit(
				ctx, "client_expired_deleted", "scheduler-cleanup",
				item.tokenID, item.clientID, map[string]any{"reason": "unused registration TTL elapsed"}, now,
			); err != nil {
				return err
			}
		}
		if _, err := txs.exec(ctx,
			`UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, ?)
			 WHERE client_id IN (SELECT client_id FROM oauth_clients WHERE expires_at IS NOT NULL AND expires_at <= ?)`,
			now, now,
		); err != nil {
			return err
		}
		for _, table := range []string{"oauth_codes", "learner_approved_clients"} {
			if _, err := txs.exec(ctx,
				`DELETE FROM `+table+` WHERE client_id IN
				 (SELECT client_id FROM oauth_clients WHERE expires_at IS NOT NULL AND expires_at <= ?)`,
				now,
			); err != nil {
				return err
			}
		}
		result, err := txs.exec(ctx, `DELETE FROM oauth_clients WHERE expires_at IS NOT NULL AND expires_at <= ?`, now)
		if err != nil {
			return err
		}
		removed, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("cleanup expired oauth clients: %w", err)
	}
	return removed, nil
}

// IsClientApproved reports whether the learner has previously consented to the
// given OAuth client for this exact redirect_uri. R001: the consent screen is
// only meaningful when there's something to approve — once a (learner, client,
// redirect_uri) triple is on file, the next /authorize POST may skip the
// approve_client prompt. A different redirect_uri (even on the same client)
// re-prompts because the approval is scoped to redirect_uri, not client_id.
func (s *Store) IsClientApproved(ctx context.Context, learnerID, clientID, redirectURI string) (bool, error) {
	return s.IsClientApprovedForScope(ctx, learnerID, clientID, redirectURI, models.OAuthScopeLearner)
}

func (s *Store) IsClientApprovedForScope(ctx context.Context, learnerID, clientID, redirectURI, scope string) (bool, error) {
	canonicalScope, scopeErr := models.CanonicalOAuthScope(scope)
	if scopeErr != nil || canonicalScope != scope {
		return false, fmt.Errorf("is client approved: %w", store.ErrInvalidOAuthScope)
	}
	var one int
	err := s.queryRow(ctx,
		`SELECT 1 FROM learner_approved_clients
		 WHERE learner_id = ? AND client_id = ? AND redirect_uri = ? AND scope = ?`,
		learnerID, clientID, redirectURI, canonicalScope,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is client approved: %w", err)
	}
	return true, nil
}

// ApproveClient records that the learner has consented to the OAuth client for
// this exact redirect_uri. Idempotent: re-approving the same triple is a no-op
// (ON CONFLICT DO NOTHING preserves the original approved_at timestamp). R001.
func (s *Store) ApproveClient(ctx context.Context, learnerID, clientID, redirectURI string) error {
	return s.ApproveClientForScope(ctx, learnerID, clientID, redirectURI, models.OAuthScopeLearner)
}

func (s *Store) ApproveClientForScope(ctx context.Context, learnerID, clientID, redirectURI, scope string) error {
	canonicalScope, scopeErr := models.CanonicalOAuthScope(scope)
	if scopeErr != nil || canonicalScope != scope {
		return fmt.Errorf("approve client: %w", store.ErrInvalidOAuthScope)
	}
	_, err := s.exec(ctx,
		`INSERT INTO learner_approved_clients (learner_id, client_id, redirect_uri, scope)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(learner_id, client_id, redirect_uri)
		 DO UPDATE SET scope = excluded.scope, approved_at = CURRENT_TIMESTAMP`,
		learnerID, clientID, redirectURI, canonicalScope,
	)
	if err != nil {
		return fmt.Errorf("approve client: %w", err)
	}
	return nil
}

func (s *Store) CleanupExpiredCodes(ctx context.Context) (int64, error) {
	result, err := s.exec(ctx, `DELETE FROM oauth_codes WHERE expires_at < ?`, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("cleanup expired codes: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) CleanupExpiredRefreshTokens(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	// Keep expired ancestors while a descendant in the same family remains
	// live: their hashes are the evidence needed to detect a replay and revoke
	// that descendant. Once the whole family has expired, all rows are eligible.
	result, err := s.exec(ctx,
		`DELETE FROM refresh_tokens
		 WHERE expires_at < ?
		   AND (
		       family_id = ''
		       OR NOT EXISTS (
		           SELECT 1 FROM refresh_tokens AS active
		           WHERE active.family_id = refresh_tokens.family_id
		             AND active.expires_at >= ?
		       )
		   )`,
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("cleanup expired refresh tokens: %w", err)
	}
	return result.RowsAffected()
}

// GetActivityStreak returns the number of consecutive UTC days, ending today,
// during which the learner has at least one interaction. Returns 0 if the
// learner has no interaction today.
//
// Implementation: pulls all distinct interaction days (DESC), then counts
// from today backwards until the first gap.
func (s *Store) GetActivityStreak(ctx context.Context, learnerID string) (int, error) {
	// substr — modernc/sqlite stores time.Time as RFC3339 with nanoseconds, on
	// which SQLite's date() returns NULL. substr reliably extracts YYYY-MM-DD.
	rows, err := s.query(ctx,
		`SELECT DISTINCT `+s.utcDateExpr("created_at")+` AS d
		 FROM interactions
		 WHERE learner_id = ?
		 ORDER BY d DESC`,
		learnerID,
	)
	if err != nil {
		return 0, fmt.Errorf("get activity streak: %w", err)
	}
	defer rows.Close()

	var days []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return 0, fmt.Errorf("scan streak day: %w", err)
		}
		days = append(days, d)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("get activity streak rows: %w", err)
	}
	if len(days) == 0 {
		return 0, nil
	}

	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	if days[0] != today {
		return 0, nil
	}

	streak := 1
	for i := 1; i < len(days); i++ {
		expected := now.AddDate(0, 0, -i).Format("2006-01-02")
		if days[i] != expected {
			break
		}
		streak++
	}
	return streak, nil
}

// fragileThreshold mirrors the engine.NodeClassify fragile boundary. The
// mastered-cutoff was previously a duplicate const here (mirrored from
// algorithms.KSTMasteryThreshold = 0.70), but it now reads through
// algorithms.MasteryKST() at the call site to honour REGULATION_THRESHOLD.
// The "db can't import algorithms" comment that lived here was outdated:
// algorithms is a leaf package and motivation_queries.go already imports it.
const fragileThreshold = 0.30

// GetRecentLearnerEvents returns a chronological-DESC list of cognitive events
// since `since`, surfaced through GlobalOLMSnapshot.RecentEvents. Events are
// derived from existing tables — no new persistence:
//
//   - estimate_threshold : concept_state.p_mastery >= the routing threshold and updated_at >= since
//   - estimate_fragile   : p_mastery currently < 0.30 (a routing snapshot, not a retention event,
//     since the query sees a snapshot, not a transition) and updated_at >= since
//   - streak_start      : earliest interaction in a still-running streak (computed via GetActivityStreak)
//
// (calibration_threshold derivable from calibration_history but skipped in v1
// to keep the query simple — covered by the calibration sparkline.)
func (s *Store) GetRecentLearnerEvents(ctx context.Context, learnerID string, since time.Time) ([]models.RawLearnerEvent, error) {
	var events []models.RawLearnerEvent

	rows, err := s.query(ctx,
		`SELECT concept, p_mastery, updated_at
		 FROM concept_states
		 WHERE learner_id = ? AND updated_at >= ?
		 ORDER BY updated_at DESC`,
		learnerID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("get learner events: %w", err)
	}
	defer rows.Close()
	masteryKST := algorithms.MasteryKST()
	for rows.Next() {
		var concept string
		var mastery float64
		var at time.Time
		if err := rows.Scan(&concept, &mastery, &at); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		if mastery >= masteryKST {
			events = append(events, models.RawLearnerEvent{
				At: at, Kind: "estimate_threshold", Concept: concept,
				Message: fmt.Sprintf("%s model estimate reached the routing threshold", concept),
			})
		} else if mastery < fragileThreshold {
			events = append(events, models.RawLearnerEvent{
				At: at, Kind: "estimate_fragile", Concept: concept,
				Message: fmt.Sprintf("%s model estimate entered the fragile routing band", concept),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get learner events rows: %w", err)
	}

	if streak, _ := s.GetActivityStreak(ctx, learnerID); streak > 0 {
		startedAt := time.Now().UTC().AddDate(0, 0, -streak+1).Truncate(24 * time.Hour)
		if !startedAt.Before(since) {
			events = append(events, models.RawLearnerEvent{
				At: startedAt, Kind: "streak_start",
				Message: fmt.Sprintf("%d-day streak started", streak),
			})
		}
	}

	sort.Slice(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	return events, nil
}
