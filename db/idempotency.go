// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	storeport "tutor-mcp/store"
)

// ClaimIdempotencyKey atomically reserves a learner/tool/key tuple. A nil
// cached response with execute=true means this caller owns the reservation.
// Completed calls return their cached response unless an opt-in retention
// policy expired it, in which case ErrIdempotencyResponseExpired is returned
// without releasing the tuple. A live reservation never gets stolen
// automatically: after an ambiguous crash, blocking a retry is safer than
// silently applying a learning-state mutation twice.
func (s *Store) ClaimIdempotencyKey(ctx context.Context, learnerID, toolName, key, requestHash string, now time.Time) (cachedResponse string, execute bool, err error) {
	if learnerID == "" || toolName == "" || key == "" || requestHash == "" {
		return "", false, fmt.Errorf("claim idempotency key: required field missing")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, insertErr := s.exec(ctx, `INSERT INTO tool_call_idempotency
			(learner_id, tool_name, idempotency_key, request_hash, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'processing', ?, ?)
			ON CONFLICT (learner_id, tool_name, idempotency_key) DO NOTHING`,
			learnerID, toolName, key, requestHash, now.UTC(), now.UTC())
		if insertErr != nil {
			return "", false, fmt.Errorf("claim idempotency key: %w", insertErr)
		}
		inserted, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return "", false, fmt.Errorf("claim idempotency key rows: %w", rowsErr)
		}
		if inserted == 1 {
			return "", true, nil
		}

		var storedHash, status, response string
		var responseExpiredAt sql.NullTime
		scanErr := s.queryRow(ctx, `SELECT request_hash, status, response_text, response_expired_at
			FROM tool_call_idempotency
			WHERE learner_id = ? AND tool_name = ? AND idempotency_key = ?`,
			learnerID, toolName, key).Scan(&storedHash, &status, &response, &responseExpiredAt)
		if errors.Is(scanErr, sql.ErrNoRows) {
			// The owner may have aborted between our conflict and SELECT. One
			// bounded retry can acquire the now-free reservation.
			continue
		}
		if scanErr != nil {
			return "", false, fmt.Errorf("read idempotency key: %w", scanErr)
		}
		if storedHash != requestHash {
			return "", false, storeport.ErrIdempotencyKeyConflict
		}
		switch status {
		case "completed":
			if responseExpiredAt.Valid {
				return "", false, storeport.ErrIdempotencyResponseExpired
			}
			return response, false, nil
		case "processing":
			return "", false, storeport.ErrIdempotencyInProgress
		default:
			return "", false, fmt.Errorf("unknown idempotency status %q", status)
		}
	}
	return "", false, storeport.ErrIdempotencyInProgress
}

func (s *Store) CompleteIdempotencyKey(ctx context.Context, learnerID, toolName, key, requestHash, responseText string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.exec(ctx, `UPDATE tool_call_idempotency
		SET status = 'completed', response_text = ?, response_expired_at = NULL,
		    completed_at = ?, updated_at = ?
		WHERE learner_id = ? AND tool_name = ? AND idempotency_key = ?
		  AND request_hash = ? AND status = 'processing'`,
		responseText, now.UTC(), now.UTC(), learnerID, toolName, key, requestHash)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete idempotency key rows: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("complete idempotency key: reservation not found")
	}
	return nil
}

func (s *Store) AbortIdempotencyKey(ctx context.Context, learnerID, toolName, key, requestHash string) error {
	_, err := s.exec(ctx, `DELETE FROM tool_call_idempotency
		WHERE learner_id = ? AND tool_name = ? AND idempotency_key = ?
		  AND request_hash = ? AND status = 'processing'`,
		learnerID, toolName, key, requestHash)
	if err != nil {
		return fmt.Errorf("abort idempotency key: %w", err)
	}
	return nil
}
