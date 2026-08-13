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
	"tutor-mcp/store"
)

const (
	accountTokenEmailVerification = "email_verification"
	accountTokenPasswordReset     = "password_reset"
)

type accountTokenScanner interface {
	Scan(dest ...any) error
}

func scanAccountToken(row accountTokenScanner) (*models.AccountToken, error) {
	token := &models.AccountToken{}
	var consumedAt sql.NullTime
	if err := row.Scan(
		&token.TokenHash,
		&token.UserID,
		&token.TenantID,
		&token.MembershipID,
		&token.LearnerID,
		&token.Purpose,
		&token.ClientID,
		&token.RedirectURI,
		&token.Resource,
		&token.State,
		&token.Scope,
		&token.CodeChallenge,
		&token.CodeChallengeMethod,
		&token.ExpiresAt,
		&token.CreatedAt,
		&consumedAt,
	); err != nil {
		return nil, err
	}
	if consumedAt.Valid {
		stamp := consumedAt.Time
		token.ConsumedAt = &stamp
	}
	// During a rolling upgrade, the immediately preceding binary can still
	// write an omitted authorization scope into the already-existing account
	// token column. Omission meant the bounded legacy learner grant, so retain
	// that exact historical meaning while all newly issued tokens are required
	// to persist a canonical non-empty scope.
	if token.Purpose == accountTokenEmailVerification && strings.TrimSpace(token.Scope) == "" {
		token.Scope = models.OAuthScopeLearner
	}
	return token, nil
}

const accountTokenColumns = `token_hash, user_id, tenant_id, membership_id, learner_id, purpose, client_id, redirect_uri, resource,
state, scope, code_challenge, code_challenge_method, expires_at, created_at, consumed_at`

