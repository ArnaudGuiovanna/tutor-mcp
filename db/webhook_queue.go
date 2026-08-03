// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
)

const (
	DefaultWebhookMaxAttempts = 5
	maxWebhookMaxAttempts     = 100
	maxWebhookFailureCodeLen  = 128

	webhookQueueColumns = `id, learner_id, kind, domain_id, scheduled_for, expires_at, content,
		priority, status, created_at, claimed_at, sent_at, attempt_count,
		max_attempts, next_attempt_at, last_error, dead_lettered_at`
)

var webhookRetryDelays = [...]time.Duration{
	time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	12 * time.Hour,
}

const webhookRetryTimestampCase = `CASE
	WHEN attempt_count <= 1 THEN ?
	WHEN attempt_count = 2 THEN ?
	WHEN attempt_count = 3 THEN ?
	WHEN attempt_count = 4 THEN ?
	ELSE ?
END`

// PostgreSQL resolves an otherwise-untyped CASE made only of bind parameters
// as text before considering the assignment target. Cast every branch so the
// expression remains TIMESTAMPTZ-compatible. SQLite uses the uncast variant
// above because its type names and parameter rules are different.
const webhookRetryTimestampCasePostgres = `CASE
	WHEN attempt_count <= 1 THEN CAST(? AS TIMESTAMPTZ)
	WHEN attempt_count = 2 THEN CAST(? AS TIMESTAMPTZ)
	WHEN attempt_count = 3 THEN CAST(? AS TIMESTAMPTZ)
	WHEN attempt_count = 4 THEN CAST(? AS TIMESTAMPTZ)
	ELSE CAST(? AS TIMESTAMPTZ)
END`

func (s *Store) webhookRetryTimestampCase() string {
	if s.dialect == DialectPostgres {
		return webhookRetryTimestampCasePostgres
	}
	return webhookRetryTimestampCase
}

func (s *Store) webhookTimestampParameter() string {
	if s.dialect == DialectPostgres {
		return "CAST(? AS TIMESTAMPTZ)"
	}
	return "?"
}

func webhookRetryTimestamps(now time.Time) []any {
	out := make([]any, len(webhookRetryDelays))
	for i, delay := range webhookRetryDelays {
		out[i] = now.Add(delay).UTC()
	}
	return out
}

func sanitizeWebhookFailureReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > maxWebhookFailureCodeLen {
		return "delivery_failed"
	}
	for _, char := range reason {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return "delivery_failed"
	}
	return reason
}

func prefixedWebhookQueueColumns(alias string) string {
	columns := strings.Split(webhookQueueColumns, ",")
	for i, column := range columns {
		columns[i] = alias + "." + strings.TrimSpace(column)
	}
	return strings.Join(columns, ", ")
}

func webhookDomainID(kind, content string) string {
	if strings.HasPrefix(kind, "olm:") {
		return strings.TrimSpace(strings.TrimPrefix(kind, "olm:"))
	}
	var envelope struct {
		DomainID string `json:"domain_id"`
	}
	if json.Unmarshal([]byte(content), &envelope) == nil {
		return strings.TrimSpace(envelope.DomainID)
	}
	return ""
}

// EnqueueWebhookMessage persists a scheduled, LLM-authored webhook nudge.
// Returns the inserted row ID.
func (s *Store) EnqueueWebhookMessage(ctx context.Context, learnerID, kind, content string, scheduledFor, expiresAt time.Time, priority int) (int64, error) {
	return s.EnqueueWebhookMessageWithMaxAttempts(
		ctx, learnerID, kind, content, scheduledFor, expiresAt, priority, DefaultWebhookMaxAttempts,
	)
}

