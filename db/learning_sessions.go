// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"tutor-mcp/models"
)

type learningSessionScanner interface {
	Scan(dest ...any) error
}

func scanLearningSession(row learningSessionScanner) (*models.LearningSession, error) {
	session := &models.LearningSession{}
	var domainID sql.NullString
	var closedAt sql.NullTime
	if err := row.Scan(
		&session.ID,
		&session.LearnerID,
		&domainID,
		&session.Status,
		&session.StartedAt,
		&session.LastActiveAt,
		&closedAt,
	); err != nil {
		return nil, err
	}
	if domainID.Valid {
		session.DomainID = domainID.String
	}
	if closedAt.Valid {
		closed := closedAt.Time
		session.ClosedAt = &closed
	}
	return session, nil
}

const learningSessionCols = `id, learner_id, domain_id, status, started_at, last_active_at, closed_at`

// OpenLearningSession returns the learner's existing open session or creates
// one. The partial unique index on open sessions is the final concurrency
// guard, while ON CONFLICT makes racing callers converge on its winner.
func (s *Store) OpenLearningSession(ctx context.Context, learnerID, domainID, requestedID string, now time.Time) (*models.LearningSession, error) {
	if learnerID == "" {
		return nil, fmt.Errorf("learner is required")
	}
	var learnerCount int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM learners WHERE id = ?`, learnerID).Scan(&learnerCount); err != nil {
		return nil, fmt.Errorf("validate learning session learner: %w", err)
	}
	if learnerCount != 1 {
		return nil, fmt.Errorf("learning session learner not found")
	}
	if domainID != "" {
		var count int
		if err := s.queryRow(ctx,
			`SELECT COUNT(*) FROM domains WHERE id = ? AND learner_id = ? AND archived = 0`,
			domainID, learnerID,
		).Scan(&count); err != nil {
			return nil, fmt.Errorf("validate learning session domain: %w", err)
		}
		if count != 1 {
			return nil, fmt.Errorf("learning session domain not found")
		}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if requestedID == "" {
		generated, err := generateID()
		if err != nil {
			return nil, fmt.Errorf("generate learning session id: %w", err)
		}
		requestedID = "sess_" + generated
	}

	var result *models.LearningSession
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx,
			`INSERT INTO learning_sessions
			    (id, learner_id, domain_id, status, started_at, last_active_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT DO NOTHING`,
			requestedID, learnerID, nullString(domainID), models.LearningSessionStatusOpen, now, now,
		); err != nil {
			return fmt.Errorf("insert learning session: %w", err)
		}

		// Repeating the same requested ID is idempotent. A closed session is
		// deliberately not reopened: session history is immutable.
		requested, err := scanLearningSession(txs.queryRow(ctx,
			`SELECT `+learningSessionCols+` FROM learning_sessions
			 WHERE id = ? AND learner_id = ?`,
			requestedID, learnerID,
		))
		if err == nil {
			if requested.Status != models.LearningSessionStatusOpen {
				return fmt.Errorf("learning session is already closed")
			}
			if _, err := txs.exec(ctx,
				`UPDATE learning_sessions
				 SET domain_id = CASE WHEN ? <> '' THEN ? ELSE domain_id END,
				     last_active_at = CASE WHEN last_active_at < ? THEN ? ELSE last_active_at END
				 WHERE id = ? AND learner_id = ? AND status = ?`,
				domainID, domainID, now, now, requested.ID, learnerID, models.LearningSessionStatusOpen,
			); err != nil {
				return fmt.Errorf("resume learning session: %w", err)
			}
			if domainID != "" {
				requested.DomainID = domainID
			}
			if requested.LastActiveAt.Before(now) {
				requested.LastActiveAt = now
			}
			result = requested
			return nil
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("read requested learning session: %w", err)
		}

		// A concurrent caller may have won with another generated ID. Return
		// that row rather than exposing a spurious conflict to the client.
		active, err := scanLearningSession(txs.queryRow(ctx,
			`SELECT `+learningSessionCols+` FROM learning_sessions
			 WHERE learner_id = ? AND status = ? LIMIT 1`,
			learnerID, models.LearningSessionStatusOpen,
		))
		if err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("learning session id is unavailable")
			}
			return fmt.Errorf("read active learning session: %w", err)
		}
		if _, err := txs.exec(ctx,
			`UPDATE learning_sessions
			 SET domain_id = CASE WHEN ? <> '' THEN ? ELSE domain_id END,
			     last_active_at = CASE WHEN last_active_at < ? THEN ? ELSE last_active_at END
			 WHERE id = ? AND learner_id = ? AND status = ?`,
			domainID, domainID, now, now, active.ID, learnerID, models.LearningSessionStatusOpen,
		); err != nil {
			return fmt.Errorf("resume active learning session: %w", err)
		}
		if domainID != "" {
			active.DomainID = domainID
		}
		if active.LastActiveAt.Before(now) {
			active.LastActiveAt = now
		}
		result = active
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) GetLearningSession(ctx context.Context, learnerID, sessionID string) (*models.LearningSession, error) {
	session, err := scanLearningSession(s.queryRow(ctx,
		`SELECT `+learningSessionCols+` FROM learning_sessions
		 WHERE id = ? AND learner_id = ?`,
		sessionID, learnerID,
	))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("learning session not found")
		}
		return nil, fmt.Errorf("get learning session: %w", err)
	}
	return session, nil
}

func (s *Store) GetActiveLearningSession(ctx context.Context, learnerID string) (*models.LearningSession, error) {
	session, err := scanLearningSession(s.queryRow(ctx,
		`SELECT `+learningSessionCols+` FROM learning_sessions
		 WHERE learner_id = ? AND status = ? LIMIT 1`,
		learnerID, models.LearningSessionStatusOpen,
	))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("active learning session not found")
		}
		return nil, fmt.Errorf("get active learning session: %w", err)
	}
	return session, nil
}

func (s *Store) TouchLearningSession(ctx context.Context, learnerID, sessionID string, now time.Time) (*models.LearningSession, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	res, err := s.exec(ctx,
		`UPDATE learning_sessions
		 SET last_active_at = CASE WHEN last_active_at < ? THEN ? ELSE last_active_at END
		 WHERE id = ? AND learner_id = ? AND status = ?`,
		now, now, sessionID, learnerID, models.LearningSessionStatusOpen,
	)
	if err != nil {
		return nil, fmt.Errorf("touch learning session: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return nil, fmt.Errorf("open learning session not found")
	}
	return s.GetLearningSession(ctx, learnerID, sessionID)
}

// CloseLearningSession is idempotent for its owner. Repeating a close returns
// the original closed_at timestamp; a different learner sees only not found.
func (s *Store) CloseLearningSession(ctx context.Context, learnerID, sessionID string, now time.Time) (*models.LearningSession, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if _, err := s.exec(ctx,
		`UPDATE learning_sessions
		 SET status = ?, closed_at = ?, last_active_at = CASE WHEN last_active_at < ? THEN ? ELSE last_active_at END
		 WHERE id = ? AND learner_id = ? AND status = ?`,
		models.LearningSessionStatusClosed, now, now, now,
		sessionID, learnerID, models.LearningSessionStatusOpen,
	); err != nil {
		return nil, fmt.Errorf("close learning session: %w", err)
	}
	session, err := s.GetLearningSession(ctx, learnerID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("learning session not found")
	}
	if session.Status != models.LearningSessionStatusClosed {
		return nil, fmt.Errorf("learning session could not be closed")
	}
	return session, nil
}
