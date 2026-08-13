// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"tutor-mcp/models"
)

const (
	DefaultWebhookMaxAttempts = 5
	maxWebhookMaxAttempts     = 100
	maxWebhookFailureCodeLen  = 128

	webhookQueueColumns = `id, event_id, learner_id, kind, domain_id, scheduled_for, expires_at, content, content_format,
		priority, status, created_at, claimed_at, dispatch_started_at, sent_at, reservation_id,
		attempt_count, max_attempts, next_attempt_at, last_error, dead_lettered_at`
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
	return s.enqueueWebhookMessage(
		ctx, learnerID, kind, webhookDomainID(kind, content), content,
		models.WebhookContentFormatMessage, scheduledFor, expiresAt, priority, maxAttempts, 0,
	)
}

func (s *Store) enqueueWebhookMessage(
	ctx context.Context,
	learnerID, kind, domainID, content, contentFormat string,
	scheduledFor, expiresAt time.Time,
	priority, maxAttempts int,
	reservationID int64,
) (int64, error) {
	if kind == "" {
		return 0, fmt.Errorf("kind is required")
	}
	if content == "" {
		return 0, fmt.Errorf("content is required")
	}
	if contentFormat != models.WebhookContentFormatMessage && contentFormat != models.WebhookContentFormatDiscordPayload {
		return 0, fmt.Errorf("unsupported webhook content format")
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
	eventID, err := generateID()
	if err != nil {
		return 0, fmt.Errorf("enqueue webhook message event id: %w", err)
	}
	eventID = "wh_" + eventID
	createdAt := time.Now().UTC()
	var id int64
	err = s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		var reservation any
		if reservationID != 0 {
			reservation = reservationID
		}
		var insertErr error
		id, insertErr = txs.insertReturningID(ctx,
			`INSERT INTO webhook_message_queue
			 (event_id, learner_id, kind, domain_id, scheduled_for, expires_at, content, content_format, priority, status,
			  created_at, max_attempts, next_attempt_at, reservation_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
			eventID, learnerID, kind, domainID, scheduledFor.UTC(), expires, content, contentFormat, priority,
			createdAt, maxAttempts, scheduledFor.UTC(), reservation,
		)
		if insertErr != nil {
			return insertErr
		}
		return txs.insertWebhookDeliveryTransition(ctx, id, eventID, learnerID, 0, "", models.WebhookStatusPending, "enqueued", createdAt)
	})
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

// HasLiveWebhookMessageKind prevents a generated fallback from overtaking an
// authored message that another worker owns, that is waiting for durable
// backoff, or whose HTTP outcome is quarantined. It is a constant-size EXISTS
// query rather than an unbounded queue read.
func (s *Store) HasLiveWebhookMessageKind(ctx context.Context, learnerID, kind string, now time.Time) (bool, error) {
	if learnerID == "" || kind == "" {
		return false, fmt.Errorf("learner_id and kind are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var exists bool
	if err := s.queryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM webhook_message_queue
		    WHERE learner_id = ? AND kind = ?
		      AND status IN ('pending', 'processing', 'dispatching', 'delivery_unknown')
		      AND (expires_at IS NULL OR expires_at > ?)
		)`, learnerID, kind, now.UTC(),
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check live webhook message kind: %w", err)
	}
	return exists, nil
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
		reservationID, allowed, err := txs.reserveNotificationDeliveryLocked(
			ctx, learnerID, alertType, "", true, scheduledFor.UTC(),
		)
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}

		id, err = txs.enqueueWebhookMessage(
			ctx, learnerID, kind, webhookDomainID(kind, content), content,
			models.WebhookContentFormatMessage, scheduledFor, expiresAt, priority,
			DefaultWebhookMaxAttempts, reservationID,
		)
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

// EnqueueClaimedWebhookPayloadOncePerDay atomically creates the daily
// reservation and a processing queue row carrying an exact, Go-authored
// Discord payload. Returning the row already claimed prevents two scheduler
// instances from materializing duplicate fallbacks between enqueue and claim.
func (s *Store) EnqueueClaimedWebhookPayloadOncePerDay(
	ctx context.Context,
	learnerID, kind, alertType, domainID, content string,
	intrusive bool,
	scheduledFor, expiresAt time.Time,
	priority int,
) (*models.WebhookQueueItem, bool, error) {
	if learnerID == "" || alertType == "" {
		return nil, false, fmt.Errorf("learner_id and alert_type are required")
	}
	if scheduledFor.IsZero() {
		return nil, false, fmt.Errorf("scheduled_for is required")
	}
	claimedAt := scheduledFor.UTC()
	var item *models.WebhookQueueItem
	enqueued := false
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		if err := txs.lockLearnerForNotification(ctx, learnerID); err != nil {
			return fmt.Errorf("lock learner for fallback enqueue: %w", err)
		}
		reservationID, allowed, err := txs.reserveNotificationDeliveryLocked(
			ctx, learnerID, alertType, domainID, intrusive, claimedAt,
		)
		if err != nil || !allowed {
			return err
		}
		id, err := txs.enqueueWebhookMessage(
			ctx, learnerID, kind, domainID, content,
			models.WebhookContentFormatDiscordPayload, scheduledFor, expiresAt,
			priority, DefaultWebhookMaxAttempts, reservationID,
		)
		if err != nil {
			return err
		}
		item, err = txs.claimWebhookByID(ctx, id, learnerID, claimedAt)
		if err != nil {
			return err
		}
		if err := txs.insertWebhookDeliveryTransition(
			ctx, item.ID, item.EventID, learnerID, item.AttemptCount,
			models.WebhookStatusPending, models.WebhookStatusProcessing, "claimed", claimedAt,
		); err != nil {
			return err
		}
		enqueued = true
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("enqueue claimed webhook fallback: %w", err)
	}
	return item, enqueued, nil
}

