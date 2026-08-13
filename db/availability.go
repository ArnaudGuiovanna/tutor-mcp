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

// ReserveNotificationDelivery atomically applies the latest learner-owned
// consent, DND, local weekly windows, frequency and daily cap. For intrusive
// pushes it also applies the high-stakes human-review gate. The scheduled_alert
// row is a short-lived reservation until CompleteNotificationDelivery; a
// normal send failure releases it, while a process crash fails closed for the
// remainder of the period instead of risking duplicate/intrusive delivery.
func (s *Store) ReserveNotificationDelivery(
	ctx context.Context,
	learnerID, alertType, domainID string,
	intrusive bool,
	at time.Time,
) (int64, bool, error) {
	if learnerID == "" || alertType == "" {
		return 0, false, fmt.Errorf("learner_id and alert_type are required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	var id int64
	var reserved bool
	err := s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		if err := txs.lockLearnerForNotification(ctx, learnerID); err != nil {
			return err
		}
		var err error
		id, reserved, err = txs.reserveNotificationDeliveryLocked(ctx, learnerID, alertType, domainID, intrusive, at)
		return err
	})
	if err != nil {
		return 0, false, err
	}
	return id, reserved, nil
}

func (s *Store) lockLearnerForNotification(ctx context.Context, learnerID string) error {
	query := `SELECT id FROM learners WHERE id = ?`
	if s.dialect == DialectPostgres {
		query += ` FOR UPDATE`
	}
	var locked string
	if err := s.queryRow(ctx, query, learnerID).Scan(&locked); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("notification learner not found")
		}
		return fmt.Errorf("lock notification learner: %w", err)
	}
	return nil
}

func (s *Store) reserveNotificationDeliveryLocked(
	ctx context.Context,
	learnerID, alertType, domainID string,
	intrusive bool,
	at time.Time,
) (int64, bool, error) {
	availability, allowed, err := s.notificationPolicyAllowsLocked(ctx, learnerID, domainID, intrusive, at)
	if err != nil || !allowed {
		return 0, false, err
	}

	bounds, err := availability.NotificationBounds(at)
	if err != nil {
		return 0, false, fmt.Errorf("derive notification period: %w", err)
	}
	var duplicate int
	if err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM scheduled_alerts
		 WHERE learner_id = ? AND alert_type = ? AND created_at >= ? AND created_at < ?`,
		learnerID, alertType, bounds.DayStart, bounds.DayEnd,
	).Scan(&duplicate); err != nil {
		return 0, false, fmt.Errorf("check notification dedup: %w", err)
	}
	if duplicate > 0 {
		return 0, false, nil
	}

	var todayCount int
	if err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM scheduled_alerts
		 WHERE learner_id = ? AND created_at >= ? AND created_at < ?`,
		learnerID, bounds.DayStart, bounds.DayEnd,
	).Scan(&todayCount); err != nil {
		return 0, false, fmt.Errorf("check notification daily cap: %w", err)
	}
	if todayCount >= availability.MaxNotificationsPerDay {
		return 0, false, nil
	}

	if bounds.OnePerPeriod {
		var periodCount int
		if err := s.queryRow(ctx,
			`SELECT COUNT(*) FROM scheduled_alerts
			 WHERE learner_id = ? AND created_at >= ? AND created_at < ?`,
			learnerID, bounds.FrequencyStart, bounds.FrequencyEnd,
		).Scan(&periodCount); err != nil {
			return 0, false, fmt.Errorf("check notification frequency: %w", err)
		}
		if periodCount > 0 {
			return 0, false, nil
		}
	}

	id, err := s.insertReturningID(ctx,
		`INSERT INTO scheduled_alerts
		    (learner_id, alert_type, concept, scheduled_at, sent, created_at)
		 VALUES (?, ?, '', ?, 0, ?)`,
		learnerID, alertType, at, at,
	)
	if err != nil {
		return 0, false, fmt.Errorf("reserve notification delivery: %w", err)
	}
	return id, true, nil
}

