// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// EnableNativeLearning is a member's explicit opt-in to their own real learner
// profile. It grants only learner, preserves staff roles, and never enrolls the
// person in a formation. A fresh signed session is required after the change.
func (s *Store) EnableNativeLearning(ctx context.Context, actor models.Principal) (*models.NativeLearningMembership, error) {
	var result *models.NativeLearningMembership
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if err := txs.ValidatePrincipal(txCtx, actor); err != nil {
			return err
		}
		query := `SELECT m.learner_id,m.roles_json,n.display_name FROM tenant_memberships m JOIN users u ON u.id=m.user_id JOIN native_identities n ON n.user_id=m.user_id WHERE m.tenant_id=? AND m.id=? AND m.user_id=? AND m.status='active' AND u.identity_mode='pubkey' AND n.status='active'`
		if txs.dialect == DialectPostgres {
			query += " FOR UPDATE OF m"
		}
		var learner sql.NullString
		var raw, name string
		if err := txs.queryRow(txCtx, query, actor.TenantID, actor.MembershipID, actor.UserID).Scan(&learner, &raw, &name); err != nil {
			return storeport.ErrInvalidPrincipal
		}
		var roles []string
		if err := json.Unmarshal([]byte(raw), &roles); err != nil {
			return err
		}
		if learner.Valid && learner.String != "" && slices.Contains(roles, models.RoleLearner) {
			result = &models.NativeLearningMembership{LearnerID: learner.String, Roles: roles}
			return nil
		}
		now := time.Now().UTC()
		id := learner.String
		if id == "" {
			var err error
			id, err = generateID()
			if err != nil {
				return err
			}
			profile, _ := json.Marshal(map[string]string{"name": name})
			if _, err = txs.exec(txCtx, `INSERT INTO learners(id,email,password_hash,objective,profile_json,created_at,last_active,email_verified_at,tenant_id,user_id,membership_id,identity_mode) VALUES(?,NULL,NULL,'',?,?,?,NULL,?,?,?,'pubkey')`, id, string(profile), now, now, actor.TenantID, actor.UserID, actor.MembershipID); err != nil {
				return err
			}
		}
		if !slices.Contains(roles, models.RoleLearner) {
			roles = append(roles, models.RoleLearner)
		}
		updated, _ := json.Marshal(roles)
		if err := txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "membership.learning_enabled", TargetType: "membership", TargetID: actor.MembershipID, Reason: "member explicitly enabled their own learner space"}); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE tenant_memberships SET learner_id=?,roles_json=?,version=version+1,updated_at=? WHERE tenant_id=? AND id=? AND user_id=?`, id, string(updated), now, actor.TenantID, actor.MembershipID, actor.UserID); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE native_sessions SET revoked_at=? WHERE tenant_id=? AND membership_id=? AND revoked_at IS NULL`, now, actor.TenantID, actor.MembershipID); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE native_challenges SET consumed_at=? WHERE tenant_id=? AND user_id=? AND consumed_at IS NULL`, now, actor.TenantID, actor.UserID); err != nil {
			return err
		}
		result = &models.NativeLearningMembership{LearnerID: id, Roles: roles, Changed: true}
		return nil
	})
	return result, err
}
