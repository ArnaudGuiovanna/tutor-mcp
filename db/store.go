// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
	"tutor-mcp/store"
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

// IsSafeWebhookURL validates that a webhook URL targets Discord over HTTPS.
// SSRF guard: only Discord webhook hosts allowed (blocks IMDS, internal ranges, etc.).
func IsSafeWebhookURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.Contains(host, "..") {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	host = strings.ToLower(host)
	switch host {
	case "discord.com", "discordapp.com":
		return true
	}
	return strings.HasSuffix(host, ".discord.com") || strings.HasSuffix(host, ".discordapp.com")
}

type Store struct {
	db   sqlExecutor // *sql.DB normally; *sql.Tx inside WithTx — query methods use this unchanged
	root *sql.DB     // the real pool. For WithTx/Ping/Close/Migrate + internal BeginTx. nil in a tx-scoped Store.
}

var ErrOAuthClientLimitReached = errors.New("oauth client limit reached")

// RawDB returns the underlying *sql.DB. Intended for tests that need
// to insert with explicit timestamps (e.g. simulating older
// interactions for diagnostic-window assertions). Not for use in
// production code paths — use the typed Store methods instead.
func (s *Store) RawDB() *sql.DB { return s.root }

func NewStore(database *sql.DB) *Store {
	return &Store{db: database, root: database}
}

// Ping verifies a live connection to the database.
func (s *Store) Ping(ctx context.Context) error { return s.root.PingContext(ctx) }

// Close closes the underlying connection pool.
func (s *Store) Close() error { return s.root.Close() }

// Migrate runs the schema migrations. (Wraps the package-level Migrate.)
func (s *Store) Migrate(ctx context.Context) error { return Migrate(s.root) }

// WithTx runs fn inside a single serializable transaction (BEGIN IMMEDIATE on
// SQLite). The Store passed to fn routes every query through the tx.
//
// Serializable isolation translates to BEGIN IMMEDIATE on modernc.org/sqlite,
// which acquires the writer lock at tx start instead of at first write. Required
// for read-modify-write patterns where concurrent goroutines would otherwise
// both read a stale snapshot and one overwrite the other on commit (R008).
func (s *Store) WithTx(ctx context.Context, fn func(s store.Store) error) error {
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	if err := fn(&Store{db: tx, root: nil}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// GetConceptStateForUpdate is identical to GetConceptState under SQLite (the
// serializable tx already holds the writer lock via BEGIN IMMEDIATE). A future
// PostgresStore overrides this with SELECT ... FOR UPDATE.
func (s *Store) GetConceptStateForUpdate(ctx context.Context, learnerID, concept string) (*models.ConceptState, error) {
	return s.GetConceptState(ctx, learnerID, concept)
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
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
	if webhookURL != "" && !IsSafeWebhookURL(webhookURL) {
		return nil, fmt.Errorf("invalid webhook_url: must be https://discord.com/...")
	}
	id := generateID()
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO learners (id, email, password_hash, objective, webhook_url, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, email, passwordHash, objective, webhookURL, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create learner: %w", err)
	}
	return &models.Learner{
		ID:           id,
		Email:        email,
		PasswordHash: passwordHash,
		Objective:    objective,
		WebhookURL:   webhookURL,
		CreatedAt:    now,
	}, nil
}

func (s *Store) GetLearnerByID(ctx context.Context, id string) (*models.Learner, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, objective, webhook_url, profile_json, created_at, last_active
		 FROM learners WHERE id = ?`, id,
	)
	return scanLearner(row)
}

func (s *Store) GetLearnerByEmail(ctx context.Context, email string) (*models.Learner, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, objective, webhook_url, profile_json, created_at, last_active
		 FROM learners WHERE email = ?`, email,
	)
	return scanLearner(row)
}

func scanLearner(row *sql.Row) (*models.Learner, error) {
	l := &models.Learner{}
	var lastActive sql.NullTime
	var profileJSON sql.NullString
	err := row.Scan(
		&l.ID, &l.Email, &l.PasswordHash, &l.Objective, &l.WebhookURL, &profileJSON, &l.CreatedAt, &lastActive,
	)
	if err != nil {
		return nil, fmt.Errorf("scan learner: %w", err)
	}
	if lastActive.Valid {
		l.LastActive = lastActive.Time
	}
	if profileJSON.Valid {
		l.ProfileJSON = profileJSON.String
	} else {
		l.ProfileJSON = "{}"
	}
	return l, nil
}

func (s *Store) UpdateLastActive(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE learners SET last_active = ? WHERE id = ?`,
		time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update last active: %w", err)
	}
	return nil
}

func (s *Store) GetActiveLearners(ctx context.Context) ([]*models.Learner, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, email, password_hash, objective, webhook_url, profile_json, created_at, last_active
		 FROM learners WHERE webhook_url != ''`,
	)
	if err != nil {
		return nil, fmt.Errorf("get active learners: %w", err)
	}
	defer rows.Close()

	var learners []*models.Learner
	for rows.Next() {
		l := &models.Learner{}
		var lastActive sql.NullTime
		var profileJSON sql.NullString
		if err := rows.Scan(
			&l.ID, &l.Email, &l.PasswordHash, &l.Objective, &l.WebhookURL, &profileJSON, &l.CreatedAt, &lastActive,
		); err != nil {
			return nil, fmt.Errorf("scan learner row: %w", err)
		}
		if lastActive.Valid {
			l.LastActive = lastActive.Time
		}
		if profileJSON.Valid {
			l.ProfileJSON = profileJSON.String
		} else {
			l.ProfileJSON = "{}"
		}
		learners = append(learners, l)
	}
	return learners, rows.Err()
}

