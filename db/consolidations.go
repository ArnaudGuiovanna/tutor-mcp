// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
)

func (s *Store) UpsertPendingConsolidation(ctx context.Context, learnerID, periodType, periodKey string, now time.Time) error {
	if learnerID == "" || periodType == "" || periodKey == "" {
		return fmt.Errorf("learner_id, period_type and period_key are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := s.exec(ctx,
		`INSERT INTO pending_consolidations (learner_id, period_type, period_key, status, detected_at)
		 VALUES (?, ?, ?, 'pending', ?)
		 ON CONFLICT(learner_id, period_type, period_key) DO NOTHING`,
		learnerID, periodType, periodKey, now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("upsert pending consolidation: %w", err)
	}
	return nil
}

func (s *Store) GetPendingConsolidations(ctx context.Context, learnerID string) ([]*models.PendingConsolidation, error) {
	rows, err := s.query(ctx,
		`SELECT id, learner_id, period_type, period_key, status, detected_at, delivered_at, completed_at
		 FROM pending_consolidations
		 WHERE learner_id = ? AND status = 'pending'
		 ORDER BY detected_at ASC, id ASC`,
		learnerID,
	)
	if err != nil {
		return nil, fmt.Errorf("get pending consolidations: %w", err)
	}
	defer rows.Close()
	return scanConsolidationRows(rows)
}

func (s *Store) GetConsolidation(ctx context.Context, learnerID, periodType, periodKey string) (*models.PendingConsolidation, error) {
	row := s.queryRow(ctx,
		`SELECT id, learner_id, period_type, period_key, status, detected_at, delivered_at, completed_at
		 FROM pending_consolidations
		 WHERE learner_id = ? AND period_type = ? AND period_key = ?`,
		learnerID, periodType, periodKey,
	)
	item, err := scanConsolidationRow(row)
	if err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Store) MarkConsolidationsDelivered(ctx context.Context, learnerID string, ids []int64, now time.Time) error {
	if learnerID == "" || len(ids) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, now.UTC(), learnerID)
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	_, err := s.exec(ctx,
		`UPDATE pending_consolidations
		 SET status = 'delivered', delivered_at = ?
		 WHERE learner_id = ? AND status = 'pending' AND id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("mark consolidations delivered: %w", err)
	}
	return nil
}

func (s *Store) MarkConsolidationCompleted(ctx context.Context, learnerID, periodType, periodKey string, now time.Time) error {
	if learnerID == "" || periodType == "" || periodKey == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := s.exec(ctx,
		`INSERT INTO pending_consolidations (learner_id, period_type, period_key, status, detected_at, completed_at)
		 VALUES (?, ?, ?, 'completed', ?, ?)
		 ON CONFLICT(learner_id, period_type, period_key) DO UPDATE SET
		   status = 'completed',
		   completed_at = excluded.completed_at`,
		learnerID, periodType, periodKey, now.UTC(), now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("mark consolidation completed: %w", err)
	}
	return nil
}

func (s *Store) RequeueStaleDeliveredConsolidations(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.exec(ctx,
		`UPDATE pending_consolidations
		 SET status = 'pending', delivered_at = NULL
		 WHERE status = 'delivered' AND delivered_at IS NOT NULL AND delivered_at < ?`,
		cutoff.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("requeue stale delivered consolidations: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanConsolidationRows(rows *sql.Rows) ([]*models.PendingConsolidation, error) {
	var out []*models.PendingConsolidation
	for rows.Next() {
		item, err := scanConsolidationScanner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type consolidationScanner interface {
	Scan(dest ...any) error
}

func scanConsolidationRow(row *sql.Row) (*models.PendingConsolidation, error) {
	item, err := scanConsolidationScanner(row)
	if err != nil {
		return nil, fmt.Errorf("scan consolidation: %w", err)
	}
	return item, nil
}

func scanConsolidationScanner(scanner consolidationScanner) (*models.PendingConsolidation, error) {
	item := &models.PendingConsolidation{}
	var deliveredAt, completedAt sql.NullTime
	if err := scanner.Scan(
		&item.ID,
		&item.LearnerID,
		&item.PeriodType,
		&item.PeriodKey,
		&item.Status,
		&item.DetectedAt,
		&deliveredAt,
		&completedAt,
	); err != nil {
		return nil, err
	}
	if deliveredAt.Valid {
		ts := deliveredAt.Time
		item.DeliveredAt = &ts
	}
	if completedAt.Valid {
		ts := completedAt.Time
		item.CompletedAt = &ts
	}
	return item, nil
}
