// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const sqliteSupportAccessMigration = `CREATE TABLE support_access_grants (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), actor_id TEXT NOT NULL,
    token_hash TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('active','revoked','expired')),
    reason TEXT NOT NULL, request_id TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL, expires_at DATETIME NOT NULL, revoked_at DATETIME,
    PRIMARY KEY (tenant_id, id), UNIQUE (token_hash)
);
CREATE TABLE support_access_routes (
    token_hash TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, grant_id TEXT NOT NULL, expires_at DATETIME NOT NULL
);`

const postgresSupportAccessMigration = `CREATE TABLE support_access_grants (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), actor_id TEXT NOT NULL,
    token_hash TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('active','revoked','expired')),
    reason TEXT NOT NULL, request_id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL, revoked_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id), UNIQUE (token_hash)
);
CREATE TABLE support_access_routes (
    token_hash TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, grant_id TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL
);
ALTER TABLE support_access_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE support_access_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON support_access_grants
USING (tenant_id = current_setting('app.current_tenant', true))
WITH CHECK (tenant_id = current_setting('app.current_tenant', true));`

func (s *Store) BeginSupportAccess(ctx context.Context, actor models.ControlPlanePrincipal, tenantID string, duration time.Duration) (*models.SupportAccessGrant, string, error) {
	if !actor.Validate() || tenantID == "" || duration <= 0 || duration > time.Hour || actor.RequestID == "" {
		return nil, "", fmt.Errorf("begin support access: invalid authority, tenant or duration")
	}
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		return nil, "", err
	}
	raw := "support_" + base64.RawURLEncoding.EncodeToString(rawBytes)
	hash := opaqueTokenHash(raw)
	id, err := generateID()
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	grant := &models.SupportAccessGrant{ID: id, TenantID: tenantID, ActorID: actor.ActorID,
		Status: "active", CreatedAt: now, ExpiresAt: now.Add(duration)}
	err = s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		var tenantStatus string
		if err := txs.queryRow(ctx, `SELECT status FROM tenants WHERE id = ?`, tenantID).Scan(&tenantStatus); err != nil || tenantStatus != models.TenantStatusActive {
			return fmt.Errorf("begin support access: active tenant not found")
		}
		if _, err := txs.exec(ctx, `INSERT INTO support_access_grants
			(id, tenant_id, actor_id, token_hash, status, reason, request_id, version, created_at, expires_at)
			VALUES (?, ?, ?, ?, 'active', ?, ?, 1, ?, ?)`, id, tenantID, actor.ActorID,
			hash, actor.Reason, actor.RequestID, now, grant.ExpiresAt); err != nil {
			return err
		}
		if _, err := txs.exec(ctx, `INSERT INTO support_access_routes
			(token_hash, tenant_id, grant_id, expires_at) VALUES (?, ?, ?, ?)`, hash,
			tenantID, id, grant.ExpiresAt); err != nil {
			return err
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "support_access.begin", "support_access_grant", id)
	})
	return grant, raw, err
}

func (s *Store) AuthenticateSupportAccess(ctx context.Context, credential models.SupportAccessCredential) (models.Principal, error) {
	if credential.Token == "" {
		return models.Principal{}, fmt.Errorf("authenticate support access: invalid credential")
	}
	hash := opaqueTokenHash(credential.Token)
	var tenantID, grantID string
	if err := s.queryRow(ctx, `SELECT tenant_id, grant_id FROM support_access_routes
		WHERE token_hash = ? AND expires_at > ?`, hash, time.Now().UTC()).Scan(&tenantID, &grantID); err != nil {
		return models.Principal{}, fmt.Errorf("authenticate support access: invalid credential")
	}
	var principal models.Principal
	err := s.withTenantControlTx(ctx, tenantID, grantID, func(txs *Store) error {
		if err := txs.queryRow(ctx, `SELECT actor_id, version FROM support_access_grants grant_row
			JOIN tenants tenant ON tenant.id = grant_row.tenant_id AND tenant.status = 'active'
			WHERE grant_row.tenant_id = ? AND grant_row.id = ? AND grant_row.token_hash = ?
			  AND grant_row.status = 'active' AND grant_row.expires_at > ?`, tenantID,
			grantID, hash, time.Now().UTC()).Scan(&principal.UserID, &principal.TokenVersion); err != nil {
			return fmt.Errorf("authenticate support access: invalid credential")
		}
		principal.TenantID = tenantID
		principal.MembershipID = "support_grant_" + grantID
		principal.Roles = []string{models.RoleSupport}
		principal.Scopes = []string{models.OAuthScopeLearnerRead}
		return principal.Validate()
	})
	return principal, err
}

func (s *Store) validateSupportPrincipal(ctx context.Context, principal models.Principal) error {
	grantID := principal.MembershipID
	const prefix = "support_grant_"
	if len(grantID) <= len(prefix) || grantID[:len(prefix)] != prefix || principal.LearnerID != "" ||
		!slices.Equal(principal.Roles, []string{models.RoleSupport}) {
		return storeport.ErrInvalidPrincipal
	}
	grantID = grantID[len(prefix):]
	var actorID string
	err := s.queryRow(ctx, `SELECT actor_id FROM support_access_grants
		WHERE tenant_id = ? AND id = ? AND status = 'active' AND version = ? AND expires_at > ?`,
		principal.TenantID, grantID, principal.TokenVersion, time.Now().UTC()).Scan(&actorID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && actorID != principal.UserID {
		return storeport.ErrInvalidPrincipal
	}
	return err
}

func (s *Store) RevokeSupportAccess(ctx context.Context, actor models.ControlPlanePrincipal, tenantID, grantID string) error {
	if !actor.Validate() || tenantID == "" || grantID == "" || actor.RequestID == "" {
		return fmt.Errorf("revoke support access: invalid authority or input")
	}
	return s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		var tokenHash string
		err := txs.queryRow(ctx, `UPDATE support_access_grants SET status = 'revoked',
			version = version + 1, revoked_at = ? WHERE tenant_id = ? AND id = ?
			AND status = 'active' RETURNING token_hash`, time.Now().UTC(), tenantID, grantID).Scan(&tokenHash)
		if err != nil {
			return err
		}
		if _, err := txs.exec(ctx, `DELETE FROM support_access_routes WHERE token_hash = ?`, tokenHash); err != nil {
			return err
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "support_access.revoke", "support_access_grant", grantID)
	})
}