func (s *Store) UpdateLearnerProfile(ctx context.Context, learnerID, profileJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE learners SET profile_json = ? WHERE id = ?`,
		profileJSON, learnerID,
	)
	if err != nil {
		return fmt.Errorf("update learner profile: %w", err)
	}
	return nil
}

// ─── Refresh Tokens ───────────────────────────────────────────────────────────

// CreateRefreshToken issues a refresh token bound to (learnerID, clientID).
// clientID may be empty for legacy callers — issue #30 part 2 introduces the
// binding so a stolen token cannot be redeemed by a different (e.g. self-
// registered confidential) client. Pre-existing rows have NULL client_id and
// the refresh-grant handler treats NULL as "any client" for backward compat.
func (s *Store) CreateRefreshToken(ctx context.Context, learnerID, clientID string) (*models.RefreshToken, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	token := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
	now := time.Now().UTC()
	expiresAt := now.Add(30 * 24 * time.Hour)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO refresh_tokens (token, learner_id, client_id, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		token, learnerID, nullString(clientID), expiresAt, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create refresh token: %w", err)
	}
	return &models.RefreshToken{
		Token:     token,
		LearnerID: learnerID,
		ClientID:  clientID,
		ExpiresAt: expiresAt,
		CreatedAt: now,
	}, nil
}

func (s *Store) GetRefreshToken(ctx context.Context, token string) (*models.RefreshToken, error) {
	rt := &models.RefreshToken{}
	var clientID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT token, learner_id, client_id, expires_at, created_at
		 FROM refresh_tokens WHERE token = ? AND expires_at > ?`,
		token, time.Now().UTC(),
	).Scan(&rt.Token, &rt.LearnerID, &clientID, &rt.ExpiresAt, &rt.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get refresh token: %w", err)
	}
	if clientID.Valid {
		rt.ClientID = clientID.String
	}
	return rt, nil
}