// EnqueueWebhookMessageWithMaxAttempts persists a message with an explicit
// retry budget. The ordinary enqueue path uses DefaultWebhookMaxAttempts;
// callers with a stricter delivery contract can lower the per-message cap.
func (s *Store) EnqueueWebhookMessageWithMaxAttempts(ctx context.Context, learnerID, kind, content string, scheduledFor, expiresAt time.Time, priority, maxAttempts int) (int64, error) {
	if kind == "" {
		return 0, fmt.Errorf("kind is required")
	}
	if content == "" {
		return 0, fmt.Errorf("content is required")
	}
	if scheduledFor.IsZero() {
		return 0, fmt.Errorf("scheduled_for is required")
	}
	if maxAttempts < 1 || maxAttempts > maxWebhookMaxAttempts {
		return 0, fmt.Errorf("max_attempts must be between 1 and %d", maxWebhookMaxAttempts)
	}
	var expires any
	if !expiresAt.IsZero() {
		expires = expiresAt.UTC()
	}
	id, err := s.insertReturningID(ctx,
		`INSERT INTO webhook_message_queue
		 (learner_id, kind, domain_id, scheduled_for, expires_at, content, priority, status,
		  created_at, max_attempts, next_attempt_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
		learnerID, kind, webhookDomainID(kind, content), scheduledFor.UTC(), expires, content, priority,
		time.Now().UTC(), maxAttempts, scheduledFor.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("enqueue webhook message: %w", err)
	}
	return id, nil
}

// IsWebhookClaimActive revalidates a claimed row immediately before the
// irreversible HTTP boundary. Domain archive/deletion can terminalize a claim
// between selection and dispatch; such a row must not be sent.
func (s *Store) IsWebhookClaimActive(ctx context.Context, id int64, learnerID string) (bool, error) {
	var active bool
	if err := s.queryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM webhook_message_queue
		    WHERE id = ? AND learner_id = ? AND status = 'processing'
		)`, id, learnerID,
	).Scan(&active); err != nil {
		return false, fmt.Errorf("revalidate webhook claim: %w", err)
	}
	return active, nil
}

// EnqueueWebhookMessageOncePerDay atomically reserves one daily alert slot and
// enqueues its webhook payload. The learner row is locked on Postgres (SQLite's
// BEGIN IMMEDIATE already serializes writers), so concurrent instances cannot
// both pass the daily check and enqueue duplicate mirror messages.
func (s *Store) EnqueueWebhookMessageOncePerDay(
	ctx context.Context,
	learnerID, kind, alertType, content string,
	scheduledFor, expiresAt time.Time,
	priority int,
) (int64, bool, error) {
	if learnerID == "" {
		return 0, false, fmt.Errorf("learner_id is required")
	}
	if alertType == "" {
		return 0, false, fmt.Errorf("alert_type is required")
	}
	var id int64
	enqueued := false
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		if err := txs.lockLearnerForNotification(ctx, learnerID); err != nil {
			return fmt.Errorf("lock learner for webhook enqueue: %w", err)
		}
		_, allowed, err := txs.reserveNotificationDeliveryLocked(
			ctx, learnerID, alertType, "", true, scheduledFor.UTC(),
		)
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}

		id, err = txs.EnqueueWebhookMessage(ctx, learnerID, kind, content, scheduledFor, expiresAt, priority)
		if err != nil {
			return err
		}
		enqueued = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return id, enqueued, nil
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

