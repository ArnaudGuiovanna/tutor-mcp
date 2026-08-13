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
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (s *Store) GetLocalUserByEmail(ctx context.Context, normalizedEmail string) (*models.User, error) {
	normalizedEmail = strings.TrimSpace(strings.ToLower(normalizedEmail))
	if normalizedEmail == "" {
		return nil, storeport.WrapNotFound(sql.ErrNoRows)
	}
	rows, err := s.query(ctx, `SELECT id, email, normalized_email, password_hash,
               status, email_verified_at, token_version, created_at, updated_at
        FROM users WHERE normalized_email = ? ORDER BY id LIMIT 2`, normalizedEmail)
	if err != nil {
		return nil, fmt.Errorf("get local user: %w", err)
	}
	defer rows.Close()
	var matches []models.User
	for rows.Next() {
		var user models.User
		var verifiedAt sql.NullTime
		if err := rows.Scan(&user.ID, &user.Email, &user.NormalizedEmail, &user.PasswordHash,
			&user.Status, &verifiedAt, &user.TokenVersion, &user.CreatedAt, &user.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan local user: %w", err)
		}
		if verifiedAt.Valid {
			stamp := verifiedAt.Time
			user.EmailVerifiedAt = &stamp
		}
		matches = append(matches, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get local user: %w", err)
	}
	if len(matches) == 0 {
		return nil, storeport.WrapNotFound(sql.ErrNoRows)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("get local user: %w", storeport.ErrAmbiguousIdentity)
	}
	return &matches[0], nil
}

func (s *Store) GetPrincipalForLearner(ctx context.Context, learnerID string, scopes []string) (models.Principal, error) {
	if strings.TrimSpace(learnerID) == "" {
		return models.Principal{}, fmt.Errorf("get principal: %w", storeport.ErrInvalidPrincipal)
	}
	legacy := models.LegacyPrincipal(learnerID).TenantScope()
	return s.GetPrincipal(ctx, legacy, scopes)
}

func (s *Store) GetPrincipal(ctx context.Context, scope models.TenantScope, scopes []string) (models.Principal, error) {
	if err := scope.Validate(); err != nil {
		return models.Principal{}, fmt.Errorf("get principal: %w", storeport.ErrInvalidPrincipal)
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var principal models.Principal
		err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			principal, innerErr = scoped.GetPrincipal(txCtx, scope, scopes)
			return innerErr
		})
		return principal, err
	}
	var principal models.Principal
	var rolesJSON string
	err := s.queryRow(ctx, `SELECT l.user_id, l.tenant_id, l.membership_id, l.id,
               tm.roles_json, tm.version
        FROM learners l
        JOIN users u ON u.id = l.user_id AND u.status = 'active'
        JOIN tenants t ON t.id = l.tenant_id AND t.status = 'active'
        JOIN tenant_memberships tm
          ON tm.id = l.membership_id
         AND tm.tenant_id = l.tenant_id
         AND tm.user_id = l.user_id
         AND tm.learner_id = l.id
         AND tm.status = 'active'
		 AND (tm.mfa_required = 0 OR tm.mfa_verified_at IS NOT NULL)
        WHERE l.tenant_id = ? AND l.user_id = ? AND l.membership_id = ? AND l.id = ?`,
		scope.TenantID, scope.UserID, scope.MembershipID, scope.LearnerID).Scan(
		&principal.UserID, &principal.TenantID, &principal.MembershipID,
		&principal.LearnerID, &rolesJSON, &principal.TokenVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Principal{}, fmt.Errorf("get principal: %w", storeport.ErrInvalidPrincipal)
	}
	if err != nil {
		return models.Principal{}, fmt.Errorf("get principal: %w", err)
	}
	if err := json.Unmarshal([]byte(rolesJSON), &principal.Roles); err != nil {
		return models.Principal{}, fmt.Errorf("get principal roles: %w", err)
	}
	principal.Scopes = append([]string(nil), scopes...)
	if err := principal.Validate(); err != nil {
		return models.Principal{}, fmt.Errorf("get principal: %w", storeport.ErrInvalidPrincipal)
	}
	return principal, nil
}