func (s *Store) claimWebhookByID(ctx context.Context, id int64, learnerID string, claimedAt time.Time) (*models.WebhookQueueItem, error) {
	row := s.queryRow(ctx,
		`UPDATE webhook_message_queue
		 SET status = 'processing', claimed_at = ?, dispatch_started_at = NULL,
		     attempt_count = attempt_count + 1, dead_lettered_at = NULL
		 WHERE id = ? AND learner_id = ? AND status = 'pending'
		 RETURNING `+webhookQueueColumns,
		claimedAt.UTC(), id, learnerID,
	)
	item, err := scanWebhookQueueItem(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("webhook fallback row was not pending")
	}
	return item, err
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
	eventID, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("claim webhook message event id: %w", err)
	}
	eventID = "wh_" + eventID
	var item *models.WebhookQueueItem
	err = s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		var claimErr error
		item, claimErr = txs.claimNextPendingWebhook(ctx, learnerID, kind, claimedAt, upper, eventID)
		if claimErr != nil || item == nil {
			return claimErr
		}
		return txs.insertWebhookDeliveryTransition(
			ctx, item.ID, item.EventID, item.LearnerID, item.AttemptCount,
			models.WebhookStatusPending, models.WebhookStatusProcessing, "claimed", claimedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("claim webhook message: %w", err)
	}
	return item, nil
}

const maxWebhookDeliveryBatch = 100

func normalizeWebhookQueueIDs(queueIDs []int64) ([]int64, string, error) {
	if len(queueIDs) == 0 {
		return nil, "", fmt.Errorf("webhook queue ids are required")
	}
	if len(queueIDs) > maxWebhookDeliveryBatch {
		return nil, "", fmt.Errorf("webhook queue batch must not exceed %d", maxWebhookDeliveryBatch)
	}
	ids := append([]int64(nil), queueIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i, id := range ids {
		if id <= 0 {
			return nil, "", fmt.Errorf("webhook queue id must be positive")
		}
		if i > 0 && ids[i-1] == id {
			return nil, "", fmt.Errorf("duplicate webhook queue id %d", id)
		}
	}
	placeholders := make([]string, len(ids))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	return ids, strings.Join(placeholders, ","), nil
}

func (s *Store) getWebhookQueueItemsForUpdate(ctx context.Context, learnerID string, queueIDs []int64) ([]*models.WebhookQueueItem, error) {
	ids, placeholders, err := normalizeWebhookQueueIDs(queueIDs)
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, learnerID)
	for _, id := range ids {
		args = append(args, id)
	}
	query := `SELECT ` + webhookQueueColumns + `
		FROM webhook_message_queue
		WHERE learner_id = ? AND id IN (` + placeholders + `)
		ORDER BY id`
	if s.dialect == DialectPostgres {
		query += ` FOR UPDATE`
	}
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load webhook delivery rows: %w", err)
	}
	defer rows.Close()
	items := make([]*models.WebhookQueueItem, 0, len(ids))
	for rows.Next() {
		item, scanErr := scanWebhookQueueItemRows(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan webhook delivery row: %w", scanErr)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate webhook delivery rows: %w", err)
	}
	if len(items) != len(ids) {
		return nil, fmt.Errorf("webhook delivery rows not found")
	}
	return items, nil
}

