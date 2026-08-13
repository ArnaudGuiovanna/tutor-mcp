// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const loginChallengeColumns = `token_hash, user_id, tenant_id, membership_id, learner_id, client_id, redirect_uri, resource,
state, scope, code_challenge, code_challenge_method, expires_at, created_at,
consumed_at, trusted_until`

type loginChallengeScanner interface {
	Scan(dest ...any) error
}

func scanLoginChallenge(row loginChallengeScanner) (*models.LoginChallenge, error) {
	challenge := &models.LoginChallenge{}
	var consumedAt, trustedUntil sql.NullTime
	if err := row.Scan(
		&challenge.TokenHash, &challenge.UserID, &challenge.TenantID,
		&challenge.MembershipID, &challenge.LearnerID, &challenge.ClientID,
		&challenge.RedirectURI, &challenge.Resource, &challenge.State,
		&challenge.Scope, &challenge.CodeChallenge, &challenge.CodeChallengeMethod,
		&challenge.ExpiresAt, &challenge.CreatedAt, &consumedAt, &trustedUntil,
	); err != nil {
		return nil, err
	}
	if consumedAt.Valid {
		stamp := consumedAt.Time
		challenge.ConsumedAt = &stamp
	}
	if trustedUntil.Valid {
		stamp := trustedUntil.Time
		challenge.TrustedUntil = &stamp
	}
	return challenge, nil
}

func (s *Store) CreateLoginChallenge(ctx context.Context, challenge *models.LoginChallenge) (bool, error) {
	if challenge == nil || strings.TrimSpace(challenge.TokenHash) == "" ||
		strings.TrimSpace(challenge.LearnerID) == "" || strings.TrimSpace(challenge.ClientID) == "" ||
		strings.TrimSpace(challenge.RedirectURI) == "" || strings.TrimSpace(challenge.Resource) == "" {
		return false, fmt.Errorf("create login challenge: incomplete challenge")
	}
	canonicalScope, err := models.CanonicalOAuthScope(challenge.Scope)
	if err != nil || canonicalScope != challenge.Scope {
		return false, fmt.Errorf("create login challenge: %w", storeport.ErrInvalidOAuthScope)
	}
	if challenge.ExpiresAt.IsZero() || !challenge.ExpiresAt.After(challenge.CreatedAt) {
		return false, fmt.Errorf("create login challenge: invalid expiration")
	}
	tenantScope := models.TenantScope{
		TenantID: challenge.TenantID, UserID: challenge.UserID,
		MembershipID: challenge.MembershipID, LearnerID: challenge.LearnerID,
	}
	if tenantScope.Validate() != nil {
		tenantScope, err = s.credentialScopeForLearner(ctx, challenge.LearnerID)
		if err != nil {
			return false, fmt.Errorf("create login challenge: %w", err)
		}
		challenge.TenantID, challenge.UserID, challenge.MembershipID = tenantScope.TenantID, tenantScope.UserID, tenantScope.MembershipID
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var created bool
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			created, innerErr = scoped.CreateLoginChallenge(txCtx, challenge)
			return innerErr
		})
		return created, err
	}
	created := false
	err = s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx,
			`UPDATE login_challenges SET active = 0
			 WHERE learner_id = ? AND active = 1 AND expires_at <= ?`,
			challenge.LearnerID, challenge.CreatedAt.UTC(),
		); err != nil {
			return fmt.Errorf("expire prior login challenge: %w", err)
		}
		if _, err := txs.exec(ctx, `DELETE FROM credential_tenant_routes
			WHERE kind = ? AND tenant_id = ? AND learner_id = ? AND expires_at <= ?`,
			credentialKindLoginChallenge, tenantScope.TenantID, tenantScope.LearnerID,
			challenge.CreatedAt.UTC()); err != nil {
			return fmt.Errorf("expire prior login challenge routes: %w", err)
		}
		result, err := txs.exec(ctx,
			`INSERT INTO login_challenges (`+loginChallengeColumns+`, active)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
			 ON CONFLICT DO NOTHING`,
			challenge.TokenHash, challenge.UserID, challenge.TenantID,
			challenge.MembershipID, challenge.LearnerID, challenge.ClientID,
			challenge.RedirectURI, challenge.Resource, challenge.State, challenge.Scope,
			challenge.CodeChallenge, challenge.CodeChallengeMethod,
			challenge.ExpiresAt.UTC(), challenge.CreatedAt.UTC(), nil, nil,
		)
		if err != nil {
			return fmt.Errorf("insert login challenge: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("insert login challenge rows: %w", err)
		}
		created = rows == 1
		if !created {
			return nil
		}
		return txs.insertCredentialRoute(ctx, credentialKindLoginChallenge, challenge.TokenHash,
			tenantScope, challenge.ExpiresAt, challenge.CreatedAt)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *Store) GetLoginChallenge(ctx context.Context, tokenHash string, now time.Time) (*models.LoginChallenge, error) {
	tenantScope, routeErr := s.credentialScope(ctx, credentialKindLoginChallenge, tokenHash)
	if routeErr != nil {
		return nil, fmt.Errorf("get login challenge: %w", storeport.ErrInvalidLoginChallenge)
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var challenge *models.LoginChallenge
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			challenge, innerErr = scoped.GetLoginChallenge(txCtx, tokenHash, now)
			return innerErr
		})
		return challenge, err
	}
	challenge, err := scanLoginChallenge(s.queryRow(ctx,
		`SELECT `+loginChallengeColumns+` FROM login_challenges
		 WHERE token_hash = ? AND tenant_id = ? AND membership_id = ?
		   AND active = 1 AND expires_at > ? AND consumed_at IS NULL`,
		tokenHash, tenantScope.TenantID, tenantScope.MembershipID, now.UTC(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get login challenge: %w", storeport.ErrInvalidLoginChallenge)
	}
	if err != nil {
		return nil, fmt.Errorf("get login challenge: %w", err)
	}
	return challenge, nil
}

func (s *Store) ConsumeLoginChallenge(ctx context.Context, tokenHash string, now, trustedUntil time.Time) (*models.LoginChallenge, error) {
	if !trustedUntil.After(now) {
		return nil, fmt.Errorf("consume login challenge: invalid trusted-device expiration")
	}
	tenantScope, routeErr := s.credentialScope(ctx, credentialKindLoginChallenge, tokenHash)
	if routeErr != nil {
		return nil, fmt.Errorf("consume login challenge: %w", storeport.ErrInvalidLoginChallenge)
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var challenge *models.LoginChallenge
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			challenge, innerErr = scoped.ConsumeLoginChallenge(txCtx, tokenHash, now, trustedUntil)
			return innerErr
		})
		return challenge, err
	}
	challenge, err := scanLoginChallenge(s.queryRow(ctx,
		`UPDATE login_challenges
		 SET active = 0, consumed_at = ?, trusted_until = ?
		 WHERE token_hash = ? AND tenant_id = ? AND membership_id = ?
		   AND active = 1 AND expires_at > ? AND consumed_at IS NULL
		 RETURNING `+loginChallengeColumns,
		now.UTC(), trustedUntil.UTC(), tokenHash, tenantScope.TenantID,
		tenantScope.MembershipID, now.UTC(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("consume login challenge: %w", storeport.ErrInvalidLoginChallenge)
	}
	if err != nil {
		return nil, fmt.Errorf("consume login challenge: %w", err)
	}
	if _, err := s.exec(ctx, `UPDATE credential_tenant_routes SET expires_at = ?
		WHERE kind = ? AND credential_key = ?`, trustedUntil.UTC(),
		credentialKindLoginChallenge, credentialRouteKey(tokenHash)); err != nil {
		return nil, fmt.Errorf("extend login challenge route: %w", err)
	}
	return challenge, nil
}

func (s *Store) DeleteLoginChallenge(ctx context.Context, tokenHash string) error {
	tenantScope, routeErr := s.credentialScope(ctx, credentialKindLoginChallenge, tokenHash)
	if routeErr != nil {
		return fmt.Errorf("delete login challenge: %w", storeport.ErrInvalidLoginChallenge)
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		return s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped storeport.Store) error {
			return scoped.DeleteLoginChallenge(txCtx, tokenHash)
		})
	}
	if _, err := s.exec(ctx, `DELETE FROM login_challenges WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete login challenge: %w", err)
	}
	return s.deleteCredentialRoute(ctx, credentialKindLoginChallenge, tokenHash)
}

func (s *Store) IsTrustedLoginDevice(ctx context.Context, learnerID, tokenHash string, now time.Time) (bool, error) {
	tenantScope, routeErr := s.credentialScope(ctx, credentialKindLoginChallenge, tokenHash)
	if routeErr != nil || tenantScope.LearnerID != learnerID {
		return false, nil
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var trusted bool
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			trusted, innerErr = scoped.IsTrustedLoginDevice(txCtx, learnerID, tokenHash, now)
			return innerErr
		})
		return trusted, err
	}
	var trusted bool
	if err := s.queryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM login_challenges
		    WHERE token_hash = ? AND learner_id = ? AND active = 0
		      AND consumed_at IS NOT NULL AND trusted_until > ?
		)`, tokenHash, learnerID, now.UTC(),
	).Scan(&trusted); err != nil {
		return false, fmt.Errorf("check trusted login device: %w", err)
	}
	return trusted, nil
}
