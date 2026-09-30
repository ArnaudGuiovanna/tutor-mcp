// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (s *Store) setFormationAdmission(ctx context.Context, tenantID, cohortID, membershipID, status string) error {
	_, err := s.exec(ctx, `INSERT INTO formation_admissions (tenant_id, cohort_id, membership_id, status, version, updated_at)
 VALUES (?, ?, ?, ?, 1, ?) ON CONFLICT (tenant_id, cohort_id, membership_id)
 DO UPDATE SET status = excluded.status, version = formation_admissions.version + 1, updated_at = excluded.updated_at`,
		tenantID, cohortID, membershipID, status, time.Now().UTC())
	return err
}

func (s *Store) ListFormationAdmissions(ctx context.Context, actor models.Principal, cohortID, after string, limit int) ([]models.FormationAdmission, error) {
	out := []models.FormationAdmission{}
	if err := validatePage(limit); err != nil {
		return out, err
	}
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		formation, err := txs.formationAccess(txCtx, actor, "cohort", cohortID, models.PermissionCohortManage, false)
		if err != nil {
			return err
		}
		rows, err := txs.query(txCtx, `SELECT a.membership_id, a.status, a.version, a.updated_at, u.email FROM formation_admissions a
		 JOIN tenant_memberships tm ON tm.tenant_id = a.tenant_id AND tm.id = a.membership_id JOIN users u ON u.id = tm.user_id
		 WHERE a.tenant_id = ? AND a.cohort_id = ? AND a.membership_id > ? ORDER BY a.membership_id LIMIT ?`, actor.TenantID, cohortID, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a := models.FormationAdmission{CohortID: cohortID, EnrollmentPolicy: formation.EnrollmentPolicy}
			if err := rows.Scan(&a.MembershipID, &a.Status, &a.Version, &a.UpdatedAt, &a.Email); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// Resolve only an explicitly supplied learner address after cohort authorization.
// Formation managers do not receive a directory of unrelated memberships.
func (s *Store) FindFormationLearner(ctx context.Context, actor models.Principal, cohortID, email string) (string, error) {
	var id string
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.formationAccess(txCtx, actor, "cohort", cohortID, models.PermissionCohortManage, false); err != nil {
			return err
		}
		var rolesJSON string
		if err := txs.queryRow(txCtx, `SELECT tm.id, tm.roles_json FROM tenant_memberships tm JOIN users u ON u.id = tm.user_id
 WHERE tm.tenant_id = ? AND tm.status = 'active' AND u.status = 'active' AND u.email = ?`, actor.TenantID, strings.ToLower(strings.TrimSpace(email))).Scan(&id, &rolesJSON); err != nil {
			return storeport.ErrInvalidPrincipal
		}
		var roles []string
		if err := json.Unmarshal([]byte(rolesJSON), &roles); err != nil {
			return err
		}
		if !containsRole(roles, models.RoleLearner) {
			return storeport.ErrInvalidPrincipal
		}
		return nil
	})
	return id, err
}

func (s *Store) DecideFormationAdmission(ctx context.Context, actor models.Principal, key, cohortID, membershipID, decision string, expectedVersion int64) (*models.FormationAdmission, bool, error) {
	request := struct {
		CohortID, MembershipID, Decision string
		ExpectedVersion                  int64
	}{cohortID, membershipID, decision, expectedVersion}
	return runCatalogMutation(ctx, s, actor, key, "formation.admission.decide", models.PermissionCohortManage, request,
		func(txCtx context.Context, txs *Store) (*models.FormationAdmission, error) {
			f, err := txs.formationAccess(txCtx, actor, "cohort", cohortID, models.PermissionCohortManage, true)
			if err != nil {
				return nil, err
			}
			if f.Status == "archived" && decision != "revoked" {
				return nil, storeport.ErrFormationUnavailable
			}
			var rolesJSON string
			if err := txs.queryRow(txCtx, `SELECT roles_json FROM tenant_memberships WHERE tenant_id = ? AND id = ? AND status = 'active'`, actor.TenantID, membershipID).Scan(&rolesJSON); err != nil {
				return nil, storeport.ErrInvalidPrincipal
			}
			var roles []string
			if err := json.Unmarshal([]byte(rolesJSON), &roles); err != nil {
				return nil, err
			}
			if !containsRole(roles, models.RoleLearner) {
				return nil, storeport.ErrInvalidPrincipal
			}
			a := &models.FormationAdmission{CohortID: cohortID, MembershipID: membershipID}
			err = txs.queryRow(txCtx, `SELECT status, version FROM formation_admissions WHERE tenant_id = ? AND cohort_id = ? AND membership_id = ?`, actor.TenantID, cohortID, membershipID).Scan(&a.Status, &a.Version)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if expectedVersion != a.Version {
				return nil, storeport.ErrEnrollmentTransition
			}
			switch decision {
			case "invited":
				if f.EnrollmentPolicy != "invitation" {
					return nil, storeport.ErrEnrollmentTransition
				}
			case "approved", "rejected":
				if f.EnrollmentPolicy != "approval" || (a.Status != "pending" && !(decision == "approved" && a.Status == "revoked")) {
					return nil, storeport.ErrEnrollmentTransition
				}
			case "revoked":
				if a.Version == 0 {
					return nil, storeport.ErrEnrollmentTransition
				}
			default:
				return nil, storeport.ErrEnrollmentTransition
			}
			if err := txs.setFormationAdmission(txCtx, actor.TenantID, cohortID, membershipID, decision); err != nil {
				return nil, err
			}
			if err := txs.queryRow(txCtx, `SELECT status, version, updated_at FROM formation_admissions WHERE tenant_id = ? AND cohort_id = ? AND membership_id = ?`, actor.TenantID, cohortID, membershipID).Scan(&a.Status, &a.Version, &a.UpdatedAt); err != nil {
				return nil, err
			}
			if err := txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "enrollment.admission." + decision, TargetType: "membership", TargetID: membershipID, DetailsJSON: `{"cohort_id":` + strconvQuote(cohortID) + `}`}); err != nil {
				return nil, err
			}
			return a, nil
		})
}
