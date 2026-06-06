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

const learningNegotiationOverrideTrigger = "__learning_negotiation_activity_override__"

// InsertLearningNegotiationOverridePayload stores a pending one-shot activity
// override in the existing implementation_intentions table. A new override
// supersedes any older pending override for the same learner/domain pair.
func (s *Store) InsertLearningNegotiationOverridePayload(ctx context.Context, learnerID, domainID, payload string, expiresAt, now time.Time) (int64, error) {
	var id int64
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx,
			`UPDATE implementation_intentions
			 SET honored = 0
			 WHERE learner_id = ? AND domain_id = ? AND trigger_text = ? AND honored IS NULL`,
			learnerID, domainID, learningNegotiationOverrideTrigger,
		); err != nil {
			return fmt.Errorf("supersede learning negotiation override: %w", err)
		}

		insertQuery := `INSERT INTO implementation_intentions
			 (learner_id, domain_id, trigger_text, action_text, scheduled_for, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`
		insertArgs := []any{learnerID, domainID, learningNegotiationOverrideTrigger, payload, expiresAt.UTC(), now.UTC()}

		var err error
		id, err = txs.insertReturningID(ctx, insertQuery, insertArgs...)
		if err != nil {
			return fmt.Errorf("insert learning negotiation override: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ConsumeLearningNegotiationOverridePayload atomically marks the latest pending
// override consumed. Expired overrides are marked missed and returned as expired.
func (s *Store) ConsumeLearningNegotiationOverridePayload(ctx context.Context, learnerID, domainID string, now time.Time) (*models.LearningNegotiationOverridePayloadResult, error) {
	var out *models.LearningNegotiationOverridePayloadResult
	err := s.inTx(ctx, nil, func(txs *Store) error {
		var id int64
		var payload string
		var expiresAt sql.NullTime
		err := txs.queryRow(ctx,
			`SELECT id, action_text, scheduled_for
			 FROM implementation_intentions
			 WHERE learner_id = ? AND domain_id = ? AND trigger_text = ? AND honored IS NULL
			 ORDER BY created_at DESC, id DESC
			 LIMIT 1`,
			learnerID, domainID, learningNegotiationOverrideTrigger,
		).Scan(&id, &payload, &expiresAt)
		if err == sql.ErrNoRows {
			out = &models.LearningNegotiationOverridePayloadResult{Status: models.LearningNegotiationOverrideStatusNone}
			return nil
		}
		if err != nil {
			return fmt.Errorf("select learning negotiation override: %w", err)
		}

		var expires *time.Time
		if expiresAt.Valid {
			t := expiresAt.Time.UTC()
			expires = &t
			if !t.After(now.UTC()) {
				if _, err := txs.exec(ctx,
					`UPDATE implementation_intentions SET honored = 0 WHERE id = ? AND honored IS NULL`,
					id,
				); err != nil {
					return fmt.Errorf("expire learning negotiation override: %w", err)
				}
				out = &models.LearningNegotiationOverridePayloadResult{
					ID:        id,
					Status:    models.LearningNegotiationOverrideStatusExpired,
					ExpiresAt: expires,
				}
				return nil
			}
		}

		result, err := txs.exec(ctx,
			`UPDATE implementation_intentions SET honored = 1 WHERE id = ? AND honored IS NULL`,
			id,
		)
		if err != nil {
			return fmt.Errorf("mark learning negotiation override consumed: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("learning negotiation override consume rows affected: %w", err)
		}
		if affected == 0 {
			out = &models.LearningNegotiationOverridePayloadResult{Status: models.LearningNegotiationOverrideStatusNone}
			return nil
		}
		out = &models.LearningNegotiationOverridePayloadResult{
			ID:        id,
			Payload:   payload,
			Status:    models.LearningNegotiationOverrideStatusConsumed,
			ExpiresAt: expires,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
