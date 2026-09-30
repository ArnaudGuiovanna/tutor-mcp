// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// SetCohortTrainerAssignment is an idempotent set operation, exposed only at
// the console boundary. Read-only progress tokens cannot change assignments.
func (s *Store) SetCohortTrainerAssignment(ctx context.Context, p models.Principal, cohort, email string, assigned bool) error {
	if !p.Authorize(models.PermissionCohortManage, models.AuthorizationResource{TenantID: p.TenantID}) ||
		(!models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeLearnerWrite) && !models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeFormationWrite)) {
		return storeport.ErrInvalidPrincipal
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if !validProgressID(cohort, false) || len(email) > 254 || !strings.Contains(email, "@") {
		return storeport.ErrInvalidProgressRequest
	}
	return s.WithTenantTx(ctx, p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if err := txs.ValidatePrincipal(ctx, p); err != nil {
			return err
		}
		if _, err := txs.formationAccess(ctx, p, "cohort", cohort, models.PermissionCohortManage, true); err != nil {
			if errors.Is(err, storeport.ErrInvalidPrincipal) {
				return storeport.ErrNotFound
			}
			return err
		}
		rolePredicate := `EXISTS (SELECT 1 FROM json_each(tm.roles_json) WHERE value = 'trainer')`
		if txs.dialect == DialectPostgres {
			rolePredicate = `jsonb_exists(tm.roles_json, 'trainer')`
		}
		eligible := `tm.status = 'active' AND u.status = 'active' AND ` + rolePredicate
		if !assigned {
			eligible = `1 = 1`
		} // Revocation must work after role/status changes too.
		var member string
		err := txs.queryRow(ctx, `SELECT tm.id FROM tenant_memberships tm JOIN users u ON u.id = tm.user_id
 WHERE tm.tenant_id = ? AND u.email = ? AND `+eligible, p.TenantID, email).Scan(&member)
		if errors.Is(err, sql.ErrNoRows) {
			return storeport.ErrNotFound
		}
		if err != nil {
			return err
		}
		var result sql.Result
		action := "cohort.trainer.revoke"
		if assigned {
			result, err = txs.exec(ctx, `INSERT INTO cohort_trainers (tenant_id, cohort_id, membership_id, assigned_by, assigned_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, p.TenantID, cohort, member, p.UserID, time.Now().UTC())
			action = "cohort.trainer.assign"
		} else {
			result, err = txs.exec(ctx, `DELETE FROM cohort_trainers WHERE tenant_id = ? AND cohort_id = ? AND membership_id = ?`, p.TenantID, cohort, member)
		}
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return nil
		}
		return txs.AppendAuditEvent(ctx, p, models.AuditEvent{Action: action, TargetType: "cohort", TargetID: cohort, DetailsJSON: `{"membership_id":` + strconvQuote(member) + `}`})
	})
}

func (s *Store) progressAssignments(ctx context.Context, p models.Principal, out *models.CohortInsights) error {
	_, err := s.formationAccess(ctx, p, "cohort", out.Cohort.CohortID, models.PermissionCohortManage, false)
	if errors.Is(err, storeport.ErrInvalidPrincipal) {
		return nil
	}
	if err != nil {
		return err
	}
	out.CanManageAssignments = true
	rows, err := s.query(ctx, `SELECT tm.id, u.email FROM cohort_trainers ct
 JOIN tenant_memberships tm ON tm.tenant_id = ct.tenant_id AND tm.id = ct.membership_id
 JOIN users u ON u.id = tm.user_id WHERE ct.tenant_id = ? AND ct.cohort_id = ? ORDER BY u.email LIMIT 101`, p.TenantID, out.Cohort.CohortID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var trainer models.CohortTrainer
		if err := rows.Scan(&trainer.MembershipID, &trainer.Email); err != nil {
			return err
		}
		out.Trainers = append(out.Trainers, trainer)
	}
	if len(out.Trainers) > 100 {
		out.Trainers = out.Trainers[:100]
		out.TrainersTruncated = true
	}
	return rows.Err()
}
