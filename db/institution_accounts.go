// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// ---------------------------------------------------------------------------
// Member administration
// ---------------------------------------------------------------------------

var errMembershipManageDenied = fmt.Errorf("%w: membership management denied", storeport.ErrInvalidPrincipal)

// UpdateTenantMember changes the status and roles of a membership. Only an
// owner grants or removes the owner role, nobody suspends or removes their own
// membership, and the tenant always keeps one active owner.
func (s *Store) UpdateTenantMember(ctx context.Context, actor models.Principal, membershipID, status string, roles []string) error {
	if !actor.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return errMembershipManageDenied
	}
	switch status {
	case models.MembershipStatusActive, models.MembershipStatusSuspended, models.MembershipStatusRevoked:
	default:
		return fmt.Errorf("update member: invalid status")
	}
	rolesJSON, err := validatedRolesJSON(roles)
	if err != nil {
		return fmt.Errorf("update member: %w", err)
	}
	if slices.Contains(roles, models.RoleSupport) {
		return fmt.Errorf("update member: invalid role %q", models.RoleSupport)
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if txs.dialect == DialectPostgres {
			if err := txs.lockMembershipChanges(txCtx, actor.TenantID); err != nil {
				return err
			}
			// Membership changes of one tenant run one at a time, so two
			// concurrent demotions cannot both see the other owner.
			if _, err := txs.exec(txCtx, `SELECT id FROM tenants WHERE id = ? FOR UPDATE`, actor.TenantID); err != nil {
				return err
			}
		}
		var userID, learnerID, currentRolesJSON, currentStatus string
		err := txs.queryRow(txCtx, `SELECT user_id, COALESCE(learner_id, ''), roles_json, status
			FROM tenant_memberships WHERE tenant_id = ? AND id = ? AND status <> 'revoked'`,
			actor.TenantID, membershipID).Scan(&userID, &learnerID, &currentRolesJSON, &currentStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return storeport.WrapNotFound(err)
		}
		if err != nil {
			return err
		}
		var currentRoles []string
		if err := json.Unmarshal([]byte(currentRolesJSON), &currentRoles); err != nil {
			return err
		}
		wasOwner := slices.Contains(currentRoles, models.RoleOwner)
		willOwn := slices.Contains(roles, models.RoleOwner)
		if (wasOwner || willOwn) && !slices.Contains(actor.Roles, models.RoleOwner) {
			return errMembershipManageDenied
		}
		if membershipID == actor.MembershipID && status != models.MembershipStatusActive {
			return fmt.Errorf("update member: you cannot suspend or remove your own membership")
		}
		if wasOwner && (!willOwn || status != models.MembershipStatusActive) {
			otherOwners, err := txs.countActiveOwners(txCtx, actor.TenantID, membershipID)
			if err != nil {
				return err
			}
			if otherOwners == 0 {
				return fmt.Errorf("update member: the institution must keep an active owner")
			}
		}
		scope := models.TenantScope{TenantID: actor.TenantID, UserID: userID, MembershipID: membershipID}
		if slices.Contains(roles, models.RoleLearner) && learnerID == "" {
			if err := txs.createInvitedLearnerProfile(txCtx, scope, time.Now().UTC()); err != nil {
				return err
			}
		}
		if _, err := txs.SetMembershipAuthorization(txCtx, scope, status, roles); err != nil {
			return err
		}
		details, _ := json.Marshal(map[string]any{"status": status, "roles": json.RawMessage(rolesJSON)})
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "membership.update", TargetType: "membership", TargetID: membershipID,
			DetailsJSON: string(details),
		})
	})
}