// ClaimNextPendingWebhook atomically transitions the highest-priority eligible
// message from pending to processing, increments its durable attempt counter,
// and returns it. The scheduling window is a look-ahead upper bound, not a
// lower freshness bound: overdue first attempts remain recoverable after
// downtime, while expires_at is the authoritative freshness policy. Retries
// remain eligible once next_attempt_at is due. The state transition and row
// selection happen in one SQL statement, so concurrent workers cannot dispatch
// the same attempt.
func (s *Store) ClaimNextPendingWebhook(ctx context.Context, learnerID, kind string, now time.Time, window time.Duration) (*models.WebhookQueueItem, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if window < 0 {
		return nil, fmt.Errorf("claim webhook message: window must be non-negative")
	}
	upper := now.Add(window).UTC()
	claimedAt := now.UTC()
	var query string
	var args []any
	if s.dialect == DialectPostgres {
		query = `WITH candidate AS (
			 SELECT id
			 FROM webhook_message_queue
			 WHERE learner_id = ? AND kind = ? AND status = 'pending'
			   AND (expires_at IS NULL OR expires_at > ?)
			   AND attempt_count < max_attempts
			   AND (
			     (attempt_count = 0 AND scheduled_for <= ?)
			     OR
			     (attempt_count > 0 AND scheduled_for <= ?
			       AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?)
			   )
			 ORDER BY priority DESC,
			          CASE WHEN attempt_count > 0 THEN next_attempt_at ELSE scheduled_for END ASC,
			          id ASC
			 LIMIT 1
			 FOR UPDATE SKIP LOCKED
		 )
		 UPDATE webhook_message_queue AS q
		 SET status = 'processing', claimed_at = ?,
		     attempt_count = q.attempt_count + 1,
		     dead_lettered_at = NULL
		 FROM candidate
		 WHERE q.id = candidate.id AND q.status = 'pending'
		 RETURNING ` + prefixedWebhookQueueColumns("q")
		args = []any{learnerID, kind, claimedAt, upper, upper, claimedAt, claimedAt}
	} else {
		query = `UPDATE webhook_message_queue
		 SET status = 'processing', claimed_at = ?,
		     attempt_count = attempt_count + 1,
		     dead_lettered_at = NULL
		 WHERE id = (
			 SELECT id
			 FROM webhook_message_queue
			 WHERE learner_id = ? AND kind = ? AND status = 'pending'
			   AND (expires_at IS NULL OR expires_at > ?)
			   AND attempt_count < max_attempts
			   AND (
			     (attempt_count = 0 AND scheduled_for <= ?)
			     OR
			     (attempt_count > 0 AND scheduled_for <= ?
			       AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?)
			   )
			 ORDER BY priority DESC,
			          CASE WHEN attempt_count > 0 THEN next_attempt_at ELSE scheduled_for END ASC,
			          id ASC
			 LIMIT 1
		 )
		 AND status = 'pending'
		 RETURNING ` + webhookQueueColumns
		args = []any{claimedAt, learnerID, kind, claimedAt, upper, upper, claimedAt}
	}
	row := s.queryRow(ctx, query, args...)
	item, err := scanWebhookQueueItem(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim webhook message: %w", err)
	}
	return item, nil
}

// MarkWebhookSent completes a learner-owned processing claim.
func (s *Store) MarkWebhookSent(ctx context.Context, id int64, learnerID string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.exec(ctx,
		`UPDATE webhook_message_queue
		 SET status = 'sent', sent_at = ?, claimed_at = NULL,
		     next_attempt_at = NULL, last_error = '', dead_lettered_at = NULL
		 WHERE id = ? AND learner_id = ? AND status = 'processing'`,
		now.UTC(), id, learnerID,
	)
	if err != nil {
		return fmt.Errorf("mark webhook sent: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark webhook sent rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark webhook sent: item not found or not processing")
	}
	return nil
}

// MarkWebhookFailed records a transport failure using the default sanitized
// reason. It requeues with durable exponential backoff until max_attempts is
// reached, then leaves the row in terminal `failed` status (the dead-letter
// queue). Use RecordWebhookFailure when a stable, non-secret reason code is
// available.
func (s *Store) MarkWebhookFailed(ctx context.Context, id int64, learnerID string) error {
	_, err := s.RecordWebhookFailure(ctx, id, learnerID, "delivery_failed", time.Now().UTC())
	return err
}