func (s *Store) DeleteRefreshToken(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE token = ?`, token)
	if err != nil {
		return fmt.Errorf("delete refresh token: %w", err)
	}
	return nil
}

// ─── Domains ──────────────────────────────────────────────────────────────────

func (s *Store) CreateDomain(ctx context.Context, learnerID, name, personalGoal string, graph models.KnowledgeSpace) (*models.Domain, error) {
	return s.CreateDomainWithValueFramings(ctx, learnerID, name, personalGoal, graph, "")
}

// CreateDomainWithValueFramings creates a domain and optionally persists a JSON-encoded
// set of value framings (4 axes: financial, employment, intellectual, innovation).
func (s *Store) CreateDomainWithValueFramings(ctx context.Context, learnerID, name, personalGoal string, graph models.KnowledgeSpace, valueFramingsJSON string) (*models.Domain, error) {
	id := generateID()
	now := time.Now().UTC()

	graphJSON, err := json.Marshal(graph)
	if err != nil {
		return nil, fmt.Errorf("marshal graph: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO domains (id, learner_id, name, personal_goal, graph_json, value_framings_json, graph_version, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		id, learnerID, name, personalGoal, string(graphJSON), valueFramingsJSON, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create domain: %w", err)
	}
	return &models.Domain{
		ID:                id,
		LearnerID:         learnerID,
		Name:              name,
		PersonalGoal:      personalGoal,
		Graph:             graph,
		ValueFramingsJSON: valueFramingsJSON,
		GraphVersion:      1,
		CreatedAt:         now,
	}, nil
}

const domainCols = `id, learner_id, name, personal_goal, graph_json, value_framings_json, last_value_axis, archived, priority_rank, graph_version, goal_relevance_json, goal_relevance_version, phase, phase_changed_at, phase_entry_entropy, created_at`

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
	var phaseEntryEntropy sql.NullFloat64
	var priorityRank sql.NullInt64
	var archived int
	err := s.Scan(
		&d.ID, &d.LearnerID, &d.Name, &d.PersonalGoal, &graphJSON,
		&valueFramings, &lastAxis, &archived,
		&priorityRank, &d.GraphVersion, &goalRelevanceJSON, &d.GoalRelevanceVersion,
		&phase, &phaseChangedAt, &phaseEntryEntropy,
		&d.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	d.Archived = archived != 0
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
	row := s.db.QueryRowContext(ctx,
		`SELECT `+domainCols+` FROM domains WHERE learner_id = ? AND archived = 0
		 ORDER BY CASE WHEN priority_rank IS NULL THEN 1 ELSE 0 END, priority_rank ASC, created_at DESC LIMIT 1`,
		learnerID,
	)
	d, err := scanDomainRow(row)
	if err != nil {
		return nil, fmt.Errorf("get domain by learner: %w", err)
	}
	return d, nil
}

func (s *Store) GetDomainByID(ctx context.Context, id string) (*models.Domain, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+domainCols+` FROM domains WHERE id = ?`, id)
	d, err := scanDomainRow(row)
	if err != nil {
		return nil, fmt.Errorf("get domain by id: %w", err)
	}
	return d, nil
}

func (s *Store) GetDomainsByLearner(ctx context.Context, learnerID string, includeArchived bool) ([]*models.Domain, error) {
	query := `SELECT ` + domainCols + ` FROM domains WHERE learner_id = ?`
	if !includeArchived {
		query += ` AND archived = 0`
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, learnerID)
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
	result, err := s.db.ExecContext(ctx,
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

// UpdateDomainValueFramings stores the JSON-encoded value framings for a domain.
func (s *Store) UpdateDomainValueFramings(ctx context.Context, domainID, valueFramingsJSON string) error {
	_, err := s.db.ExecContext(ctx,
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
	_, err := s.db.ExecContext(ctx,
		`UPDATE domains SET last_value_axis = ? WHERE id = ?`,
		axis, domainID,
	)
	if err != nil {
		return fmt.Errorf("update domain last value axis: %w", err)
	}
	return nil
}

func (s *Store) UpdateDomainGraph(ctx context.Context, domainID string, graph models.KnowledgeSpace) error {
	graphJSON, err := json.Marshal(graph)
	if err != nil {
		return fmt.Errorf("marshal graph: %w", err)
	}
	// Bumping graph_version makes any prior goal_relevance vector stale
	// per OQ-1.1 / IsGoalRelevanceStale(). Existing entries remain valid;
	// only the new concepts will appear in UncoveredConcepts() until a
	// new set_goal_relevance call covers them.
	_, err = s.db.ExecContext(ctx,
		`UPDATE domains SET graph_json = ?, graph_version = graph_version + 1 WHERE id = ?`,
		string(graphJSON), domainID,
	)
	if err != nil {
		return fmt.Errorf("update domain graph: %w", err)
	}
	return nil
}

func (s *Store) ArchiveDomain(ctx context.Context, domainID, learnerID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE domains SET archived = 1 WHERE id = ? AND learner_id = ?`,
		domainID, learnerID,
	)
	if err != nil {
		return fmt.Errorf("archive domain: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("domain not found")
	}
	return nil
}