func (s *Store) countActiveOwners(ctx context.Context, tenantID, excludedMembershipID string) (int, error) {
	rows, err := s.query(ctx, `SELECT roles_json FROM tenant_memberships
		WHERE tenant_id = ? AND id <> ? AND status = 'active'`, tenantID, excludedMembershipID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	owners := 0
	for rows.Next() {
		var rolesJSON string
		var roles []string
		if err := rows.Scan(&rolesJSON); err != nil {
			return 0, err
		}
		if err := json.Unmarshal([]byte(rolesJSON), &roles); err != nil {
			return 0, err
		}
		if slices.Contains(roles, models.RoleOwner) {
			owners++
		}
	}
	return owners, rows.Err()
}

// createInvitedLearnerProfile attaches a learner profile to an existing
// membership. learners.email is globally unique, so tenant profiles use a
// synthetic address; the user's real address stays on users.
func (s *Store) createInvitedLearnerProfile(ctx context.Context, scope models.TenantScope, verifiedAt time.Time) error {
	learnerID, err := generateID()
	if err != nil {
		return err
	}
	var identityMode, displayName string
	if err := s.queryRow(ctx, `SELECT u.identity_mode,COALESCE(n.display_name,'') FROM users u LEFT JOIN native_identities n ON n.user_id=u.id WHERE u.id=?`, scope.UserID).Scan(&identityMode, &displayName); err != nil {
		return err
	}
	if identityMode == "pubkey" {
		profile, _ := json.Marshal(map[string]string{"name": displayName})
		if _, err := s.exec(ctx, `INSERT INTO learners(id,email,password_hash,objective,profile_json,created_at,last_active,email_verified_at,tenant_id,user_id,membership_id,identity_mode) VALUES(?,NULL,NULL,'',?,?,?,NULL,?,?,?,'pubkey')`, learnerID, string(profile), verifiedAt, verifiedAt, scope.TenantID, scope.UserID, scope.MembershipID); err != nil {
			return err
		}
		_, err = s.exec(ctx, `UPDATE tenant_memberships SET learner_id=? WHERE tenant_id=? AND id=? AND user_id=? AND learner_id IS NULL`, learnerID, scope.TenantID, scope.MembershipID, scope.UserID)
		return err
	}
	syntheticEmail := scope.TenantID + "+" + learnerID + "@profile.invalid"
	if _, err := s.exec(ctx, `INSERT INTO learners
		(id, email, password_hash, objective, profile_json, created_at, email_verified_at,
		 tenant_id, user_id, membership_id)
		VALUES (?, ?, '', '', '{}', ?, ?, ?, ?, ?)`, learnerID, syntheticEmail, verifiedAt,
		verifiedAt, scope.TenantID, scope.UserID, scope.MembershipID); err != nil {
		return fmt.Errorf("create learner profile: %w", err)
	}
	if _, err := s.exec(ctx, `UPDATE tenant_memberships SET learner_id = ?
		WHERE tenant_id = ? AND id = ? AND user_id = ? AND learner_id IS NULL`,
		learnerID, scope.TenantID, scope.MembershipID, scope.UserID); err != nil {
		return fmt.Errorf("attach learner profile: %w", err)
	}
	return nil
}

// attachTenantPlan starts the tenant's subscription and entitlements.
func (s *Store) attachTenantPlan(ctx context.Context, tenantID, planID string, now time.Time) error {
	var entitlementsJSON string
	if err := s.queryRow(ctx, `SELECT entitlements_json FROM plans
		WHERE id = ? AND status = 'active'`, planID).Scan(&entitlementsJSON); err != nil {
		return fmt.Errorf("tenant plan: %w", err)
	}
	periodEnd := now.AddDate(0, 1, 0)
	if _, err := s.exec(ctx, `INSERT INTO tenant_subscriptions
		(tenant_id, plan_id, status, current_period_start, current_period_end, updated_at)
		VALUES (?, ?, 'active', ?, ?, ?)
		ON CONFLICT (tenant_id) DO NOTHING`, tenantID, planID, now, periodEnd, now); err != nil {
		return err
	}
	var entitlements map[string]int64
	if err := json.Unmarshal([]byte(entitlementsJSON), &entitlements); err != nil {
		return fmt.Errorf("tenant entitlements: %w", err)
	}
	for key, limit := range entitlements {
		if _, err := s.exec(ctx, `INSERT INTO tenant_entitlements
			(tenant_id, entitlement_key, hard_limit, used_value, reserved_value,
			 period_start, period_end, version, updated_at)
			VALUES (?, ?, ?, 0, 0, ?, ?, 1, ?)
			ON CONFLICT (tenant_id, entitlement_key) DO NOTHING`,
			tenantID, key, limit, now, periodEnd, now); err != nil {
			return err
		}
	}
	return nil
}