func (s *Store) ValidatePrincipal(ctx context.Context, principal models.Principal) error {
	if err := principal.Validate(); err != nil {
		return fmt.Errorf("validate principal: %w", storeport.ErrInvalidPrincipal)
	}
	if slices.Contains(principal.Roles, models.RoleServiceAccount) {
		if !s.inTenantTransaction(ctx, principal.TenantScope()) && s.root != nil {
			return s.WithTenantTx(ctx, principal.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
				return scoped.(*Store).validateServiceAccountPrincipal(txCtx, principal)
			})
		}
		return s.validateServiceAccountPrincipal(ctx, principal)
	}
	if slices.Contains(principal.Roles, models.RoleSupport) {
		if !s.inTenantTransaction(ctx, principal.TenantScope()) && s.root != nil {
			return s.WithTenantTx(ctx, principal.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
				return scoped.(*Store).validateSupportPrincipal(txCtx, principal)
			})
		}
		return s.validateSupportPrincipal(ctx, principal)
	}
	if !s.inTenantTransaction(ctx, principal.TenantScope()) && s.root != nil {
		return s.WithTenantTx(ctx, principal.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
			return scoped.ValidatePrincipal(txCtx, principal)
		})
	}
	var rolesJSON string
	var version int64
	err := s.queryRow(ctx, `SELECT tm.roles_json, tm.version
        FROM tenant_memberships tm
        JOIN tenants t ON t.id = tm.tenant_id AND t.status = 'active'
        JOIN users u ON u.id = tm.user_id AND u.status = 'active'
        WHERE tm.tenant_id = ?
          AND tm.id = ?
          AND tm.user_id = ?
          AND tm.status = 'active'
		  AND (tm.mfa_required = 0 OR tm.mfa_verified_at IS NOT NULL)
          AND tm.version = ?
          AND ((? = '' AND tm.learner_id IS NULL) OR tm.learner_id = ?)`,
		principal.TenantID, principal.MembershipID, principal.UserID,
		principal.TokenVersion, principal.LearnerID, principal.LearnerID,
	).Scan(&rolesJSON, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("validate principal: %w", storeport.ErrInvalidPrincipal)
	}
	if err != nil {
		return fmt.Errorf("validate principal: %w", err)
	}
	var currentRoles []string
	if err := json.Unmarshal([]byte(rolesJSON), &currentRoles); err != nil {
		return fmt.Errorf("validate principal roles: %w", err)
	}
	if version != principal.TokenVersion || !slices.Equal(currentRoles, principal.Roles) {
		return fmt.Errorf("validate principal: %w", storeport.ErrInvalidPrincipal)
	}
	return nil
}

func (s *Store) ListActiveMembershipsForUser(ctx context.Context, userID string) ([]models.TenantMembership, error) {
	return s.listMembershipsForUser(ctx, userID, true)
}

func (s *Store) ListMembershipsForUser(ctx context.Context, userID string) ([]models.TenantMembership, error) {
	return s.listMembershipsForUser(ctx, userID, false)
}

func (s *Store) listMembershipsForUser(ctx context.Context, userID string, activeOnly bool) ([]models.TenantMembership, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("list memberships: user_id is required")
	}
	if s.dialect == DialectPostgres && s.root != nil {
		var memberships []models.TenantMembership
		err := s.withIdentityTx(ctx, userID, func(txs *Store) error {
			var innerErr error
			memberships, innerErr = txs.listMembershipsForUser(ctx, userID, activeOnly)
			return innerErr
		})
		return memberships, err
	}
	query := `SELECT tm.id, tm.tenant_id, t.name, tm.user_id,
               COALESCE(tm.learner_id, ''), tm.roles_json, tm.status,
               tm.version, tm.created_at, tm.updated_at
        FROM tenant_memberships tm
        JOIN tenants t ON t.id = tm.tenant_id AND t.status = 'active'
		WHERE tm.user_id = ? AND tm.status <> 'revoked'`
	if activeOnly {
		query += ` AND tm.status = 'active'`
	}
	query += ` ORDER BY tm.tenant_id, tm.id`
	rows, err := s.query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()
	var memberships []models.TenantMembership
	for rows.Next() {
		var membership models.TenantMembership
		var rolesJSON string
		if err := rows.Scan(&membership.ID, &membership.TenantID, &membership.TenantName, &membership.UserID,
			&membership.LearnerID, &rolesJSON, &membership.Status, &membership.Version,
			&membership.CreatedAt, &membership.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		if err := json.Unmarshal([]byte(rolesJSON), &membership.Roles); err != nil {
			return nil, fmt.Errorf("scan membership roles: %w", err)
		}
		memberships = append(memberships, membership)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	return memberships, nil
}

func (s *Store) withIdentityTx(ctx context.Context, userID string, fn func(*Store) error) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("identity transaction: user_id is required")
	}
	if s.root == nil || s.dialect != DialectPostgres {
		return fn(s)
	}
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("identity transaction: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.identity_user', $1, true)`, userID); err != nil {
		return fmt.Errorf("identity transaction: bind user: %w", err)
	}
	if err := fn(&Store{db: tx, root: nil, dialect: s.dialect, secretKeyring: s.secretKeyring}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("identity transaction: commit: %w", err)
	}
	return nil
}