func (s *Store) insertWebhookDeliveryTransition(
	ctx context.Context,
	queueID int64,
	eventID, learnerID string,
	attemptCount int,
	fromStatus, toStatus, reason string,
	at time.Time,
) error {
	reason = sanitizeWebhookFailureReason(reason)
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if _, err := s.exec(ctx,
		`INSERT INTO webhook_delivery_transitions
		 (queue_id, event_id, learner_id, attempt_count, from_status, to_status, reason, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		queueID, eventID, learnerID, attemptCount, fromStatus, toStatus, reason, at.UTC(),
	); err != nil {
		return fmt.Errorf("record webhook delivery transition: %w", err)
	}
	return nil
}

// PrepareWebhookDelivery atomically binds all claimed queue rows to the
// notification reservation that authorizes their outbound request. A queue
// row may already carry the reservation created by EnqueueWebhookMessageOncePerDay;
// that reservation is revalidated rather than duplicated.
func (s *Store) PrepareWebhookDelivery(
	ctx context.Context,
	learnerID, alertType, domainID string,
	intrusive bool,
	queueIDs []int64,
	at time.Time,
) (int64, bool, error) {
	if learnerID == "" || alertType == "" {
		return 0, false, fmt.Errorf("prepare webhook delivery: learner_id and alert_type are required")
	}
	ids, placeholders, err := normalizeWebhookQueueIDs(queueIDs)
	if err != nil {
		return 0, false, fmt.Errorf("prepare webhook delivery: %w", err)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	reservationID := int64(0)
	prepared := false
	err = s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		if err := txs.lockLearnerForNotification(ctx, learnerID); err != nil {
			return err
		}
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, ids)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Status != models.WebhookStatusProcessing {
				return fmt.Errorf("webhook delivery row %d is not processing", item.ID)
			}
			if item.ReservationID == nil {
				continue
			}
			if reservationID != 0 && reservationID != *item.ReservationID {
				return fmt.Errorf("webhook delivery rows have conflicting reservations")
			}
			reservationID = *item.ReservationID
		}

		if reservationID != 0 {
			_, allowed, policyErr := txs.notificationPolicyAllowsLocked(ctx, learnerID, domainID, intrusive, at)
			if policyErr != nil {
				return policyErr
			}
			if !allowed {
				return nil
			}
			var reservationOwner, reservationAlertType, deliveryState string
			var sent int
			if err := txs.queryRow(ctx,
				`SELECT learner_id, alert_type, sent, delivery_state
					 FROM scheduled_alerts WHERE id = ?`, reservationID,
			).Scan(&reservationOwner, &reservationAlertType, &sent, &deliveryState); err != nil {
				return fmt.Errorf("load webhook notification reservation: %w", err)
			}
			if reservationOwner != learnerID || reservationAlertType != alertType || sent != 0 ||
				deliveryState != models.NotificationDeliveryStateReserved {
				return fmt.Errorf("webhook notification reservation is not active")
			}
		} else {
			var allowed bool
			reservationID, allowed, err = txs.reserveNotificationDeliveryLocked(
				ctx, learnerID, alertType, domainID, intrusive, at,
			)
			if err != nil || !allowed {
				return err
			}
		}

		args := make([]any, 0, len(ids)+2)
		args = append(args, reservationID, learnerID)
		for _, id := range ids {
			args = append(args, id)
		}
		result, err := txs.exec(ctx,
			`UPDATE webhook_message_queue SET reservation_id = ?
			 WHERE learner_id = ? AND id IN (`+placeholders+`) AND status = 'processing'`,
			args...,
		)
		if err != nil {
			return fmt.Errorf("bind webhook notification reservation: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != int64(len(ids)) {
			return fmt.Errorf("bind webhook notification reservation: expected %d rows, updated %d", len(ids), rows)
		}
		for _, item := range items {
			if err := txs.insertWebhookDeliveryTransition(
				ctx, item.ID, item.EventID, learnerID, item.AttemptCount,
				models.WebhookStatusProcessing, models.WebhookStatusProcessing,
				"reservation_bound", at,
			); err != nil {
				return err
			}
		}
		prepared = true
		return nil
	})
	if err != nil {
		return 0, false, fmt.Errorf("prepare webhook delivery: %w", err)
	}
	if !prepared {
		return 0, false, nil
	}
	return reservationID, true, nil
}

// BeginWebhookDelivery is the explicit irreversible boundary. No HTTP request
// for a durable queue row may start until this transaction commits.
func (s *Store) BeginWebhookDelivery(ctx context.Context, learnerID string, queueIDs []int64, reservationID int64, at time.Time) error {
	if reservationID == 0 {
		return fmt.Errorf("begin webhook delivery: reservation_id is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	return s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, queueIDs)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Status != models.WebhookStatusProcessing || item.ReservationID == nil || *item.ReservationID != reservationID {
				return fmt.Errorf("webhook delivery row %d is not prepared", item.ID)
			}
			result, err := txs.exec(ctx,
				`UPDATE webhook_message_queue
				 SET status = 'dispatching', dispatch_started_at = ?, last_error = ''
				 WHERE id = ? AND learner_id = ? AND status = 'processing' AND reservation_id = ?`,
				at, item.ID, learnerID, reservationID,
			)
			if err != nil {
				return fmt.Errorf("begin webhook delivery row %d: %w", item.ID, err)
			}
			rows, _ := result.RowsAffected()
			if rows != 1 {
				return fmt.Errorf("begin webhook delivery row %d lost claim", item.ID)
			}
			if err := txs.insertWebhookDeliveryTransition(
				ctx, item.ID, item.EventID, learnerID, item.AttemptCount,
				models.WebhookStatusProcessing, models.WebhookStatusDispatching,
				"http_boundary_entered", at,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func validateWebhookDeliveryItems(items []*models.WebhookQueueItem, status string, reservationID int64) error {
	for _, item := range items {
		if item.Status != status {
			return fmt.Errorf("webhook delivery row %d has status %q, want %q", item.ID, item.Status, status)
		}
		if reservationID != 0 && (item.ReservationID == nil || *item.ReservationID != reservationID) {
			return fmt.Errorf("webhook delivery row %d has a different reservation", item.ID)
		}
	}
	return nil
}

// CompleteWebhookDelivery commits the successful HTTP result and consumes its
// notification reservation in one transaction. A database failure leaves the
// rows in dispatching; stale reconciliation will quarantine them as unknown.
func (s *Store) CompleteWebhookDelivery(ctx context.Context, learnerID string, queueIDs []int64, reservationID int64, at time.Time) error {
	return s.completeWebhookDeliveryFromStatus(
		ctx, learnerID, queueIDs, reservationID, models.WebhookStatusDispatching, "http_2xx", at,
	)
}

func (s *Store) completeWebhookDeliveryFromStatus(
	ctx context.Context,
	learnerID string,
	queueIDs []int64,
	reservationID int64,
	expectedStatus, reason string,
	at time.Time,
) error {
	if reservationID == 0 {
		return fmt.Errorf("complete webhook delivery: reservation_id is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	return s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, queueIDs)
		if err != nil {
			return err
		}
		if err := validateWebhookDeliveryItems(items, expectedStatus, reservationID); err != nil {
			return err
		}
		expectedReservationState := models.NotificationDeliveryStateReserved
		if expectedStatus == models.WebhookStatusDeliveryUnknown {
			expectedReservationState = models.NotificationDeliveryStateDeliveryUnknown
		}
		reservation, err := txs.exec(ctx,
			`UPDATE scheduled_alerts SET sent = 1, delivery_state = 'delivered'
			 WHERE id = ? AND learner_id = ? AND sent = 0 AND delivery_state = ?`,
			reservationID, learnerID, expectedReservationState,
		)
		if err != nil {
			return fmt.Errorf("complete webhook notification reservation: %w", err)
		}
		rows, err := reservation.RowsAffected()
		if err != nil || rows != 1 {
			return fmt.Errorf("complete webhook notification reservation: expected 1 row, updated %d", rows)
		}
		for _, item := range items {
			result, err := txs.exec(ctx,
				`UPDATE webhook_message_queue
				 SET status = 'sent', sent_at = ?, claimed_at = NULL,
				     next_attempt_at = NULL, last_error = '', dead_lettered_at = NULL
				 WHERE id = ? AND learner_id = ? AND status = ? AND reservation_id = ?`,
				at, item.ID, learnerID, expectedStatus, reservationID,
			)
			if err != nil {
				return fmt.Errorf("complete webhook delivery row %d: %w", item.ID, err)
			}
			updated, _ := result.RowsAffected()
			if updated != 1 {
				return fmt.Errorf("complete webhook delivery row %d lost state", item.ID)
			}
			if err := txs.insertWebhookDeliveryTransition(
				ctx, item.ID, item.EventID, learnerID, item.AttemptCount,
				expectedStatus, models.WebhookStatusSent, reason, at,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordKnownWebhookDeliveryFailure is used only after an HTTP response proves
// the endpoint rejected the request. It releases the reservation and applies
// durable backoff with the same event_id.
func (s *Store) RecordKnownWebhookDeliveryFailure(
	ctx context.Context,
	learnerID string,
	queueIDs []int64,
	reservationID int64,
	reason string,
	at time.Time,
) (int, error) {
	if reservationID == 0 {
		return 0, fmt.Errorf("record known webhook delivery failure: reservation_id is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	reason = sanitizeWebhookFailureReason(reason)
	deadLettered := 0
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, queueIDs)
		if err != nil {
			return err
		}
		if err := validateWebhookDeliveryItems(items, models.WebhookStatusDispatching, reservationID); err != nil {
			return err
		}
		for _, item := range items {
			status, err := txs.recordWebhookFailureFromStatus(
				ctx, item.ID, learnerID, models.WebhookStatusDispatching, reason, at,
			)
			if err != nil {
				return err
			}
			if status == models.WebhookStatusFailed {
				deadLettered++
			}
			if err := txs.insertWebhookDeliveryTransition(
				ctx, item.ID, item.EventID, learnerID, item.AttemptCount,
				models.WebhookStatusDispatching, status, reason, at,
			); err != nil {
				return err
			}
		}
		return txs.deleteWebhookNotificationReservation(
			ctx, reservationID, learnerID, models.NotificationDeliveryStateReserved,
		)
	})
	if err != nil {
		return 0, fmt.Errorf("record known webhook delivery failure: %w", err)
	}
	return deadLettered, nil
}

// MarkWebhookDeliveryUnknown quarantines an attempt whose request crossed the
// HTTP boundary but has no trustworthy response. The reservation stays linked
// and consumed for dedup purposes; only an explicit operator resolution can
// declare success or authorize a retry.
func (s *Store) MarkWebhookDeliveryUnknown(
	ctx context.Context,
	learnerID string,
	queueIDs []int64,
	reservationID int64,
	reason string,
	at time.Time,
) error {
	if reservationID == 0 {
		return fmt.Errorf("mark webhook delivery unknown: reservation_id is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	reason = sanitizeWebhookFailureReason(reason)
	return s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, queueIDs)
		if err != nil {
			return err
		}
		if err := validateWebhookDeliveryItems(items, models.WebhookStatusDispatching, reservationID); err != nil {
			return err
		}
		reservation, err := txs.exec(ctx,
			`UPDATE scheduled_alerts SET delivery_state = 'delivery_unknown'
			 WHERE id = ? AND learner_id = ? AND sent = 0 AND delivery_state = 'reserved'`,
			reservationID, learnerID,
		)
		if err != nil {
			return fmt.Errorf("quarantine webhook notification reservation: %w", err)
		}
		rows, err := reservation.RowsAffected()
		if err != nil || rows != 1 {
			return fmt.Errorf("quarantine webhook notification reservation: expected 1 row, updated %d", rows)
		}
		for _, item := range items {
			result, err := txs.exec(ctx,
				`UPDATE webhook_message_queue
				 SET status = 'delivery_unknown', claimed_at = NULL,
				     next_attempt_at = NULL, last_error = ?, dead_lettered_at = NULL
				 WHERE id = ? AND learner_id = ? AND status = 'dispatching' AND reservation_id = ?`,
				reason, item.ID, learnerID, reservationID,
			)
			if err != nil {
				return fmt.Errorf("mark webhook delivery unknown row %d: %w", item.ID, err)
			}
			updated, _ := result.RowsAffected()
			if updated != 1 {
				return fmt.Errorf("mark webhook delivery unknown row %d lost state", item.ID)
			}
			if err := txs.insertWebhookDeliveryTransition(
				ctx, item.ID, item.EventID, learnerID, item.AttemptCount,
				models.WebhookStatusDispatching, models.WebhookStatusDeliveryUnknown,
				reason, at,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReleasePreparedWebhookDelivery is the safe pre-HTTP rollback. It returns the
// rows to pending without consuming an attempt and deletes the unsent linked
// reservation in the same transaction.
func (s *Store) ReleasePreparedWebhookDelivery(
	ctx context.Context,
	learnerID string,
	queueIDs []int64,
	reservationID int64,
	reason string,
	at time.Time,
) error {
	if reservationID == 0 {
		return fmt.Errorf("release prepared webhook delivery: reservation_id is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	reason = sanitizeWebhookFailureReason(reason)
	return s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, queueIDs)
		if err != nil {
			return err
		}
		if err := validateWebhookDeliveryItems(items, models.WebhookStatusProcessing, reservationID); err != nil {
			return err
		}
		for _, item := range items {
			result, err := txs.exec(ctx,
				`UPDATE webhook_message_queue
				 SET status = 'pending', claimed_at = NULL, reservation_id = NULL,
				     attempt_count = CASE WHEN attempt_count > 0 THEN attempt_count - 1 ELSE 0 END,
				     last_error = ?
				 WHERE id = ? AND learner_id = ? AND status = 'processing' AND reservation_id = ?`,
				reason, item.ID, learnerID, reservationID,
			)
			if err != nil {
				return fmt.Errorf("release prepared webhook delivery row %d: %w", item.ID, err)
			}
			updated, _ := result.RowsAffected()
			if updated != 1 {
				return fmt.Errorf("release prepared webhook delivery row %d lost state", item.ID)
			}
			if err := txs.insertWebhookDeliveryTransition(
				ctx, item.ID, item.EventID, learnerID, item.AttemptCount,
				models.WebhookStatusProcessing, models.WebhookStatusPending, reason, at,
			); err != nil {
				return err
			}
		}
		return txs.deleteWebhookNotificationReservation(
			ctx, reservationID, learnerID, models.NotificationDeliveryStateReserved,
		)
	})
}

func (s *Store) deleteWebhookNotificationReservation(ctx context.Context, reservationID int64, learnerID, expectedState string) error {
	if reservationID == 0 {
		return nil
	}
	result, err := s.exec(ctx,
		`DELETE FROM scheduled_alerts
		 WHERE id = ? AND learner_id = ? AND sent = 0 AND delivery_state = ?`,
		reservationID, learnerID, expectedState,
	)
	if err != nil {
		return fmt.Errorf("release webhook notification reservation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return fmt.Errorf("release webhook notification reservation: expected 1 row, deleted %d", rows)
	}
	return nil
}

func (s *Store) deleteWebhookNotificationReservationIfUnreferenced(
	ctx context.Context,
	reservationID int64,
	learnerID, expectedState string,
) error {
	if reservationID == 0 {
		return nil
	}
	if _, err := s.exec(ctx,
		`DELETE FROM scheduled_alerts
		 WHERE id = ? AND learner_id = ? AND sent = 0 AND delivery_state = ?
		   AND NOT EXISTS (
		       SELECT 1 FROM webhook_message_queue WHERE reservation_id = ?
		   )`,
		reservationID, learnerID, expectedState, reservationID,
	); err != nil {
		return fmt.Errorf("release unreferenced webhook notification reservation: %w", err)
	}
	return nil
}

func (s *Store) claimNextPendingWebhook(ctx context.Context, learnerID, kind string, claimedAt, upper time.Time, eventID string) (*models.WebhookQueueItem, error) {
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
		     dispatch_started_at = NULL,
		     event_id = CASE WHEN q.event_id = '' THEN ? ELSE q.event_id END,
		     attempt_count = q.attempt_count + 1,
		     dead_lettered_at = NULL
		 FROM candidate
		 WHERE q.id = candidate.id AND q.status = 'pending'
		 RETURNING ` + prefixedWebhookQueueColumns("q")
		args = []any{learnerID, kind, claimedAt, upper, upper, claimedAt, claimedAt, eventID}
	} else {
		query = `UPDATE webhook_message_queue
		 SET status = 'processing', claimed_at = ?,
		     dispatch_started_at = NULL,
		     event_id = CASE WHEN event_id = '' THEN ? ELSE event_id END,
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
		args = []any{claimedAt, eventID, learnerID, kind, claimedAt, upper, upper, claimedAt}
	}
	row := s.queryRow(ctx, query, args...)
	item, err := scanWebhookQueueItem(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return item, nil
}

// MarkWebhookSent completes a learner-owned processing claim.
func (s *Store) MarkWebhookSent(ctx context.Context, id int64, learnerID string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, []int64{id})
		if err != nil {
			return err
		}
		item := items[0]
		if item.Status != models.WebhookStatusProcessing {
			return fmt.Errorf("item not found or not processing")
		}
		if item.ReservationID != nil {
			return txs.completeWebhookDeliveryFromStatus(
				ctx, learnerID, []int64{id}, *item.ReservationID,
				models.WebhookStatusProcessing, "legacy_mark_sent", now,
			)
		}
		result, err := txs.exec(ctx,
			`UPDATE webhook_message_queue
			 SET status = 'sent', sent_at = ?, claimed_at = NULL,
			     next_attempt_at = NULL, last_error = '', dead_lettered_at = NULL
			 WHERE id = ? AND learner_id = ? AND status = 'processing'`,
			now, id, learnerID,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return fmt.Errorf("item not found or not processing")
		}
		return txs.insertWebhookDeliveryTransition(
			ctx, id, item.EventID, learnerID, item.AttemptCount,
			models.WebhookStatusProcessing, models.WebhookStatusSent,
			"legacy_mark_sent", now,
		)
	})
	if err != nil {
		return fmt.Errorf("mark webhook sent: %w", err)
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
	deadLettered := false
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, []int64{id})
		if err != nil {
			return err
		}
		item := items[0]
		if item.Status != models.WebhookStatusProcessing {
			return fmt.Errorf("item not found or not processing")
		}
		status, err := txs.recordWebhookFailureFromStatus(
			ctx, id, learnerID, models.WebhookStatusProcessing, reason, now,
		)
		if err != nil {
			return err
		}
		deadLettered = status == models.WebhookStatusFailed
		if err := txs.insertWebhookDeliveryTransition(
			ctx, id, item.EventID, learnerID, item.AttemptCount,
			models.WebhookStatusProcessing, status, reason, now,
		); err != nil {
			return err
		}
		if item.ReservationID != nil {
			return txs.deleteWebhookNotificationReservation(
				ctx, *item.ReservationID, learnerID, models.NotificationDeliveryStateReserved,
			)
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record webhook failure: %w", err)
	}
	return deadLettered, nil
}

func (s *Store) recordWebhookFailureFromStatus(ctx context.Context, id int64, learnerID, expectedStatus, reason string, now time.Time) (string, error) {
	retryTimes := webhookRetryTimestamps(now)

	query := `UPDATE webhook_message_queue
		 SET status = CASE
		       WHEN expires_at IS NOT NULL AND expires_at <= ? THEN 'expired'
		       WHEN attempt_count >= max_attempts THEN 'failed'
		       ELSE 'pending'
		     END,
		     claimed_at = NULL,
		     reservation_id = NULL,
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
		 WHERE id = ? AND learner_id = ? AND status = ?
		 RETURNING status`
	args := make([]any, 0, 13)
	args = append(args, now, now)
	args = append(args, retryTimes...)
	args = append(args, reason, now, now, id, learnerID, expectedStatus)

	var status string
	if err := s.queryRow(ctx, query, args...).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("item not found or not %s", expectedStatus)
		}
		return "", err
	}
	return status, nil
}

// ReleaseWebhookClaim returns an unsent learner-owned claim to the pending
// state when a delivery slot was lost to another concurrent scheduler. Policy
// denials are not transport failures and must not permanently discard content.
func (s *Store) ReleaseWebhookClaim(ctx context.Context, id int64, learnerID string) error {
	now := time.Now().UTC()
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, []int64{id})
		if err != nil {
			return err
		}
		item := items[0]
		if item.Status != models.WebhookStatusProcessing {
			return fmt.Errorf("item not found or not processing")
		}
		result, err := txs.exec(ctx,
			`UPDATE webhook_message_queue
			 SET status = 'pending', claimed_at = NULL, reservation_id = NULL,
			     attempt_count = CASE WHEN attempt_count > 0 THEN attempt_count - 1 ELSE 0 END,
			     last_error = 'pre_http_release'
			 WHERE id = ? AND learner_id = ? AND status = 'processing' AND sent_at IS NULL`,
			id, learnerID,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return fmt.Errorf("item not found or not processing")
		}
		if err := txs.insertWebhookDeliveryTransition(
			ctx, id, item.EventID, learnerID, item.AttemptCount,
			models.WebhookStatusProcessing, models.WebhookStatusPending,
			"pre_http_release", now,
		); err != nil {
			return err
		}
		if item.ReservationID != nil {
			return txs.deleteWebhookNotificationReservation(
				ctx, *item.ReservationID, learnerID, models.NotificationDeliveryStateReserved,
			)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("release webhook claim: %w", err)
	}
	return nil
}

// RequeueStaleWebhookClaims reconciles a bounded batch of abandoned workers.
// A processing row is still pre-HTTP and can safely receive normal backoff. A
// dispatching row crossed the persisted HTTP boundary, so it is quarantined as
// delivery_unknown and is never retried without explicit operator resolution.
func (s *Store) RequeueStaleWebhookClaims(ctx context.Context, cutoff, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if cutoff.IsZero() {
		return 0, fmt.Errorf("requeue stale webhook claims: cutoff is required")
	}
	now = now.UTC()
	var reconciled int64
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		query := `SELECT ` + webhookQueueColumns + `
			 FROM webhook_message_queue
			 WHERE (status = 'processing' AND claimed_at IS NOT NULL AND claimed_at < ?)
			    OR (status = 'dispatching'
			        AND COALESCE(dispatch_started_at, claimed_at) IS NOT NULL
			        AND COALESCE(dispatch_started_at, claimed_at) < ?)
			 ORDER BY CASE WHEN status = 'dispatching' THEN 0 ELSE 1 END,
			          COALESCE(dispatch_started_at, claimed_at), id
			 LIMIT 1000`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE SKIP LOCKED`
		}
		rows, err := txs.query(ctx, query, cutoff.UTC(), cutoff.UTC())
		if err != nil {
			return err
		}
		var items []*models.WebhookQueueItem
		for rows.Next() {
			item, scanErr := scanWebhookQueueItemRows(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}

		for _, item := range items {
			switch item.Status {
			case models.WebhookStatusProcessing:
				status, err := txs.recordWebhookFailureFromStatus(
					ctx, item.ID, item.LearnerID, models.WebhookStatusProcessing,
					"delivery_claim_timed_out", now,
				)
				if err != nil {
					return err
				}
				if err := txs.insertWebhookDeliveryTransition(
					ctx, item.ID, item.EventID, item.LearnerID, item.AttemptCount,
					models.WebhookStatusProcessing, status, "delivery_claim_timed_out", now,
				); err != nil {
					return err
				}
				if item.ReservationID != nil {
					if err := txs.deleteWebhookNotificationReservationIfUnreferenced(
						ctx, *item.ReservationID, item.LearnerID,
						models.NotificationDeliveryStateReserved,
					); err != nil {
						return err
					}
				}
			case models.WebhookStatusDispatching:
				reason := "http_boundary_stale"
				if item.ReservationID != nil {
					result, err := txs.exec(ctx,
						`UPDATE scheduled_alerts SET delivery_state = 'delivery_unknown'
						 WHERE id = ? AND learner_id = ? AND sent = 0
						   AND delivery_state IN ('reserved', 'delivery_unknown')`,
						*item.ReservationID, item.LearnerID,
					)
					if err != nil {
						return err
					}
					updated, _ := result.RowsAffected()
					if updated != 1 {
						return fmt.Errorf("stale dispatch reservation %d is not active", *item.ReservationID)
					}
				} else {
					reason = "http_boundary_stale_reservation_missing"
				}
				result, err := txs.exec(ctx,
					`UPDATE webhook_message_queue
					 SET status = 'delivery_unknown', claimed_at = NULL,
					     next_attempt_at = NULL, last_error = ?, dead_lettered_at = NULL
					 WHERE id = ? AND status = 'dispatching'`,
					reason, item.ID,
				)
				if err != nil {
					return err
				}
				updated, _ := result.RowsAffected()
				if updated != 1 {
					return fmt.Errorf("stale dispatch row %d lost state", item.ID)
				}
				if err := txs.insertWebhookDeliveryTransition(
					ctx, item.ID, item.EventID, item.LearnerID, item.AttemptCount,
					models.WebhookStatusDispatching, models.WebhookStatusDeliveryUnknown,
					reason, now,
				); err != nil {
					return err
				}
			}
			reconciled++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("requeue stale webhook claims: %w", err)
	}
	return reconciled, nil
}

// ExpirePastWebhookMessages marks any pending message whose expires_at is in the past as 'expired'.
// This is intentionally global: it is a scheduler cleanup pass, not a learner-scoped mutator.
// Returns the number of rows updated.
func (s *Store) ExpirePastWebhookMessages(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	var expired int64
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		query := `SELECT ` + webhookQueueColumns + `
			 FROM webhook_message_queue
			 WHERE status = 'pending' AND expires_at IS NOT NULL AND expires_at < ?
			 ORDER BY expires_at, id LIMIT 1000`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE SKIP LOCKED`
		}
		rows, err := txs.query(ctx, query, now)
		if err != nil {
			return err
		}
		var items []*models.WebhookQueueItem
		for rows.Next() {
			item, scanErr := scanWebhookQueueItemRows(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range items {
			status, err := txs.recordWebhookFailureFromStatus(
				ctx, item.ID, item.LearnerID, models.WebhookStatusPending,
				"message_expired", now,
			)
			if err != nil {
				return err
			}
			if status != models.WebhookStatusExpired {
				return fmt.Errorf("expire webhook message %d: unexpected status %q", item.ID, status)
			}
			if err := txs.insertWebhookDeliveryTransition(
				ctx, item.ID, item.EventID, item.LearnerID, item.AttemptCount,
				models.WebhookStatusPending, models.WebhookStatusExpired,
				"message_expired", now,
			); err != nil {
				return err
			}
			if item.ReservationID != nil {
				if err := txs.deleteWebhookNotificationReservationIfUnreferenced(
					ctx, *item.ReservationID, item.LearnerID,
					models.NotificationDeliveryStateReserved,
				); err != nil {
					return err
				}
			}
			expired++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("expire past webhook messages: %w", err)
	}
	return expired, nil
}

// GetWebhookDeliveryUnknown returns a tenant-scoped, bounded reconciliation
// queue. Payloads remain in the durable queue row but are not copied into the
// transition history.
func (s *Store) GetWebhookDeliveryUnknown(ctx context.Context, learnerID string, limit int) ([]*models.WebhookQueueItem, error) {
	if learnerID == "" {
		return nil, fmt.Errorf("get unknown webhook deliveries: learner_id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, fmt.Errorf("get unknown webhook deliveries: limit must not exceed 1000")
	}
	rows, err := s.query(ctx,
		`SELECT `+webhookQueueColumns+`
		 FROM webhook_message_queue
		 WHERE learner_id = ? AND status = 'delivery_unknown'
		 ORDER BY dispatch_started_at DESC, id DESC LIMIT ?`,
		learnerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get unknown webhook deliveries: %w", err)
	}
	defer rows.Close()
	var out []*models.WebhookQueueItem
	for rows.Next() {
		item, scanErr := scanWebhookQueueItemRows(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("get unknown webhook deliveries: %w", scanErr)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get unknown webhook deliveries: %w", err)
	}
	return out, nil
}

// GetWebhookDeliveryTransitions returns payload-free history for one stable
// event identity. learnerID is mandatory so operator tooling cannot cross a
// tenant boundary even if an event ID is disclosed.
func (s *Store) GetWebhookDeliveryTransitions(ctx context.Context, learnerID, eventID string, limit int) ([]models.WebhookDeliveryTransition, error) {
	if learnerID == "" || eventID == "" {
		return nil, fmt.Errorf("get webhook delivery transitions: learner_id and event_id are required")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, fmt.Errorf("get webhook delivery transitions: limit must not exceed 1000")
	}
	rows, err := s.query(ctx,
		`SELECT id, queue_id, event_id, learner_id, attempt_count,
		        from_status, to_status, reason, occurred_at
		 FROM webhook_delivery_transitions
		 WHERE learner_id = ? AND event_id = ?
		 ORDER BY id DESC LIMIT ?`,
		learnerID, eventID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get webhook delivery transitions: %w", err)
	}
	defer rows.Close()
	var out []models.WebhookDeliveryTransition
	for rows.Next() {
		var transition models.WebhookDeliveryTransition
		if err := rows.Scan(
			&transition.ID, &transition.QueueID, &transition.EventID,
			&transition.LearnerID, &transition.AttemptCount,
			&transition.FromStatus, &transition.ToStatus, &transition.Reason,
			&transition.OccurredAt,
		); err != nil {
			return nil, fmt.Errorf("get webhook delivery transitions: %w", err)
		}
		out = append(out, transition)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get webhook delivery transitions: %w", err)
	}
	return out, nil
}

// ResolveWebhookDeliveryUnknown is the sole path out of quarantine. delivered
// consumes the retained reservation; not-delivered explicitly releases it and
// applies the normal retry/dead-letter policy without changing event_id.
func (s *Store) ResolveWebhookDeliveryUnknown(ctx context.Context, id int64, learnerID string, delivered bool, at time.Time) error {
	if id <= 0 || learnerID == "" {
		return fmt.Errorf("resolve unknown webhook delivery: id and learner_id are required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		items, err := txs.getWebhookQueueItemsForUpdate(ctx, learnerID, []int64{id})
		if err != nil {
			return err
		}
		item := items[0]
		if item.Status != models.WebhookStatusDeliveryUnknown {
			return fmt.Errorf("webhook delivery row %d is not delivery_unknown", id)
		}
		if delivered {
			if item.ReservationID == nil {
				return fmt.Errorf("webhook delivery row %d has no retained reservation", id)
			}
			return txs.completeWebhookDeliveryFromStatus(
				ctx, learnerID, []int64{id}, *item.ReservationID,
				models.WebhookStatusDeliveryUnknown, "operator_confirmed_delivered", at,
			)
		}

		status, err := txs.recordWebhookFailureFromStatus(
			ctx, id, learnerID, models.WebhookStatusDeliveryUnknown,
			"operator_retry_authorized", at,
		)
		if err != nil {
			return err
		}
		if err := txs.insertWebhookDeliveryTransition(
			ctx, id, item.EventID, learnerID, item.AttemptCount,
			models.WebhookStatusDeliveryUnknown, status,
			"operator_retry_authorized", at,
		); err != nil {
			return err
		}
		if item.ReservationID != nil {
			return txs.deleteWebhookNotificationReservationIfUnreferenced(
				ctx, *item.ReservationID, learnerID,
				models.NotificationDeliveryStateDeliveryUnknown,
			)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("resolve unknown webhook delivery: %w", err)
	}
	return nil
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
	var expiresAt, claimedAt, dispatchStartedAt, sentAt, nextAttemptAt, deadLetteredAt sql.NullTime
	var reservationID sql.NullInt64
	err := row.Scan(
		&item.ID, &item.EventID, &item.LearnerID, &item.Kind, &item.DomainID, &item.ScheduledFor,
		&expiresAt, &item.Content, &item.ContentFormat, &item.Priority, &item.Status,
		&item.CreatedAt, &claimedAt, &dispatchStartedAt, &sentAt, &reservationID, &item.AttemptCount,
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
	if dispatchStartedAt.Valid {
		t := dispatchStartedAt.Time
		item.DispatchStartedAt = &t
	}
	if sentAt.Valid {
		t := sentAt.Time
		item.SentAt = &t
	}
	if reservationID.Valid {
		id := reservationID.Int64
		item.ReservationID = &id
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
	var expiresAt, claimedAt, dispatchStartedAt, sentAt, nextAttemptAt, deadLetteredAt sql.NullTime
	var reservationID sql.NullInt64
	err := rows.Scan(
		&item.ID, &item.EventID, &item.LearnerID, &item.Kind, &item.DomainID, &item.ScheduledFor,
		&expiresAt, &item.Content, &item.ContentFormat, &item.Priority, &item.Status,
		&item.CreatedAt, &claimedAt, &dispatchStartedAt, &sentAt, &reservationID, &item.AttemptCount,
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
	if dispatchStartedAt.Valid {
		t := dispatchStartedAt.Time
		item.DispatchStartedAt = &t
	}
	if sentAt.Valid {
		t := sentAt.Time
		item.SentAt = &t
	}
	if reservationID.Valid {
		id := reservationID.Int64
		item.ReservationID = &id
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