// CreateAccountToken persists only a digest of the bearer capability. Issuing
// a new token invalidates older capabilities of the same purpose for the same
// learner, which bounds both replay surface and mailbox-link confusion.
func (s *Store) CreateAccountToken(ctx context.Context, token *models.AccountToken) error {
	if token == nil || strings.TrimSpace(token.TokenHash) == "" || strings.TrimSpace(token.LearnerID) == "" {
		return fmt.Errorf("create account token: token hash and learner are required")
	}
	if token.Purpose != accountTokenEmailVerification && token.Purpose != accountTokenPasswordReset {
		return fmt.Errorf("create account token: unsupported purpose")
	}
	if token.ExpiresAt.IsZero() || !token.ExpiresAt.After(token.CreatedAt) {
		return fmt.Errorf("create account token: invalid expiration")
	}
	if token.Purpose == accountTokenEmailVerification &&
		(strings.TrimSpace(token.ClientID) == "" || strings.TrimSpace(token.RedirectURI) == "" ||
			strings.TrimSpace(token.Resource) == "") {
		return fmt.Errorf("create account token: OAuth continuation is incomplete")
	}
	if token.Purpose == accountTokenEmailVerification {
		canonicalScope, err := models.CanonicalOAuthScope(token.Scope)
		if err != nil || canonicalScope != token.Scope {
			return fmt.Errorf("create account token: %w", store.ErrInvalidOAuthScope)
		}
	}
	if token.CodeChallenge == "" && token.CodeChallengeMethod != "" {
		return fmt.Errorf("create account token: PKCE method requires a challenge")
	}
	credentialKind := accountCredentialKind(token.Purpose)
	tenantScope := models.TenantScope{
		TenantID: token.TenantID, UserID: token.UserID,
		MembershipID: token.MembershipID, LearnerID: token.LearnerID,
	}
	if tenantScope.Validate() != nil {
		var err error
		tenantScope, err = s.credentialScopeForLearner(ctx, token.LearnerID)
		if err != nil {
			return fmt.Errorf("create account token: %w", err)
		}
		token.TenantID, token.UserID, token.MembershipID = tenantScope.TenantID, tenantScope.UserID, tenantScope.MembershipID
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		return s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped store.Store) error {
			return scoped.CreateAccountToken(txCtx, token)
		})
	}

	return s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx,
			`DELETE FROM account_tokens WHERE learner_id = ? AND purpose = ?`,
			token.LearnerID, token.Purpose,
		); err != nil {
			return fmt.Errorf("replace account token: %w", err)
		}
		if _, err := txs.exec(ctx, `DELETE FROM credential_tenant_routes
			WHERE kind = ? AND tenant_id = ? AND learner_id = ?`,
			credentialKind, token.TenantID, token.LearnerID); err != nil {
			return fmt.Errorf("replace account token route: %w", err)
		}
		if _, err := txs.exec(ctx,
			`INSERT INTO account_tokens (`+accountTokenColumns+`)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			token.TokenHash, token.UserID, token.TenantID, token.MembershipID,
			token.LearnerID, token.Purpose, token.ClientID,
			token.RedirectURI, token.Resource, token.State, token.Scope,
			token.CodeChallenge, token.CodeChallengeMethod, token.ExpiresAt,
			token.CreatedAt, token.ConsumedAt,
		); err != nil {
			return fmt.Errorf("create account token: %w", err)
		}
		return txs.insertCredentialRoute(ctx, credentialKind, token.TokenHash,
			tenantScope, token.ExpiresAt, token.CreatedAt)
	})
}

func (s *Store) GetAccountToken(ctx context.Context, tokenHash, purpose string) (*models.AccountToken, error) {
	tenantScope, routeErr := s.credentialScope(ctx, accountCredentialKind(purpose), tokenHash)
	if routeErr != nil {
		var expiresAt, createdAt time.Time
		fallbackErr := s.queryRow(ctx, `SELECT l.tenant_id, l.user_id, l.membership_id, l.id,
		        a.expires_at, a.created_at
		    FROM account_tokens a JOIN learners l ON l.id = a.learner_id
		    WHERE a.token_hash = ? AND a.purpose = ?`, tokenHash, purpose).Scan(
			&tenantScope.TenantID, &tenantScope.UserID, &tenantScope.MembershipID,
			&tenantScope.LearnerID, &expiresAt, &createdAt)
		if fallbackErr != nil || tenantScope.Validate() != nil {
			return nil, fmt.Errorf("get account token: %w", store.ErrInvalidAccountToken)
		}
		if _, err := s.exec(ctx, `UPDATE account_tokens
		    SET tenant_id = ?, user_id = ?, membership_id = ?
		    WHERE token_hash = ? AND purpose = ?
		      AND (user_id = '' OR membership_id = '')`,
			tenantScope.TenantID, tenantScope.UserID, tenantScope.MembershipID,
			tokenHash, purpose); err != nil {
			return nil, fmt.Errorf("get account token: %w", store.ErrInvalidAccountToken)
		}
		if err := s.insertCredentialRoute(ctx, accountCredentialKind(purpose), tokenHash,
			tenantScope, expiresAt, createdAt); err != nil {
			return nil, fmt.Errorf("get account token: %w", store.ErrInvalidAccountToken)
		}
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var token *models.AccountToken
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped store.Store) error {
			var innerErr error
			token, innerErr = scoped.GetAccountToken(txCtx, tokenHash, purpose)
			return innerErr
		})
		return token, err
	}
	token, err := scanAccountToken(s.queryRow(ctx,
		`SELECT `+accountTokenColumns+` FROM account_tokens
		 WHERE token_hash = ? AND tenant_id = ? AND membership_id = ?
		   AND purpose = ? AND expires_at > ? AND consumed_at IS NULL`,
		tokenHash, tenantScope.TenantID, tenantScope.MembershipID, purpose, time.Now().UTC(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get account token: %w", store.ErrInvalidAccountToken)
	}
	if err != nil {
		return nil, fmt.Errorf("get account token: %w", err)
	}
	return token, nil
}

// ActivateLearnerAndCreateAuthCode consumes mailbox proof, replaces the
// unusable pending credential with one chosen by the mailbox holder, records
// that holder's explicit client approval, and resumes the exact OAuth request
// in one transaction. Clearing stale codes and approvals also repairs pending
// identities created by older builds that trusted the unauthenticated
// registration initiator.
func (s *Store) ActivateLearnerAndCreateAuthCode(ctx context.Context, tokenHash, passwordHash, code string, codeExpiresAt time.Time, persistClientApproval bool) (*models.AccountToken, error) {
	if strings.TrimSpace(passwordHash) == "" || strings.TrimSpace(code) == "" || codeExpiresAt.IsZero() {
		return nil, fmt.Errorf("activate learner: password hash, code, and expiration are required")
	}
	tenantScope, routeErr := s.credentialScope(ctx, credentialKindEmailVerification, tokenHash)
	if routeErr != nil {
		return nil, fmt.Errorf("activate learner: %w", store.ErrInvalidAccountToken)
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var token *models.AccountToken
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped store.Store) error {
			var innerErr error
			token, innerErr = scoped.ActivateLearnerAndCreateAuthCode(txCtx, tokenHash, passwordHash, code, codeExpiresAt, persistClientApproval)
			return innerErr
		})
		return token, err
	}
	var token *models.AccountToken
	now := time.Now().UTC()
	err := s.inTx(ctx, nil, func(txs *Store) error {
		var err error
		token, err = scanAccountToken(txs.queryRow(ctx,
			`UPDATE account_tokens SET consumed_at = ?
			 WHERE token_hash = ? AND purpose = ? AND expires_at > ? AND consumed_at IS NULL
			 RETURNING `+accountTokenColumns,
			now, tokenHash, accountTokenEmailVerification, now,
		))
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrInvalidAccountToken
		}
		if err != nil {
			return fmt.Errorf("consume email verification: %w", err)
		}
		if strings.TrimSpace(token.ClientID) == "" || strings.TrimSpace(token.RedirectURI) == "" ||
			strings.TrimSpace(token.Resource) == "" {
			return store.ErrInvalidAccountToken
		}
		canonicalScope, scopeErr := models.CanonicalOAuthScope(token.Scope)
		if scopeErr != nil || canonicalScope != token.Scope {
			return store.ErrInvalidAccountToken
		}
		result, err := txs.exec(ctx,
			`UPDATE learners SET password_hash = ?, email_verified_at = ?
			 WHERE id = ? AND email_verified_at IS NULL`,
			passwordHash, now, token.LearnerID,
		)
		if err != nil {
			return fmt.Errorf("activate learner: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("activate learner rows: %w", err)
		}
		if updated != 1 {
			return store.ErrInvalidAccountToken
		}
		// An inactive identity must not carry grants or codes chosen before its
		// mailbox owner established the first usable credential.
		if _, err := txs.exec(ctx, `DELETE FROM oauth_codes WHERE learner_id = ?`, token.LearnerID); err != nil {
			return fmt.Errorf("clear pending authorization codes: %w", err)
		}
		if _, err := txs.exec(ctx, `DELETE FROM credential_tenant_routes
			WHERE kind = ? AND tenant_id = ? AND learner_id = ?`,
			credentialKindAuthorizationCode, token.TenantID, token.LearnerID); err != nil {
			return fmt.Errorf("clear pending authorization code routes: %w", err)
		}
		if _, err := txs.exec(ctx, `DELETE FROM learner_approved_clients WHERE learner_id = ?`, token.LearnerID); err != nil {
			return fmt.Errorf("clear pending client approvals: %w", err)
		}
		if persistClientApproval {
			if err := txs.ApproveClientForScope(ctx, token.LearnerID, token.ClientID, token.RedirectURI, token.Scope); err != nil {
				return fmt.Errorf("approve verified client: %w", err)
			}
		}
		if err := txs.CreateAuthCodeWithBindingAndScope(
			ctx, code, token.LearnerID, token.CodeChallenge, token.CodeChallengeMethod,
			token.ClientID, token.RedirectURI, token.Resource, token.Scope, codeExpiresAt,
		); err != nil {
			return err
		}
		if _, err := txs.exec(ctx,
			`UPDATE account_tokens SET consumed_at = COALESCE(consumed_at, ?)
			 WHERE learner_id = ? AND purpose = ?`,
			now, token.LearnerID, accountTokenEmailVerification,
		); err != nil {
			return fmt.Errorf("invalidate email verification tokens: %w", err)
		}
		return txs.deleteCredentialRoute(ctx, credentialKindEmailVerification, tokenHash)
	})
	if errors.Is(err, store.ErrInvalidAccountToken) {
		return nil, fmt.Errorf("activate learner: %w", store.ErrInvalidAccountToken)
	}
	if err != nil {
		return nil, fmt.Errorf("activate learner: %w", err)
	}
	return token, nil
}

// ResetPasswordWithToken consumes a reset link, updates the credential and
// revokes every durable OAuth credential for the learner atomically.
func (s *Store) ResetPasswordWithToken(ctx context.Context, tokenHash, passwordHash string) (string, error) {
	if strings.TrimSpace(passwordHash) == "" {
		return "", fmt.Errorf("reset password: password hash is required")
	}
	tenantScope, routeErr := s.credentialScope(ctx, credentialKindPasswordReset, tokenHash)
	if routeErr != nil {
		return "", fmt.Errorf("reset password: %w", store.ErrInvalidAccountToken)
	}
	if !s.inTenantTransaction(ctx, tenantScope) && s.root != nil {
		var learnerID string
		err := s.WithTenantTx(ctx, tenantScope, func(txCtx context.Context, scoped store.Store) error {
			var innerErr error
			learnerID, innerErr = scoped.ResetPasswordWithToken(txCtx, tokenHash, passwordHash)
			return innerErr
		})
		return learnerID, err
	}
	var learnerID string
	now := time.Now().UTC()
	err := s.inTx(ctx, nil, func(txs *Store) error {
		err := txs.queryRow(ctx,
			`UPDATE account_tokens SET consumed_at = ?
			 WHERE token_hash = ? AND purpose = ? AND expires_at > ? AND consumed_at IS NULL
			 RETURNING learner_id`,
			now, tokenHash, accountTokenPasswordReset, now,
		).Scan(&learnerID)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrInvalidAccountToken
		}
		if err != nil {
			return fmt.Errorf("consume password reset: %w", err)
		}
		if _, err := txs.exec(ctx,
			`UPDATE learners
			 SET password_hash = ?, email_verified_at = COALESCE(email_verified_at, ?)
			 WHERE id = ?`,
			passwordHash, now, learnerID,
		); err != nil {
			return fmt.Errorf("update password: %w", err)
		}
		if _, err := txs.exec(ctx,
			`UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE learner_id = ?`,
			now, learnerID,
		); err != nil {
			return fmt.Errorf("revoke refresh tokens: %w", err)
		}
		if _, err := txs.exec(ctx, `DELETE FROM oauth_codes WHERE learner_id = ?`, learnerID); err != nil {
			return fmt.Errorf("delete authorization codes: %w", err)
		}
		if _, err := txs.exec(ctx, `DELETE FROM credential_tenant_routes
			WHERE kind = ? AND tenant_id = ? AND learner_id = ?`,
			credentialKindAuthorizationCode, tenantScope.TenantID, learnerID); err != nil {
			return fmt.Errorf("delete authorization code routes: %w", err)
		}
		if _, err := txs.exec(ctx,
			`UPDATE account_tokens SET consumed_at = COALESCE(consumed_at, ?) WHERE learner_id = ?`,
			now, learnerID,
		); err != nil {
			return fmt.Errorf("invalidate account tokens: %w", err)
		}
		return txs.deleteCredentialRoute(ctx, credentialKindPasswordReset, tokenHash)
	})
	if errors.Is(err, store.ErrInvalidAccountToken) {
		return "", fmt.Errorf("reset password: %w", store.ErrInvalidAccountToken)
	}
	if err != nil {
		return "", fmt.Errorf("reset password: %w", err)
	}
	return learnerID, nil
}

func (s *Store) CleanupExpiredAccountTokens(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	var deleted int64
	err := s.inTx(ctx, nil, func(txs *Store) error {
		result, err := txs.exec(ctx,
			`DELETE FROM account_tokens
			 WHERE expires_at <= ? OR (consumed_at IS NOT NULL AND consumed_at <= ?)`,
			now, now.Add(-24*time.Hour),
		)
		if err != nil {
			return err
		}
		deleted, err = result.RowsAffected()
		if err != nil {
			return err
		}
		result, err = txs.exec(ctx,
			`DELETE FROM login_challenges
			 WHERE (active = 1 AND expires_at <= ?)
			    OR (active = 0 AND COALESCE(trusted_until, consumed_at, expires_at) <= ?)`,
			now, now,
		)
		if err != nil {
			return err
		}
		challengeDeleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted += challengeDeleted
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("cleanup account tokens: %w", err)
	}
	return deleted, nil
}