// RecordWebhookFailure persists a failed delivery attempt. reason must be a
// sanitized diagnostic code, not a raw webhook URL, bearer credential, or
// response body. The returned bool reports whether this attempt exhausted the
// retry budget and moved the row to the terminal failed/dead-letter state.
func (s *Store) RecordWebhookFailure(ctx context.Context, id int64, learnerID, reason string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	reason = sanitizeWebhookFailureReason(reason)
	retryTimes := webhookRetryTimestamps(now)

	query := `UPDATE webhook_message_queue
		 SET status = CASE
		       WHEN expires_at IS NOT NULL AND expires_at <= ? THEN 'expired'
		       WHEN attempt_count >= max_attempts THEN 'failed'
		       ELSE 'pending'
		     END,
		     claimed_at = NULL,
		     next_attempt_at = CASE
		       WHEN (expires_at IS NOT NULL AND expires_at <= ?)
		         OR attempt_count >= max_attempts THEN NULL
		       ELSE ` + s.webhookRetryTimestampCase() + `
		     END,
		     last_error = ?,
		     dead_lettered_at = CASE
		       WHEN (expires_at IS NULL OR expires_at > ?)
		         AND attempt_count >= max_attempts THEN ` + s.webhookTimestampParameter() + `
		       ELSE NULL
		     END
		 WHERE id = ? AND learner_id = ? AND status = 'processing'
		 RETURNING status`
	args := make([]any, 0, 12)
	args = append(args, now, now)
	args = append(args, retryTimes...)
	args = append(args, reason, now, now, id, learnerID)

	var status string
	if err := s.queryRow(ctx, query, args...).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return false, fmt.Errorf("record webhook failure: item not found or not processing")
		}
		return false, fmt.Errorf("record webhook failure: %w", err)
	}
	return status == models.WebhookStatusFailed, nil
}

// ReleaseWebhookClaim returns an unsent learner-owned claim to the pending
// state when a delivery slot was lost to another concurrent scheduler. Policy
// denials are not transport failures and must not permanently discard content.
func (s *Store) ReleaseWebhookClaim(ctx context.Context, id int64, learnerID string) error {
	result, err := s.exec(ctx,
		`UPDATE webhook_message_queue
		 SET status = 'pending', claimed_at = NULL,
		     attempt_count = CASE WHEN attempt_count > 0 THEN attempt_count - 1 ELSE 0 END
		 WHERE id = ? AND learner_id = ? AND status = 'processing' AND sent_at IS NULL`,
		id, learnerID,
	)
	if err != nil {
		return fmt.Errorf("release webhook claim: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("release webhook claim rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("release webhook claim: item not found or not processing")
	}
	return nil
}

// RequeueStaleWebhookClaims recovers processing rows abandoned by a crashed
// worker. The claim already consumed an attempt: recoverable rows receive the
// same durable backoff as an explicit delivery failure; expired rows become
// expired, and exhausted rows enter the terminal failed/dead-letter state.
func (s *Store) RequeueStaleWebhookClaims(ctx context.Context, cutoff, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if cutoff.IsZero() {
		return 0, fmt.Errorf("requeue stale webhook claims: cutoff is required")
	}
	now = now.UTC()
	retryTimes := webhookRetryTimestamps(now)
	query := `UPDATE webhook_message_queue
		 SET status = CASE
		       WHEN expires_at IS NOT NULL AND expires_at <= ? THEN 'expired'
		       WHEN attempt_count >= max_attempts THEN 'failed'
		       ELSE 'pending'
		     END,
		     claimed_at = NULL,
		     next_attempt_at = CASE
		       WHEN (expires_at IS NOT NULL AND expires_at <= ?)
		         OR attempt_count >= max_attempts THEN NULL
		       ELSE ` + s.webhookRetryTimestampCase() + `
		     END,
		     last_error = 'delivery_claim_timed_out',
		     dead_lettered_at = CASE
		       WHEN (expires_at IS NULL OR expires_at > ?)
		         AND attempt_count >= max_attempts THEN ` + s.webhookTimestampParameter() + `
		       ELSE NULL
		     END
		 WHERE status = 'processing' AND claimed_at IS NOT NULL AND claimed_at < ?`
	args := make([]any, 0, 12)
	args = append(args, now, now)
	args = append(args, retryTimes...)
	args = append(args, now, now, cutoff.UTC())
	result, err := s.exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("requeue stale webhook claims: %w", err)
	}
	return result.RowsAffected()
}

