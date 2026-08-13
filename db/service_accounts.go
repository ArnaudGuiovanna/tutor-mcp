// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (s *Store) CreateServiceAccount(ctx context.Context, actor models.Principal, name string, delegatedRoles []string, scope string, expiresAt time.Time) (*models.ServiceAccount, string, error) {
	if !actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		strings.TrimSpace(name) == "" || !expiresAt.After(time.Now().UTC()) {
		return nil, "", fmt.Errorf("create service account: invalid authorization or input")
	}
	canonicalScope, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		return nil, "", fmt.Errorf("create service account: %w", err)
	}
	roles := []string{models.RoleServiceAccount}
	seen := map[string]bool{models.RoleServiceAccount: true}
	for _, role := range delegatedRoles {
		if seen[role] || !models.ValidTenantRole(role) || role == models.RoleOwner ||
			role == models.RoleAdmin || role == models.RoleBillingAdmin || role == models.RoleLearner ||
			role == models.RoleServiceAccount || role == models.RoleSupport {
			return nil, "", fmt.Errorf("create service account: unsafe delegated role %q", role)
		}
		seen[role] = true
		roles = append(roles, role)
	}
	rolesJSON, _ := json.Marshal(roles)
	scopes := strings.Fields(canonicalScope)
	scopesJSON, _ := json.Marshal(scopes)
	id, err := generateID()
	if err != nil {
		return nil, "", err
	}
	clientID := "service_client_" + id
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, "", err
	}
	rawToken := "tsa_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	tokenHash := opaqueTokenHash(rawToken)
	now := time.Now().UTC()
	account := &models.ServiceAccount{
		ID: id, TenantID: actor.TenantID, Name: strings.TrimSpace(name), ClientID: clientID,
		Roles: roles, Scopes: scopes, Status: "active", Version: 1,
		CreatedBy: actor.UserID, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expiresAt,
	}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.exec(txCtx, `INSERT INTO oauth_clients
			(client_id, client_name, redirect_uris, client_secret_hash, expires_at)
			VALUES (?, ?, '[]', '', ?)`, clientID, account.Name, expiresAt.UTC()); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `INSERT INTO service_accounts
			(id, tenant_id, name, client_id, roles_json, scopes_json, token_hash,
			 status, version, created_by, created_at, updated_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'active', 1, ?, ?, ?, ?)`, id,
			actor.TenantID, account.Name, clientID, string(rolesJSON), string(scopesJSON),
			tokenHash, actor.UserID, now, now, expiresAt.UTC()); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `INSERT INTO service_account_routes
			(token_hash, tenant_id, service_account_id, expires_at) VALUES (?, ?, ?, ?)`,
			tokenHash, actor.TenantID, id, expiresAt.UTC()); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "service_account.create", TargetType: "service_account", TargetID: id,
		})
	})
	if err != nil {
		return nil, "", err
	}
	return account, rawToken, nil
}

func (s *Store) AuthenticateServiceAccount(ctx context.Context, credential models.ServiceAccountCredential) (models.Principal, error) {
	clientID, rawToken := credential.ClientID, credential.Secret
	if clientID == "" || rawToken == "" {
		return models.Principal{}, fmt.Errorf("authenticate service account: invalid credential")
	}
	tokenHash := opaqueTokenHash(rawToken)
	var tenantID, accountID string
	if err := s.queryRow(ctx, `SELECT tenant_id, service_account_id FROM service_account_routes
		WHERE token_hash = ? AND (expires_at IS NULL OR expires_at > ?)`, tokenHash, time.Now().UTC()).
		Scan(&tenantID, &accountID); err != nil {
		return models.Principal{}, fmt.Errorf("authenticate service account: invalid credential")
	}
	var principal models.Principal
	err := s.withTenantControlTx(ctx, tenantID, accountID, func(txs *Store) error {
		var rolesJSON, scopesJSON, currentClient string
		if err := txs.queryRow(ctx, `SELECT id, client_id, roles_json, scopes_json, version
			FROM service_accounts WHERE tenant_id = ? AND id = ? AND token_hash = ?
			  AND status = 'active' AND (expires_at IS NULL OR expires_at > ?)`,
			tenantID, accountID, tokenHash, time.Now().UTC()).Scan(&principal.UserID,
			&currentClient, &rolesJSON, &scopesJSON, &principal.TokenVersion); err != nil {
			return fmt.Errorf("authenticate service account: invalid credential")
		}
		if currentClient != clientID {
			return fmt.Errorf("authenticate service account: invalid credential")
		}
		if err := json.Unmarshal([]byte(rolesJSON), &principal.Roles); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(scopesJSON), &principal.Scopes); err != nil {
			return err
		}
		principal.TenantID = tenantID
		principal.MembershipID = "service_account_" + accountID
		if err := principal.Validate(); err != nil {
			return err
		}
		_, err := txs.exec(ctx, `UPDATE service_accounts SET last_used_at = ?
			WHERE tenant_id = ? AND id = ? AND version = ?`, time.Now().UTC(), tenantID,
			accountID, principal.TokenVersion)
		return err
	})
	return principal, err
}

func (s *Store) validateServiceAccountPrincipal(ctx context.Context, principal models.Principal) error {
	if principal.MembershipID != "service_account_"+principal.UserID || principal.LearnerID != "" {
		return storeport.ErrInvalidPrincipal
	}
	var rolesJSON, scopesJSON string
	var version int64
	err := s.queryRow(ctx, `SELECT sa.roles_json, sa.scopes_json, sa.version
		FROM service_accounts sa JOIN tenants tenant ON tenant.id = sa.tenant_id AND tenant.status = 'active'
		WHERE sa.tenant_id = ? AND sa.id = ? AND sa.status = 'active' AND sa.version = ?
		  AND (sa.expires_at IS NULL OR sa.expires_at > ?)`, principal.TenantID,
		principal.UserID, principal.TokenVersion, time.Now().UTC()).Scan(&rolesJSON, &scopesJSON, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return storeport.ErrInvalidPrincipal
	}
	if err != nil {
		return err
	}
	var roles, scopes []string
	if json.Unmarshal([]byte(rolesJSON), &roles) != nil || json.Unmarshal([]byte(scopesJSON), &scopes) != nil ||
		version != principal.TokenVersion || !slices.Equal(roles, principal.Roles) || !slices.Equal(scopes, principal.Scopes) {
		return storeport.ErrInvalidPrincipal
	}
	return nil
}

func (s *Store) RevokeServiceAccount(ctx context.Context, actor models.Principal, accountID string) error {
	if !actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) || accountID == "" {
		return fmt.Errorf("revoke service account: invalid authorization or input")
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var tokenHash string
		if err := txs.queryRow(txCtx, `UPDATE service_accounts
			SET status = 'revoked', version = version + 1, updated_at = ?
			WHERE tenant_id = ? AND id = ? AND status <> 'revoked' RETURNING token_hash`,
			time.Now().UTC(), actor.TenantID, accountID).Scan(&tokenHash); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `DELETE FROM service_account_routes WHERE token_hash = ?`, tokenHash); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "service_account.revoke", TargetType: "service_account", TargetID: accountID,
		})
	})
}