func (s *Store) SetMembershipAuthorization(ctx context.Context, scope models.TenantScope, status string, roles []string) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, fmt.Errorf("set membership authorization: %w", err)
	}
	switch status {
	case models.MembershipStatusInvited, models.MembershipStatusActive,
		models.MembershipStatusSuspended, models.MembershipStatusRevoked:
	default:
		return 0, fmt.Errorf("set membership authorization: invalid status")
	}
	if len(roles) == 0 {
		return 0, fmt.Errorf("set membership authorization: roles are required")
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var version int64
		err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			version, innerErr = scoped.SetMembershipAuthorization(txCtx, scope, status, roles)
			return innerErr
		})
		return version, err
	}
	seen := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		if !models.ValidTenantRole(role) {
			return 0, fmt.Errorf("set membership authorization: invalid role %q", role)
		}
		if _, duplicate := seen[role]; duplicate {
			return 0, fmt.Errorf("set membership authorization: duplicate role %q", role)
		}
		seen[role] = struct{}{}
	}
	rolesJSON, err := json.Marshal(roles)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	mfaRequired := 0
	if slices.Contains(roles, models.RoleOwner) || slices.Contains(roles, models.RoleAdmin) {
		mfaRequired = 1
	}
	var version int64
	err = s.queryRow(ctx, `UPDATE tenant_memberships
		SET status = ?, roles_json = ?, mfa_required = ?,
		    version = version + 1, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND user_id = ?
		RETURNING version`, status, string(rolesJSON), mfaRequired, now,
		scope.TenantID, scope.MembershipID, scope.UserID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, storeport.WrapNotFound(err)
	}
	if err != nil {
		return 0, fmt.Errorf("set membership authorization: %w", err)
	}
	return version, nil
}

func (s *Store) RecordMembershipMFAVerification(ctx context.Context, scope models.TenantScope, verifiedAt time.Time) (int64, error) {
	if err := scope.Validate(); err != nil || verifiedAt.IsZero() {
		return 0, fmt.Errorf("record MFA verification: invalid scope or timestamp")
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var version int64
		err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			version, innerErr = scoped.RecordMembershipMFAVerification(txCtx, scope, verifiedAt)
			return innerErr
		})
		return version, err
	}
	var version int64
	err := s.queryRow(ctx, `UPDATE tenant_memberships
		SET mfa_verified_at = ?, version = version + 1, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND user_id = ? AND status = 'active'
		RETURNING version`, verifiedAt.UTC(), verifiedAt.UTC(), scope.TenantID,
		scope.MembershipID, scope.UserID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, storeport.WrapNotFound(err)
	}
	if err != nil {
		return 0, fmt.Errorf("record MFA verification: %w", err)
	}
	return version, nil
}

func (s *Store) inTenantTransaction(ctx context.Context, scope models.TenantScope) bool {
	if s.tenantScope != nil {
		return *s.tenantScope == scope
	}
	if scoped, ok := ctx.Value(tenantTransactionContextKey{}).(*tenantTransactionContext); ok {
		return scoped.scope == scope && (s.root == nil || scoped.root == s.root)
	}
	return false
}
