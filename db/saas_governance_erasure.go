// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"time"
)

// These stores expose only routing identifiers to the worker. PostgreSQL's
// system ctid needs table-wide SELECT, so credential erasure uses a dedicated
// non-secret row identifier instead of widening access to token values.
var dsarCredentialTables = map[string]bool{
	"native_challenges": true, "native_sessions": true, "conversation_contexts": true, "training_proposals": true, "training_receipts": true,
	"refresh_tokens": true, "oauth_codes": true, "login_challenges": true,
	"account_tokens": true, "learner_approved_clients": true, "tool_call_idempotency": true,
}

func (s *Store) revokeErasedLearnerAccess(ctx context.Context, tenant, learner string, now time.Time) error {
	// The membership row is also the lock used by authenticated write
	// transactions. Wait for those to finish before the first erasure phase,
	// then make every existing bearer/browser session fail its live check.
	if _, err := s.exec(ctx, `UPDATE tenant_memberships SET status = 'revoked', version = version + 1, updated_at = ?
 WHERE tenant_id = ? AND learner_id = ? AND status <> 'revoked'`, now, tenant, learner); err != nil {
		return err
	}
	// Domain-before-enrollment locking matches leave/version migration. Closing
	// access at the start also prevents a staff migration from recreating state
	// after an earlier data phase has already deleted it.
	if _, err := s.exec(ctx, `UPDATE domains SET archived = 1 WHERE tenant_id = ? AND learner_id = ?`, tenant, learner); err != nil {
		return err
	}
	cohorts, err := s.erasedLearnerCohorts(ctx, tenant, learner)
	if err != nil {
		return err
	}
	if err := s.cancelErasedLearnerEnrollments(ctx, tenant, learner, now); err != nil {
		return err
	}
	for _, cohort := range cohorts {
		if err := s.recomputeCohortStatistics(ctx, tenant, cohort[0], cohort[1], now.UTC()); err != nil {
			return err
		}
	}
	return nil
}

// Release only reservations actually held by these enrollments. Both the
// enrollment flag and cohort counter commit together; concurrent workers and
// retries cannot release a place twice.
func (s *Store) cancelErasedLearnerEnrollments(ctx context.Context, tenant, learner string, now time.Time) error {
	query := `SELECT id, cohort_id, seat_reserved FROM enrollments
 WHERE tenant_id = ? AND learner_id = ? ORDER BY cohort_id, id`
	if s.dialect == DialectPostgres {
		query += ` FOR UPDATE`
	}
	rows, err := s.query(ctx, query, tenant, learner)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	var cohorts []string
	for rows.Next() {
		var id, cohort string
		var reserved int
		if err := rows.Scan(&id, &cohort, &reserved); err != nil {
			rows.Close()
			return err
		}
		if reserved == 1 {
			if counts[cohort] == 0 {
				cohorts = append(cohorts, cohort)
			}
			counts[cohort]++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, cohort := range cohorts {
		count := counts[cohort]
		result, err := s.exec(ctx, `UPDATE cohorts SET reserved_seats = reserved_seats - ?,
 version = version + 1, updated_at = ? WHERE tenant_id = ? AND id = ? AND reserved_seats >= ?`,
			count, now, tenant, cohort, count)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return fmt.Errorf("erase learner: inconsistent cohort reservations")
		}
	}
	_, err = s.exec(ctx, `UPDATE enrollments SET status = 'cancelled', seat_reserved = 0,
 objectives_json = '{}', updated_at = ? WHERE tenant_id = ? AND learner_id = ?`, now, tenant, learner)
	return err
}

// erasedLearnerCohorts lists the published institutional cohorts whose
// aggregates include the learner, as (cohort, version) pairs.
func (s *Store) erasedLearnerCohorts(ctx context.Context, tenant, learner string) ([][2]string, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT c.id, c.formation_version_id FROM enrollments e
 JOIN cohorts c ON c.tenant_id = e.tenant_id AND c.id = e.cohort_id
 JOIN formation_versions v ON v.tenant_id = c.tenant_id AND v.id = c.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 WHERE e.tenant_id = ? AND e.learner_id = ? AND v.status = 'published' AND `+institutionalFormationSQL, tenant, learner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}
