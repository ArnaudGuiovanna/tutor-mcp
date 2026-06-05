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

// InsertImplementationIntention persists a Gollwitzer-style if-then commitment
// captured at session close. scheduledFor is optional (use the zero time to skip).
func (s *Store) InsertImplementationIntention(ctx context.Context, learnerID, domainID, trigger, action string, scheduledFor time.Time) (int64, error) {
	var scheduled any
	if !scheduledFor.IsZero() {
		scheduled = scheduledFor.UTC()
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO implementation_intentions (learner_id, domain_id, trigger_text, action_text, scheduled_for, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		learnerID, domainID, trigger, action, scheduled, time.Now().UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("insert implementation intention: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get impl intention id: %w", err)
	}
	return id, nil
}

// HasRecentImplementationIntention returns true if the learner (and optionally the
// specific domain) recorded any implementation intention since `since`.
// Pass an empty domainID to check across all domains.
func (s *Store) HasRecentImplementationIntention(ctx context.Context, learnerID, domainID string, since time.Time) (bool, error) {
	var query string
	var args []any
	if domainID == "" {
		query = `SELECT COUNT(*) FROM implementation_intentions
		 WHERE learner_id = ? AND trigger_text <> ? AND created_at >= ?`
		args = []any{learnerID, learningNegotiationOverrideTrigger, since.UTC()}
	} else {
		query = `SELECT COUNT(*) FROM implementation_intentions
		 WHERE learner_id = ? AND domain_id = ? AND trigger_text <> ? AND created_at >= ?`
		args = []any{learnerID, domainID, learningNegotiationOverrideTrigger, since.UTC()}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, learner_id, domain_id, trigger_text, action_text, honored, created_at, scheduled_for
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
		ii := &models.ImplementationIntention{}
		var honored sql.NullInt64
		var scheduled sql.NullTime
		if err := rows.Scan(&ii.ID, &ii.LearnerID, &ii.DomainID, &ii.Trigger, &ii.Action, &honored, &ii.CreatedAt, &scheduled); err != nil {
			return nil, fmt.Errorf("scan implementation intention: %w", err)
		}
		if honored.Valid {
			b := honored.Int64 != 0
			ii.Honored = &b
		}
		if scheduled.Valid {
			t := scheduled.Time
			ii.ScheduledFor = &t
		}
		out = append(out, ii)
	}
	return out, rows.Err()
}

// MarkIntentionHonored transitions the honored flag to 1 (honored) or 0 (missed).
func (s *Store) MarkIntentionHonored(ctx context.Context, id int64, honored bool) error {
	val := 0
	if honored {
		val = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE implementation_intentions SET honored = ? WHERE id = ?`,
		val, id,
	)
	if err != nil {
		return fmt.Errorf("mark intention honored: %w", err)
	}
	return nil
}