// notificationPolicyAllowsLocked re-evaluates mutable learner consent and
// high-stakes policy while the learner notification lock is held. Delivery
// preparations that reuse an enqueue-time reservation call this helper so a
// later consent revocation still wins before the HTTP boundary.
func (s *Store) notificationPolicyAllowsLocked(
	ctx context.Context,
	learnerID, domainID string,
	intrusive bool,
	at time.Time,
) (*models.Availability, bool, error) {
	availability, err := s.GetAvailability(ctx, learnerID)
	if err != nil {
		return nil, false, err
	}
	allowed, err := availability.AllowsNotificationAt(at)
	if err != nil {
		return nil, false, fmt.Errorf("evaluate notification availability: %w", err)
	}
	if !allowed {
		return availability, false, nil
	}
	if intrusive {
		allowed, err = s.highStakesNotificationAllowed(ctx, learnerID, domainID)
		if err != nil {
			return nil, false, err
		}
		if !allowed {
			return availability, false, nil
		}
	}
	return availability, true, nil
}

func (s *Store) CompleteNotificationDelivery(ctx context.Context, reservationID int64, learnerID string) error {
	result, err := s.exec(ctx,
		`UPDATE scheduled_alerts SET sent = 1, delivery_state = 'delivered'
		 WHERE id = ? AND learner_id = ? AND sent = 0 AND delivery_state = 'reserved'`,
		reservationID, learnerID,
	)
	if err != nil {
		return fmt.Errorf("complete notification delivery: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete notification delivery rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("notification reservation not found")
	}
	return nil
}

func (s *Store) ReleaseNotificationDelivery(ctx context.Context, reservationID int64, learnerID string) error {
	if reservationID == 0 {
		return nil
	}
	_, err := s.exec(ctx,
		`DELETE FROM scheduled_alerts
		 WHERE id = ? AND learner_id = ? AND sent = 0 AND delivery_state = 'reserved'`,
		reservationID, learnerID,
	)
	if err != nil {
		return fmt.Errorf("release notification delivery: %w", err)
	}
	return nil
}

// highStakesNotificationAllowed is deliberately conservative for a global
// push: every active high-stakes domain must have at least one trusted,
// human-reviewed evaluation. Domain-scoped pushes check only that owned domain.
func (s *Store) highStakesNotificationAllowed(ctx context.Context, learnerID, domainID string) (bool, error) {
	if domainID != "" {
		var highStakes int
		err := s.queryRow(ctx,
			`SELECT high_stakes FROM domains
			 WHERE id = ? AND learner_id = ? AND archived = 0 AND deleted_at IS NULL`,
			domainID, learnerID,
		).Scan(&highStakes)
		if err != nil {
			if err == sql.ErrNoRows {
				return false, fmt.Errorf("notification domain not found")
			}
			return false, fmt.Errorf("check notification domain safety: %w", err)
		}
		if highStakes == 0 {
			return true, nil
		}
		return s.HasHumanReviewedEvaluationInDomain(ctx, learnerID, domainID)
	}

	var unreviewed int
	err := s.queryRow(ctx,
		`SELECT COUNT(*)
		 FROM domains d
		 WHERE d.learner_id = ? AND d.archived = 0 AND d.deleted_at IS NULL
		   AND d.high_stakes = 1
		   AND NOT EXISTS (
		       SELECT 1 FROM assessment_attempts a
		       WHERE a.learner_id = d.learner_id AND a.domain_id = d.id
		         AND a.status = 'evaluated' AND a.trusted_evaluation = 1
		         AND a.evaluation_method = 'human_review'
		   )`,
		learnerID,
	).Scan(&unreviewed)
	if err != nil {
		return false, fmt.Errorf("check global high-stakes notification policy: %w", err)
	}
	return unreviewed == 0, nil
}