func (s *Store) UnarchiveDomain(ctx context.Context, domainID, learnerID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE domains SET archived = 0 WHERE id = ? AND learner_id = ?`,
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
// orphan concept_states / interactions left behind by delete_domain (which
// intentionally preserves history but removes the domain row).
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
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete domain tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`DELETE FROM domains WHERE id = ? AND learner_id = ?`,
		domainID, learnerID,
	)
	if err != nil {
		return fmt.Errorf("delete domain: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("domain not found")
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM implementation_intentions WHERE learner_id = ? AND domain_id = ?`,
		learnerID, domainID,
	); err != nil {
		return fmt.Errorf("delete domain implementation intentions: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webhook_message_queue WHERE learner_id = ? AND kind = ?`,
		learnerID, "olm:"+domainID,
	); err != nil {
		return fmt.Errorf("delete domain webhook queue: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete domain tx: %w", err)
	}
	return nil
}

func (s *Store) InsertConceptStateIfNotExists(ctx context.Context, cs *models.ConceptState) error {
	cs.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO concept_states
		    (learner_id, concept, stability, difficulty, elapsed_days, scheduled_days,
		     reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
		     p_slip, p_guess, theta, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cs.LearnerID, cs.Concept, cs.Stability, cs.Difficulty, cs.ElapsedDays, cs.ScheduledDays,
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
	return getConceptStateWithQ(ctx, s.db, learnerID, concept)
}

func getConceptStateWithQ(ctx context.Context, q sqlExecutor, learnerID, concept string) (*models.ConceptState, error) {
	cs := &models.ConceptState{}
	var lastReview, nextReview sql.NullTime
	err := q.QueryRowContext(ctx,
		`SELECT id, learner_id, concept, stability, difficulty, elapsed_days, scheduled_days,
		        reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
		        p_slip, p_guess, theta, updated_at
		 FROM concept_states WHERE learner_id = ? AND concept = ?`,
		learnerID, concept,
	).Scan(
		&cs.ID, &cs.LearnerID, &cs.Concept, &cs.Stability, &cs.Difficulty,
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
	return upsertConceptStateWithQ(ctx, s.db, cs)
}

func upsertConceptStateWithQ(ctx context.Context, q sqlExecutor, cs *models.ConceptState) error {
	cs.UpdatedAt = time.Now().UTC()
	_, err := q.ExecContext(ctx,
		`INSERT INTO concept_states
		    (learner_id, concept, stability, difficulty, elapsed_days, scheduled_days,
		     reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
		     p_slip, p_guess, theta, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(learner_id, concept) DO UPDATE SET
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
		cs.LearnerID, cs.Concept, cs.Stability, cs.Difficulty, cs.ElapsedDays, cs.ScheduledDays,
		cs.Reps, cs.Lapses, cs.CardState, cs.LastReview, cs.NextReview,
		cs.PMastery, cs.PLearn, cs.PForget, cs.PSlip, cs.PGuess,
		cs.Theta, cs.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert concept state: %w", err)
	}
	return nil
}

func (s *Store) GetConceptStatesByLearner(ctx context.Context, learnerID string) ([]*models.ConceptState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, learner_id, concept, stability, difficulty, elapsed_days, scheduled_days,
		        reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
		        p_slip, p_guess, theta, updated_at
		 FROM concept_states WHERE learner_id = ?`,
		learnerID,
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
			&cs.ID, &cs.LearnerID, &cs.Concept, &cs.Stability, &cs.Difficulty,
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

const interactionCols = `id, learner_id, concept, activity_type, success, response_time, confidence, error_type, notes, hints_requested, self_initiated, calibration_id, is_proactive_review, misconception_type, misconception_detail, domain_id, bkt_slip, bkt_guess, rubric_json, rubric_score_json, created_at`

func (s *Store) CreateInteraction(ctx context.Context, i *models.Interaction) error {
	return createInteractionWithQ(ctx, s.db, i)
}

func createInteractionWithQ(ctx context.Context, q sqlExecutor, i *models.Interaction) error {
	i.CreatedAt = time.Now().UTC()
	result, err := q.ExecContext(ctx,
		`INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, error_type, notes, hints_requested, self_initiated, calibration_id, is_proactive_review, misconception_type, misconception_detail, domain_id, bkt_slip, bkt_guess, rubric_json, rubric_score_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.LearnerID, i.Concept, i.ActivityType, boolToInt(i.Success),
		i.ResponseTime, i.Confidence, i.ErrorType, i.Notes,
		i.HintsRequested, boolToInt(i.SelfInitiated), i.CalibrationID, boolToInt(i.IsProactiveReview),
		nullString(i.MisconceptionType), nullString(i.MisconceptionDetail),
		nullString(i.DomainID),
		nullFloat64(i.BKTSlip), nullFloat64(i.BKTGuess),
		nullString(i.RubricJSON), nullString(i.RubricScoreJSON),
		i.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create interaction: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get interaction id: %w", err)
	}
	i.ID = id
	return nil
}

func (s *Store) GetRecentInteractions(ctx context.Context, learnerID, concept string, limit int) ([]*models.Interaction, error) {
	return getRecentInteractionsWithQ(ctx, s.db, learnerID, concept, limit)
}

func getRecentInteractionsWithQ(ctx context.Context, q sqlExecutor, learnerID, concept string, limit int) ([]*models.Interaction, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+interactionCols+` FROM interactions WHERE learner_id = ? AND concept = ?
		 ORDER BY created_at DESC LIMIT ?`,
		learnerID, concept, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get recent interactions: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetRecentInteractionsByLearner(ctx context.Context, learnerID string, limit int) ([]*models.Interaction, error) {
	rows, err := s.db.QueryContext(ctx,
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

func (s *Store) GetSessionInteractions(ctx context.Context, learnerID string) ([]*models.Interaction, error) {
	cutoff := time.Now().UTC().Add(-2 * time.Hour)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+interactionCols+` FROM interactions WHERE learner_id = ? AND created_at > ?
		 ORDER BY created_at DESC`,
		learnerID, cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("get session interactions: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func scanInteractions(rows *sql.Rows) ([]*models.Interaction, error) {
	var interactions []*models.Interaction
	for rows.Next() {
		i := &models.Interaction{}
		var successInt, selfInitInt, proactiveInt int
		var errorType, notes, calibrationID, misconceptionType, misconceptionDetail, domainID, rubricJSON, rubricScoreJSON sql.NullString
		var bktSlip, bktGuess sql.NullFloat64
		if err := rows.Scan(
			&i.ID, &i.LearnerID, &i.Concept, &i.ActivityType,
			&successInt, &i.ResponseTime, &i.Confidence, &errorType, &notes,
			&i.HintsRequested, &selfInitInt, &calibrationID, &proactiveInt,
			&misconceptionType, &misconceptionDetail, &domainID,
			&bktSlip, &bktGuess,
			&rubricJSON, &rubricScoreJSON,
			&i.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan interaction row: %w", err)
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+interactionCols+` FROM interactions WHERE learner_id = ? AND created_at >= ? ORDER BY created_at ASC`,
		learnerID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("get interactions since: %w", err)
	}
	defer rows.Close()
	return scanInteractions(rows)
}

func (s *Store) GetSessionStart(ctx context.Context, learnerID string) (time.Time, error) {
	cutoff := time.Now().UTC().Add(-2 * time.Hour)
	var sessionStart sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(created_at) FROM interactions WHERE learner_id = ? AND created_at > ?`,
		learnerID, cutoff,
	).Scan(&sessionStart)
	if err != nil {
		return time.Time{}, fmt.Errorf("get session start: %w", err)
	}
	if !sessionStart.Valid {
		return time.Now().UTC(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST"} {
		if parsed, err := time.Parse(layout, sessionStart.String); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("parse session start %q", sessionStart.String)
}

// ─── Availability ─────────────────────────────────────────────────────────────

func (s *Store) GetAvailability(ctx context.Context, learnerID string) (*models.Availability, error) {
	a := &models.Availability{}
	var dndInt int
	err := s.db.QueryRowContext(ctx,
		`SELECT learner_id, windows_json, avg_duration, sessions_week, do_not_disturb
		 FROM availability WHERE learner_id = ?`,
		learnerID,
	).Scan(&a.LearnerID, &a.WindowsJSON, &a.AvgDuration, &a.SessionsWeek, &dndInt)
	if err == sql.ErrNoRows {
		return &models.Availability{
			LearnerID:    learnerID,
			WindowsJSON:  "[]",
			AvgDuration:  30,
			SessionsWeek: 3,
			DoNotDisturb: false,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get availability: %w", err)
	}
	a.DoNotDisturb = dndInt != 0
	return a, nil
}

func (s *Store) UpsertAvailability(ctx context.Context, a *models.Availability) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO availability (learner_id, windows_json, avg_duration, sessions_week, do_not_disturb)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(learner_id) DO UPDATE SET
		    windows_json   = excluded.windows_json,
		    avg_duration   = excluded.avg_duration,
		    sessions_week  = excluded.sessions_week,
		    do_not_disturb = excluded.do_not_disturb`,
		a.LearnerID, a.WindowsJSON, a.AvgDuration, a.SessionsWeek, boolToInt(a.DoNotDisturb),
	)
	if err != nil {
		return fmt.Errorf("upsert availability: %w", err)
	}
	return nil
}

// ─── Scheduled Alerts ─────────────────────────────────────────────────────────

func (s *Store) CreateScheduledAlert(ctx context.Context, learnerID, alertType, concept string, scheduledAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduled_alerts (learner_id, alert_type, concept, scheduled_at) VALUES (?, ?, ?, ?)`,
		learnerID, alertType, concept, scheduledAt,
	)
	if err != nil {
		return fmt.Errorf("create scheduled alert: %w", err)
	}
	return nil
}

func (s *Store) GetUnsentAlerts(ctx context.Context, learnerID string) ([]*models.ScheduledAlert, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, learner_id, alert_type, concept, scheduled_at, sent, created_at
		 FROM scheduled_alerts WHERE learner_id = ? AND sent = 0`,
		learnerID,
	)
	if err != nil {
		return nil, fmt.Errorf("get unsent alerts: %w", err)
	}
	defer rows.Close()

	var alerts []*models.ScheduledAlert
	for rows.Next() {
		sa := &models.ScheduledAlert{}
		var sentInt int
		if err := rows.Scan(
			&sa.ID, &sa.LearnerID, &sa.AlertType, &sa.Concept,
			&sa.ScheduledAt, &sentInt, &sa.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan alert row: %w", err)
		}
		sa.Sent = sentInt != 0
		alerts = append(alerts, sa)
	}
	return alerts, rows.Err()
}

func (s *Store) MarkAlertSent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scheduled_alerts SET sent = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("mark alert sent: %w", err)
	}
	return nil
}

func (s *Store) WasAlertSentToday(ctx context.Context, learnerID, alertType string) (bool, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scheduled_alerts
		 WHERE learner_id = ? AND alert_type = ? AND created_at >= ?`,
		learnerID, alertType, today,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check alert sent today: %w", err)
	}
	return count > 0, nil
}

// ─── Stats for Scheduler ─────────────────────────────────────────────────────

// GetDailyStreak returns how many consecutive days the learner has had interactions.
func (s *Store) GetDailyStreak(ctx context.Context, learnerID string) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT DATE(created_at) as d FROM interactions
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
			return streak, nil
		}
		d, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			return streak, nil
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
	return streak, nil
}

// GetTodayInteractionCount returns the number of interactions today.
func (s *Store) GetTodayInteractionCount(ctx context.Context, learnerID string) (int, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var count int
	err := s.db.QueryRowContext(ctx,
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
	err := s.db.QueryRowContext(ctx,
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
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx,
		`SELECT concept FROM concept_states
		 WHERE learner_id = ? AND next_review IS NOT NULL AND next_review <= ? AND card_state != 'new'
		 ORDER BY next_review ASC`,
		learnerID, now,
	)
	if err != nil {
		return nil, fmt.Errorf("get concepts due for review: %w", err)
	}
	defer rows.Close()

	var concepts []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return concepts, nil
		}
		concepts = append(concepts, c)
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

func (s *Store) CreateAuthCode(ctx context.Context, code, learnerID, codeChallenge, clientID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_codes (code, learner_id, code_challenge, client_id, expires_at) VALUES (?, ?, ?, ?, ?)`,
		code, learnerID, codeChallenge, clientID, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("create auth code: %w", err)
	}
	return nil
}

// ConsumeAuthCode retrieves and deletes an auth code in one operation.
// Binds the code to the requesting client_id: returns invalid_grant if mismatch.
func (s *Store) ConsumeAuthCode(ctx context.Context, code, clientID string) (*models.AuthCode, error) {
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	ac := &models.AuthCode{}
	err = tx.QueryRowContext(ctx,
		`SELECT code, learner_id, code_challenge, client_id, expires_at FROM oauth_codes WHERE code = ? AND client_id = ?`,
		code, clientID,
	).Scan(&ac.Code, &ac.LearnerID, &ac.CodeChallenge, &ac.ClientID, &ac.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("invalid_grant")
	}
	if err != nil {
		return nil, fmt.Errorf("consume auth code: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_codes WHERE code = ?`, code); err != nil {
		return nil, fmt.Errorf("delete auth code: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit consume auth code: %w", err)
	}
	return ac, nil
}

func (s *Store) CreateOAuthClient(ctx context.Context, clientID, clientName, redirectURIs string) error {
	return s.CreateOAuthClientWithSecret(ctx, clientID, clientName, redirectURIs, "")
}

// CreateOAuthClientWithSecret persists a confidential client when secretHash != "".
// secretHash should be a bcrypt digest of the issued secret; pass "" for public (PKCE) clients.
func (s *Store) CreateOAuthClientWithSecret(ctx context.Context, clientID, clientName, redirectURIs, secretHash string) error {
	_, err := s.db.ExecContext(ctx,
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
	if maxClients <= 0 {
		return s.CreateOAuthClientWithSecret(ctx, clientID, clientName, redirectURIs, secretHash)
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_clients (client_id, client_name, redirect_uris, client_secret_hash)
		 SELECT ?, ?, ?, ?
		 WHERE (SELECT COUNT(*) FROM oauth_clients) < ?`,
		clientID, clientName, redirectURIs, secretHash, maxClients,
	)
	if err != nil {
		return fmt.Errorf("create oauth client: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("create oauth client rows affected: %w", err)
	}
	if rows == 0 {
		return ErrOAuthClientLimitReached
	}
	return nil
}

func (s *Store) CountOAuthClients(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count oauth clients: %w", err)
	}
	return count, nil
}

func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (*models.OAuthClient, error) {
	c := &models.OAuthClient{}
	var secretHash sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT client_id, client_name, redirect_uris, client_secret_hash FROM oauth_clients WHERE client_id = ?`,
		clientID,
	).Scan(&c.ClientID, &c.ClientName, &c.RedirectURIs, &secretHash)
	if err != nil {
		return nil, fmt.Errorf("get oauth client: %w", err)
	}
	if secretHash.Valid {
		c.ClientSecretHash = secretHash.String
	}
	return c, nil
}

// IsClientApproved reports whether the learner has previously consented to the
// given OAuth client for this exact redirect_uri. R001: the consent screen is
// only meaningful when there's something to approve — once a (learner, client,
// redirect_uri) triple is on file, the next /authorize POST may skip the
// approve_client prompt. A different redirect_uri (even on the same client)
// re-prompts because the approval is scoped to redirect_uri, not client_id.
func (s *Store) IsClientApproved(ctx context.Context, learnerID, clientID, redirectURI string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM learner_approved_clients
		 WHERE learner_id = ? AND client_id = ? AND redirect_uri = ?`,
		learnerID, clientID, redirectURI,
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO learner_approved_clients (learner_id, client_id, redirect_uri)
		 VALUES (?, ?, ?)
		 ON CONFLICT(learner_id, client_id, redirect_uri) DO NOTHING`,
		learnerID, clientID, redirectURI,
	)
	if err != nil {
		return fmt.Errorf("approve client: %w", err)
	}
	return nil
}

func (s *Store) CleanupExpiredCodes(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM oauth_codes WHERE expires_at < ?`, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("cleanup expired codes: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) CleanupExpiredRefreshTokens(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE expires_at < ?`, time.Now().UTC())
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT substr(created_at, 1, 10) AS d
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
//   - mastery_threshold : concept_state.p_mastery >= 0.70 and updated_at >= since
//   - retention_drop    : p_mastery currently < 0.30 (proxy for the fragile state — not a real drop event,
//     since the query sees a snapshot, not a transition) and updated_at >= since
//   - streak_start      : earliest interaction in a still-running streak (computed via GetActivityStreak)
//
// (calibration_threshold derivable from calibration_history but skipped in v1
// to keep the query simple — covered by the calibration sparkline.)
func (s *Store) GetRecentLearnerEvents(ctx context.Context, learnerID string, since time.Time) ([]models.RawLearnerEvent, error) {
	var events []models.RawLearnerEvent

	rows, err := s.db.QueryContext(ctx,
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
				At: at, Kind: "mastery_threshold", Concept: concept,
				Message: fmt.Sprintf("%s reached the mastery threshold", concept),
			})
		} else if mastery < fragileThreshold {
			events = append(events, models.RawLearnerEvent{
				At: at, Kind: "retention_drop", Concept: concept,
				Message: fmt.Sprintf("%s passe en fragile", concept),
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
