// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const (
	credentialKindAuthorizationCode = "authorization_code"
	credentialKindRefreshToken      = "refresh_token"
	credentialKindEmailVerification = "email_verification"
	credentialKindPasswordReset     = "password_reset"
	credentialKindLoginChallenge    = "login_challenge"
)

func accountCredentialKind(purpose string) string {
	if purpose == accountTokenEmailVerification {
		return credentialKindEmailVerification
	}
	if purpose == accountTokenPasswordReset {
		return credentialKindPasswordReset
	}
	return ""
}

func (s *Store) credentialPrincipalForLearner(ctx context.Context, learnerID string, scopes []string) (models.Principal, error) {
	tenantScope, err := s.credentialScopeForLearner(ctx, learnerID)
	if err != nil {
		return models.Principal{}, err
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var principal models.Principal
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, _ storeport.Store) error {
			var innerErr error
			principal, innerErr = s.credentialPrincipalForLearner(txCtx, learnerID, scopes)
			return innerErr
		})
		return principal, err
	}
	var rolesJSON, membershipStatus, tenantStatus, userStatus string
	principal := models.Principal{
		UserID: tenantScope.UserID, TenantID: tenantScope.TenantID,
		MembershipID: tenantScope.MembershipID, LearnerID: tenantScope.LearnerID,
		Scopes: append([]string(nil), scopes...),
	}
	err = s.queryRow(ctx, `SELECT tm.roles_json, tm.version, tm.status, t.status, u.status
        FROM tenant_memberships tm
        JOIN tenants t ON t.id = tm.tenant_id
        JOIN users u ON u.id = tm.user_id
        WHERE tm.tenant_id = ? AND tm.id = ? AND tm.user_id = ? AND tm.learner_id = ?`,
		tenantScope.TenantID, tenantScope.MembershipID, tenantScope.UserID, tenantScope.LearnerID,
	).Scan(&rolesJSON, &principal.TokenVersion, &membershipStatus, &tenantStatus, &userStatus)
	if err != nil {
		return models.Principal{}, err
	}
	if tenantStatus != models.TenantStatusActive ||
		(membershipStatus != models.MembershipStatusActive && membershipStatus != models.MembershipStatusInvited) ||
		(userStatus != models.UserStatusActive && userStatus != models.UserStatusPending) {
		return models.Principal{}, storeport.ErrInvalidPrincipal
	}
	if err := json.Unmarshal([]byte(rolesJSON), &principal.Roles); err != nil {
		return models.Principal{}, err
	}
	if err := principal.Validate(); err != nil {
		return models.Principal{}, err
	}
	return principal, nil
}

func credentialRouteKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Store) insertCredentialRoute(ctx context.Context, kind, raw string, scope models.TenantScope, expiresAt, createdAt time.Time) error {
	if err := scope.Validate(); err != nil || scope.LearnerID == "" {
		return fmt.Errorf("insert credential route: invalid tenant scope")
	}
	_, err := s.exec(ctx, `INSERT INTO credential_tenant_routes
        (kind, credential_key, tenant_id, user_id, membership_id, learner_id, expires_at, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, credentialRouteKey(raw), scope.TenantID, scope.UserID,
		scope.MembershipID, scope.LearnerID, expiresAt, createdAt)
	if err != nil {
		return fmt.Errorf("insert credential route: %w", err)
	}
	return nil
}

func (s *Store) credentialScope(ctx context.Context, kind, raw string) (models.TenantScope, error) {
	var scope models.TenantScope
	err := s.queryRow(ctx, `SELECT tenant_id, user_id, membership_id, learner_id
        FROM credential_tenant_routes
		WHERE kind = ? AND credential_key = ?`,
		kind, credentialRouteKey(raw)).Scan(
		&scope.TenantID, &scope.UserID, &scope.MembershipID, &scope.LearnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return models.TenantScope{}, storeport.WrapNotFound(err)
	}
	if err != nil {
		return models.TenantScope{}, fmt.Errorf("lookup credential route: %w", err)
	}
	if err := scope.Validate(); err != nil {
		return models.TenantScope{}, fmt.Errorf("lookup credential route: invalid scope: %w", err)
	}
	return scope, nil
}

func (s *Store) deleteCredentialRoute(ctx context.Context, kind, raw string) error {
	_, err := s.exec(ctx, `DELETE FROM credential_tenant_routes WHERE kind = ? AND credential_key = ?`,
		kind, credentialRouteKey(raw))
	if err != nil {
		return fmt.Errorf("delete credential route: %w", err)
	}
	return nil
}

// credentialScopeForLearner is the compatibility resolver used while account
// registration still creates a learner and its tenant roots together. It does
// not require an active membership, because email verification starts from the
// intentional `invited` state.
func (s *Store) credentialScopeForLearner(ctx context.Context, learnerID string) (models.TenantScope, error) {
	legacy := models.LegacyPrincipal(learnerID).TenantScope()
	if !s.inTenantTransaction(ctx, legacy) && s.root != nil {
		var resolved models.TenantScope
		err := s.WithTenantTx(ctx, legacy, func(txCtx context.Context, _ storeport.Store) error {
			var innerErr error
			resolved, innerErr = s.credentialScopeForLearner(txCtx, learnerID)
			return innerErr
		})
		return resolved, err
	}
	var scope models.TenantScope
	err := s.queryRow(ctx, `SELECT tenant_id, user_id, membership_id, id
        FROM learners WHERE tenant_id = ? AND id = ?`, legacy.TenantID, learnerID).Scan(
		&scope.TenantID, &scope.UserID, &scope.MembershipID, &scope.LearnerID)
	if err != nil {
		return models.TenantScope{}, fmt.Errorf("resolve learner credential scope: %w", err)
	}
	if err := scope.Validate(); err != nil {
		return models.TenantScope{}, fmt.Errorf("resolve learner credential scope: %w", err)
	}
	return scope, nil
}
