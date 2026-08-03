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

// InsertImplementationIntentionForSession persists a pending intention linked
// to the explicit session in which it was agreed. An empty sessionID is kept
// only for compatibility with pre-session callers and legacy data.
func (s *Store) InsertImplementationIntentionForSession(ctx context.Context, learnerID, domainID, sessionID, trigger, action string, scheduledFor time.Time) (int64, error) {
	var scheduled any
	if !scheduledFor.IsZero() {
		scheduled = scheduledFor.UTC()
	}
	if sessionID != "" {
		session, err := s.GetLearningSession(ctx, learnerID, sessionID)
		if err != nil {
			return 0, fmt.Errorf("validate intention session: %w", err)
		}
		if domainID != "" && session.DomainID != "" && session.DomainID != domainID {
			return 0, fmt.Errorf("intention session belongs to another domain")
		}
		if existing, err := s.getImplementationIntentionBySession(ctx, learnerID, sessionID); err == nil {
			if existing.Trigger == trigger && existing.Action == action {
				return existing.ID, nil
			}
			return 0, fmt.Errorf("learning session already has an implementation intention")
		} else if err != sql.ErrNoRows {
			return 0, fmt.Errorf("check existing session intention: %w", err)
		}
	}
	now := time.Now().UTC()
	id, err := s.insertReturningID(ctx,
		`INSERT INTO implementation_intentions
		    (learner_id, domain_id, session_id, trigger_text, action_text, status, scheduled_for, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		learnerID, domainID, nullString(sessionID), trigger, action,
		models.IntentionStatusPending, scheduled, now, now,
	)
	if err != nil {
		// The unique session index may have been won by a concurrent retry.
		if sessionID != "" {
			if existing, getErr := s.getImplementationIntentionBySession(ctx, learnerID, sessionID); getErr == nil {
				if existing.Trigger == trigger && existing.Action == action {
					return existing.ID, nil
				}
				return 0, fmt.Errorf("learning session already has an implementation intention")
			}
		}
		return 0, fmt.Errorf("insert implementation intention: %w", err)
	}
	return id, nil
}

func (s *Store) getImplementationIntentionBySession(ctx context.Context, learnerID, sessionID string) (*models.ImplementationIntention, error) {
	return scanImplementationIntention(s.queryRow(ctx,
		`SELECT id, learner_id, domain_id, session_id, trigger_text, action_text,
		        status, honored, created_at, scheduled_for, resolved_at, updated_at
		 FROM implementation_intentions
		 WHERE learner_id = ? AND session_id = ? AND trigger_text <> ?`,
		learnerID, sessionID, learningNegotiationOverrideTrigger,
	))
}

// HasRecentImplementationIntention returns true if the learner (and optionally the
// specific domain) recorded any implementation intention since `since`.
// Pass an empty domainID to check across all domains.
func (s *Store) HasRecentImplementationIntention(ctx context.Context, learnerID, domainID string, since time.Time) (bool, error) {
	var query string
	var args []any
	if domainID == "" {
		query = `SELECT COUNT(*) FROM implementation_intentions
		 WHERE learner_id = ? AND trigger_text <> ? AND status <> ? AND created_at >= ?`
		args = []any{learnerID, learningNegotiationOverrideTrigger, models.IntentionStatusCancelled, since.UTC()}
	} else {
		query = `SELECT COUNT(*) FROM implementation_intentions
		 WHERE learner_id = ? AND domain_id = ? AND trigger_text <> ? AND status <> ? AND created_at >= ?`
		args = []any{learnerID, domainID, learningNegotiationOverrideTrigger, models.IntentionStatusCancelled, since.UTC()}
	}
	var count int
	if err := s.queryRow(ctx, query, args...).Scan(&count); err != nil {
		return false, fmt.Errorf("check recent implementation intention: %w", err)
	}
	return count > 0, nil
}

// GetRecentImplementationIntentions returns intentions for a learner recorded on or after `since`,
// most recent first.
func (s *Store) GetRecentImplementationIntentions(ctx context.Context, learnerID string, since time.Time, limit int) ([]*models.ImplementationIntention, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.query(ctx,
		`SELECT id, learner_id, domain_id, session_id, trigger_text, action_text,
		        status, honored, created_at, scheduled_for, resolved_at, updated_at
		 FROM implementation_intentions
		 WHERE learner_id = ? AND trigger_text <> ? AND created_at >= ?
		 ORDER BY created_at DESC LIMIT ?`,
		learnerID, learningNegotiationOverrideTrigger, since.UTC(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get implementation intentions: %w", err)
	}
	defer rows.Close()

	var out []*models.ImplementationIntention
	for rows.Next() {
		ii, err := scanImplementationIntention(rows)
		if err != nil {
			return nil, fmt.Errorf("scan implementation intention: %w", err)
		}
		out = append(out, ii)
	}
	return out, rows.Err()
}

type implementationIntentionScanner interface {
	Scan(dest ...any) error
}

func scanImplementationIntention(row implementationIntentionScanner) (*models.ImplementationIntention, error) {
	ii := &models.ImplementationIntention{}
	var sessionID sql.NullString
	var honored sql.NullInt64
	var scheduled, resolved sql.NullTime
	if err := row.Scan(
		&ii.ID, &ii.LearnerID, &ii.DomainID, &sessionID, &ii.Trigger, &ii.Action,
		&ii.Status, &honored, &ii.CreatedAt, &scheduled, &resolved, &ii.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if sessionID.Valid {
		ii.SessionID = sessionID.String
	}
	if honored.Valid {
		value := honored.Int64 != 0
		ii.Honored = &value
	}
	if scheduled.Valid {
		value := scheduled.Time
		ii.ScheduledFor = &value
	}
	if resolved.Valid {
		value := resolved.Time
		ii.ResolvedAt = &value
	}
	return ii, nil
}

func (s *Store) GetImplementationIntention(ctx context.Context, learnerID string, id int64) (*models.ImplementationIntention, error) {
	ii, err := scanImplementationIntention(s.queryRow(ctx,
		`SELECT id, learner_id, domain_id, session_id, trigger_text, action_text,
		        status, honored, created_at, scheduled_for, resolved_at, updated_at
		 FROM implementation_intentions
		 WHERE id = ? AND learner_id = ? AND trigger_text <> ?`,
		id, learnerID, learningNegotiationOverrideTrigger,
	))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("implementation intention not found")
		}
		return nil, fmt.Errorf("get implementation intention: %w", err)
	}
	return ii, nil
}

func validIntentionStatus(status string) bool {
	switch status {
	case models.IntentionStatusPending,
		models.IntentionStatusHonored,
		models.IntentionStatusMissed,
		models.IntentionStatusCancelled:
		return true
	default:
		return false
	}
}

// UpdateImplementationIntentionStatus applies a one-way lifecycle transition.
// Terminal states cannot be rewritten; repeating the same transition is
// idempotent. learnerID is part of every lookup/update to enforce ownership.
func (s *Store) UpdateImplementationIntentionStatus(ctx context.Context, learnerID string, id int64, status string, now time.Time) (*models.ImplementationIntention, error) {
	if !validIntentionStatus(status) {
		return nil, fmt.Errorf("invalid implementation intention status")
	}
	current, err := s.GetImplementationIntention(ctx, learnerID, id)
	if err != nil {
		return nil, err
	}
	if current.Status == status {
		return current, nil
	}
	if current.Status != models.IntentionStatusPending || status == models.IntentionStatusPending {
		return nil, fmt.Errorf("implementation intention is already resolved")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	honored := 0
	if status == models.IntentionStatusHonored {
		honored = 1
	}
	res, err := s.exec(ctx,
		`UPDATE implementation_intentions
		 SET status = ?, honored = ?, resolved_at = ?, updated_at = ?
		 WHERE id = ? AND learner_id = ? AND status = ?`,
		status, honored, now, now, id, learnerID, models.IntentionStatusPending,
	)
	if err != nil {
		return nil, fmt.Errorf("update implementation intention status: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// A concurrent identical transition is still idempotent.
		latest, getErr := s.GetImplementationIntention(ctx, learnerID, id)
		if getErr == nil && latest.Status == status {
			return latest, nil
		}
		return nil, fmt.Errorf("implementation intention status conflict")
	}
	return s.GetImplementationIntention(ctx, learnerID, id)
}
