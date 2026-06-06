// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"tutor-mcp/models"
)

// EnqueueWebhookMessage persists a scheduled, LLM-authored webhook nudge.
// Returns the inserted row ID.
func (s *Store) EnqueueWebhookMessage(ctx context.Context, learnerID, kind, content string, scheduledFor, expiresAt time.Time, priority int) (int64, error) {
	if kind == "" {
		return 0, fmt.Errorf("kind is required")
	}
	if content == "" {
		return 0, fmt.Errorf("content is required")
	}
	if scheduledFor.IsZero() {
		return 0, fmt.Errorf("scheduled_for is required")
	}
	var expires any
	if !expiresAt.IsZero() {
		expires = expiresAt.UTC()
	}
	id, err := s.insertReturningID(ctx,
		`INSERT INTO webhook_message_queue (learner_id, kind, scheduled_for, expires_at, content, priority, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
		learnerID, kind, scheduledFor.UTC(), expires, content, priority, time.Now().UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("enqueue webhook message: %w", err)
	}
	return id, nil
}

// CreateWebhookPushLog records a learner-facing push after the webhook send
// succeeds. queueID can be zero for Go fallback messages that did not originate
// from webhook_message_queue.
func (s *Store) CreateWebhookPushLog(ctx context.Context, learnerID string, queueID int64, brief *models.WebhookBrief, pushedAt time.Time) (int64, error) {
	if learnerID == "" {
		return 0, fmt.Errorf("learner_id is required")
	}
	if brief == nil {
		return 0, fmt.Errorf("webhook brief is required")
	}
	brief.Normalize(brief.Kind)
	if brief.Kind == "" {
		return 0, fmt.Errorf("kind is required")
	}
	if pushedAt.IsZero() {
		pushedAt = time.Now().UTC()
	}
	id, err := s.insertReturningID(ctx,
		`INSERT INTO webhook_push_log
		 (learner_id, queue_id, kind, domain_id, domain_name, concept, trigger_text,
		  pedagogical_intent, learning_gain, open_loop, next_action, pushed_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		learnerID, queueID, brief.Kind, brief.DomainID, brief.DomainName, brief.Concept,
		brief.Trigger, brief.PedagogicalIntent, brief.LearningGain, brief.OpenLoop,
		brief.NextAction, pushedAt.UTC(), time.Now().UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("create webhook push log: %w", err)
	}
	return id, nil
}

// GetLatestOpenWebhookPush returns the newest unresolved pedagogical push for
// a learner. If domainID is provided, global pushes with an empty domain_id
// and matching domain pushes are both eligible.
func (s *Store) GetLatestOpenWebhookPush(ctx context.Context, learnerID, domainID string, since time.Time) (*models.WebhookPushLog, error) {
	query := `SELECT id, learner_id, queue_id, kind, domain_id, domain_name, concept,
		         trigger_text, pedagogical_intent, learning_gain, open_loop, next_action,
		         pushed_at, opened_session_at, concept_addressed, created_at
		  FROM webhook_push_log
		  WHERE learner_id = ?
		    AND concept_addressed = 0
		    AND pushed_at >= ?`
	args := []any{learnerID, since.UTC()}
	if domainID != "" {
		query += ` AND (domain_id = '' OR domain_id = ?)`
		args = append(args, domainID)
	}
	query += ` ORDER BY pushed_at DESC LIMIT 1`
	row := s.queryRow(ctx, query, args...)
	push, err := scanWebhookPushLog(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest webhook push: %w", err)
	}
	return push, nil
}

// MarkWebhookPushSessionOpened notes that a learner returned after a push.
// It intentionally does not mark concept_addressed; that happens only when an
// interaction touches the pushed concept.
func (s *Store) MarkWebhookPushSessionOpened(ctx context.Context, learnerID string, openedAt, since time.Time) error {
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	_, err := s.exec(ctx,
		`UPDATE webhook_push_log
		    SET opened_session_at = COALESCE(opened_session_at, ?),
		        concept_addressed = CASE WHEN concept = '' THEN 1 ELSE concept_addressed END
		  WHERE learner_id = ?
		    AND opened_session_at IS NULL
		    AND pushed_at >= ?`,
		openedAt.UTC(), learnerID, since.UTC(),
	)
	if err != nil {
		return fmt.Errorf("mark webhook push session opened: %w", err)
	}
	return nil
}

// MarkWebhookPushConceptAddressed closes open-loop pushes whose concept was
// actually worked on in a later session.
func (s *Store) MarkWebhookPushConceptAddressed(ctx context.Context, learnerID, domainID, concept string, addressedAt, since time.Time) error {
	if concept == "" {
		return nil
	}
	if addressedAt.IsZero() {
		addressedAt = time.Now().UTC()
	}
	query := `UPDATE webhook_push_log
		     SET opened_session_at = COALESCE(opened_session_at, ?),
		         concept_addressed = 1
		   WHERE learner_id = ?
		     AND concept = ?
		     AND concept_addressed = 0
		     AND pushed_at >= ?`
	args := []any{addressedAt.UTC(), learnerID, concept, since.UTC()}
	if domainID != "" {
		query += ` AND (domain_id = '' OR domain_id = ?)`
		args = append(args, domainID)
	}
	if _, err := s.exec(ctx, query, args...); err != nil {
		return fmt.Errorf("mark webhook push concept addressed: %w", err)
	}
	return nil
}

// DequeueNextPending returns the highest-priority pending message for a learner/kind
// whose scheduled_for is within [now-window, now+window] and not expired.
// Returns (nil, nil) if nothing is pending.
func (s *Store) DequeueNextPending(ctx context.Context, learnerID, kind string, now time.Time, window time.Duration) (*models.WebhookQueueItem, error) {
	lower := now.Add(-window).UTC()
	upper := now.Add(window).UTC()
	query := `SELECT id, learner_id, kind, scheduled_for, expires_at, content, priority, status, created_at, sent_at
		 FROM webhook_message_queue
		 WHERE learner_id = ? AND kind = ? AND status = 'pending'
		   AND scheduled_for BETWEEN ? AND ?
		   AND (expires_at IS NULL OR expires_at > ?)
		 ORDER BY priority DESC, scheduled_for ASC
		 LIMIT 1`
	// On Postgres, lock the claimed row and skip rows already locked by another
	// worker so multiple scheduler instances dequeuing concurrently each get a
	// distinct message (exactly-once dispatch). Requires a surrounding tx
	// (WithTx) to hold the lock until the row is marked sent. SQLite is
	// single-writer (BEGIN IMMEDIATE), so no row-lock clause is needed.
	if s.dialect == DialectPostgres {
		query += ` FOR UPDATE SKIP LOCKED`
	}
	row := s.queryRow(ctx, query, learnerID, kind, lower, upper, now.UTC())
	item, err := scanWebhookQueueItem(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dequeue webhook message: %w", err)
	}
	return item, nil
}

// MarkWebhookSent marks the learner-owned queue item as sent at `now`.
func (s *Store) MarkWebhookSent(ctx context.Context, id int64, learnerID string, now time.Time) error {
	_, err := s.exec(ctx,
		`UPDATE webhook_message_queue SET status = 'sent', sent_at = ? WHERE id = ? AND learner_id = ?`,
		now.UTC(), id, learnerID,
	)
	if err != nil {
		return fmt.Errorf("mark webhook sent: %w", err)
	}
	return nil
}

// MarkWebhookFailed marks the learner-owned queue item as failed (will not be retried).
func (s *Store) MarkWebhookFailed(ctx context.Context, id int64, learnerID string) error {
	_, err := s.exec(ctx,
		`UPDATE webhook_message_queue SET status = 'failed' WHERE id = ? AND learner_id = ?`,
		id, learnerID,
	)
	if err != nil {
		return fmt.Errorf("mark webhook failed: %w", err)
	}
	return nil
}

// ExpirePastWebhookMessages marks any pending message whose expires_at is in the past as 'expired'.
// This is intentionally global: it is a scheduler cleanup pass, not a learner-scoped mutator.
// Returns the number of rows updated.
func (s *Store) ExpirePastWebhookMessages(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.exec(ctx,
		`UPDATE webhook_message_queue SET status = 'expired'
		 WHERE status = 'pending' AND expires_at IS NOT NULL AND expires_at < ?`,
		now.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("expire past webhook messages: %w", err)
	}
	return result.RowsAffected()
}

// GetPendingWebhookMessages returns all pending messages (for monitoring / debugging).
func (s *Store) GetPendingWebhookMessages(ctx context.Context, learnerID string) ([]*models.WebhookQueueItem, error) {
	rows, err := s.query(ctx,
		`SELECT id, learner_id, kind, scheduled_for, expires_at, content, priority, status, created_at, sent_at
		 FROM webhook_message_queue
		 WHERE learner_id = ? AND status = 'pending'
		 ORDER BY scheduled_for ASC`,
		learnerID,
	)
	if err != nil {
		return nil, fmt.Errorf("get pending webhook messages: %w", err)
	}
	defer rows.Close()

	var out []*models.WebhookQueueItem
	for rows.Next() {
		item, err := scanWebhookQueueItemRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// scanWebhookQueueItem is used for QueryRow results.
func scanWebhookQueueItem(row *sql.Row) (*models.WebhookQueueItem, error) {
	item := &models.WebhookQueueItem{}
	var expiresAt, sentAt sql.NullTime
	err := row.Scan(
		&item.ID, &item.LearnerID, &item.Kind, &item.ScheduledFor,
		&expiresAt, &item.Content, &item.Priority, &item.Status,
		&item.CreatedAt, &sentAt,
	)
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		item.ExpiresAt = &t
	}
	if sentAt.Valid {
		t := sentAt.Time
		item.SentAt = &t
	}
	return item, nil
}

// scanWebhookQueueItemRows is used for Query results (iteration).
func scanWebhookQueueItemRows(rows *sql.Rows) (*models.WebhookQueueItem, error) {
	item := &models.WebhookQueueItem{}
	var expiresAt, sentAt sql.NullTime
	err := rows.Scan(
		&item.ID, &item.LearnerID, &item.Kind, &item.ScheduledFor,
		&expiresAt, &item.Content, &item.Priority, &item.Status,
		&item.CreatedAt, &sentAt,
	)
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		item.ExpiresAt = &t
	}
	if sentAt.Valid {
		t := sentAt.Time
		item.SentAt = &t
	}
	return item, nil
}

func scanWebhookPushLog(row *sql.Row) (*models.WebhookPushLog, error) {
	push := &models.WebhookPushLog{}
	var openedAt sql.NullTime
	var conceptAddressed int
	err := row.Scan(
		&push.ID, &push.LearnerID, &push.QueueID, &push.Kind, &push.DomainID,
		&push.DomainName, &push.Concept, &push.Trigger, &push.PedagogicalIntent,
		&push.LearningGain, &push.OpenLoop, &push.NextAction, &push.PushedAt,
		&openedAt, &conceptAddressed, &push.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if openedAt.Valid {
		t := openedAt.Time
		push.OpenedSessionAt = &t
	}
	push.ConceptAddressed = conceptAddressed != 0
	return push, nil
}