// ExpirePastWebhookMessages marks any pending message whose expires_at is in the past as 'expired'.
// This is intentionally global: it is a scheduler cleanup pass, not a learner-scoped mutator.
// Returns the number of rows updated.
func (s *Store) ExpirePastWebhookMessages(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.exec(ctx,
		`UPDATE webhook_message_queue
		 SET status = 'expired', next_attempt_at = NULL, dead_lettered_at = NULL
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
		`SELECT `+webhookQueueColumns+`
		 FROM webhook_message_queue
		 WHERE learner_id = ? AND status = 'pending'
		 ORDER BY COALESCE(next_attempt_at, scheduled_for) ASC, id ASC`,
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

// GetDeadLetterWebhookMessages returns terminal delivery failures for operator
// inspection. Content is intentionally not copied to a separate table: the
// existing queue row is the DLQ and remains subject to the configured terminal
// queue retention policy.
func (s *Store) GetDeadLetterWebhookMessages(ctx context.Context, learnerID string, limit int) ([]*models.WebhookQueueItem, error) {
	if learnerID == "" {
		return nil, fmt.Errorf("get dead-letter webhook messages: learner_id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, fmt.Errorf("get dead-letter webhook messages: limit must not exceed 1000")
	}
	rows, err := s.query(ctx,
		`SELECT `+webhookQueueColumns+`
		 FROM webhook_message_queue
		 WHERE learner_id = ? AND status = 'failed'
		 ORDER BY CASE WHEN dead_lettered_at IS NULL THEN 1 ELSE 0 END,
		          dead_lettered_at DESC, id DESC
		 LIMIT ?`,
		learnerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get dead-letter webhook messages: %w", err)
	}
	defer rows.Close()

	var out []*models.WebhookQueueItem
	for rows.Next() {
		item, err := scanWebhookQueueItemRows(rows)
		if err != nil {
			return nil, fmt.Errorf("get dead-letter webhook messages: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get dead-letter webhook messages: %w", err)
	}
	return out, nil
}

// scanWebhookQueueItem is used for QueryRow results.
func scanWebhookQueueItem(row *sql.Row) (*models.WebhookQueueItem, error) {
	item := &models.WebhookQueueItem{}
	var expiresAt, claimedAt, sentAt, nextAttemptAt, deadLetteredAt sql.NullTime
	err := row.Scan(
		&item.ID, &item.LearnerID, &item.Kind, &item.DomainID, &item.ScheduledFor,
		&expiresAt, &item.Content, &item.Priority, &item.Status,
		&item.CreatedAt, &claimedAt, &sentAt, &item.AttemptCount,
		&item.MaxAttempts, &nextAttemptAt, &item.LastError, &deadLetteredAt,
	)
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		item.ExpiresAt = &t
	}
	if claimedAt.Valid {
		t := claimedAt.Time
		item.ClaimedAt = &t
	}
	if sentAt.Valid {
		t := sentAt.Time
		item.SentAt = &t
	}
	if nextAttemptAt.Valid {
		t := nextAttemptAt.Time
		item.NextAttemptAt = &t
	}
	if deadLetteredAt.Valid {
		t := deadLetteredAt.Time
		item.DeadLetteredAt = &t
	}
	return item, nil
}

// scanWebhookQueueItemRows is used for Query results (iteration).
func scanWebhookQueueItemRows(rows *sql.Rows) (*models.WebhookQueueItem, error) {
	item := &models.WebhookQueueItem{}
	var expiresAt, claimedAt, sentAt, nextAttemptAt, deadLetteredAt sql.NullTime
	err := rows.Scan(
		&item.ID, &item.LearnerID, &item.Kind, &item.DomainID, &item.ScheduledFor,
		&expiresAt, &item.Content, &item.Priority, &item.Status,
		&item.CreatedAt, &claimedAt, &sentAt, &item.AttemptCount,
		&item.MaxAttempts, &nextAttemptAt, &item.LastError, &deadLetteredAt,
	)
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		item.ExpiresAt = &t
	}
	if claimedAt.Valid {
		t := claimedAt.Time
		item.ClaimedAt = &t
	}
	if sentAt.Valid {
		t := sentAt.Time
		item.SentAt = &t
	}
	if nextAttemptAt.Valid {
		t := nextAttemptAt.Time
		item.NextAttemptAt = &t
	}
	if deadLetteredAt.Valid {
		t := deadLetteredAt.Time
		item.DeadLetteredAt = &t
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
