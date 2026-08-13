// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const sqliteIdentityFederationMigration = `CREATE TABLE tenant_identity_providers (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    kind TEXT NOT NULL CHECK (kind IN ('oidc','saml','scim')),
    issuer TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active','suspended','revoked')),
    version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_by TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, kind, issuer)
);
CREATE TABLE federated_identity_links (
    tenant_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id),
    membership_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active','suspended','revoked')),
    created_at DATETIME NOT NULL,
    last_seen_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, provider_id, subject),
    UNIQUE (tenant_id, membership_id),
    FOREIGN KEY (tenant_id, provider_id) REFERENCES tenant_identity_providers(tenant_id, id),
    FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id)
);
CREATE INDEX idx_federated_identity_links_user ON federated_identity_links(user_id, tenant_id, status);`

const postgresIdentityFederationMigration = `CREATE TABLE tenant_identity_providers (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id),
    kind TEXT NOT NULL CHECK (kind IN ('oidc','saml','scim')), issuer TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active','suspended','revoked')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1), created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id), UNIQUE (tenant_id, kind, issuer)
);
CREATE TABLE federated_identity_links (
    tenant_id TEXT NOT NULL, provider_id TEXT NOT NULL, issuer TEXT NOT NULL,
    subject TEXT NOT NULL, user_id TEXT NOT NULL REFERENCES users(id), membership_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active','suspended','revoked')),
    created_at TIMESTAMPTZ NOT NULL, last_seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, provider_id, subject), UNIQUE (tenant_id, membership_id),
    FOREIGN KEY (tenant_id, provider_id) REFERENCES tenant_identity_providers(tenant_id, id),
    FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id)
);
CREATE INDEX idx_federated_identity_links_user ON federated_identity_links(user_id, tenant_id, status);
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['tenant_identity_providers','federated_identity_links'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %1$I USING (tenant_id = current_setting(''app.current_tenant'', true)) WITH CHECK (tenant_id = current_setting(''app.current_tenant'', true))', table_name);
    END LOOP;
END $$;`

func (s *Store) ConfigureIdentityProvider(ctx context.Context, actor models.Principal, kind, issuer string) (*models.TenantIdentityProvider, error) {
	if !actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, fmt.Errorf("configure identity provider: %w", storeport.ErrInvalidPrincipal)
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	issuer = strings.TrimSpace(issuer)
	if (kind != "oidc" && kind != "saml" && kind != "scim") || issuer == "" {
		return nil, fmt.Errorf("configure identity provider: invalid kind or issuer")
	}
	id := stableLearningID("identity_provider_", actor.TenantID, kind, issuer)
	now := time.Now().UTC()
	provider := &models.TenantIdentityProvider{
		ID: id, TenantID: actor.TenantID, Kind: kind, Issuer: issuer,
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.exec(txCtx, `INSERT INTO tenant_identity_providers
			(id, tenant_id, kind, issuer, status, version, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'active', 1, ?, ?, ?)
			ON CONFLICT (tenant_id, kind, issuer) DO NOTHING`, id, actor.TenantID,
			kind, issuer, actor.UserID, now, now); err != nil {
			return err
		}
		if err := txs.queryRow(txCtx, `SELECT id, status, version, created_at, updated_at
			FROM tenant_identity_providers WHERE tenant_id = ? AND kind = ? AND issuer = ?`,
			actor.TenantID, kind, issuer).Scan(&provider.ID, &provider.Status,
			&provider.Version, &provider.CreatedAt, &provider.UpdatedAt); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "identity_provider.configure", TargetType: "identity_provider", TargetID: provider.ID,
		})
	})
	return provider, err
}

func validateFederatedAssertion(assertion models.VerifiedFederatedIdentityAssertion) error {
	if assertion.TenantID == "" || assertion.ProviderID == "" || assertion.Issuer == "" || assertion.Subject == "" ||
		assertion.TenantID != strings.TrimSpace(assertion.TenantID) || assertion.Subject != strings.TrimSpace(assertion.Subject) {
		return fmt.Errorf("invalid verified federation assertion")
	}
	return nil
}

// ProvisionFederatedMembership is the common durable boundary behind SCIM and
// just-in-time OIDC/SAML provisioning. A new subject always creates a new user;
// matching email addresses are deliberately ignored.
func (s *Store) ProvisionFederatedMembership(ctx context.Context, actor models.Principal, assertion models.VerifiedFederatedIdentityAssertion, roles []string) (*models.TenantMembership, error) {
	if !actor.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		actor.TenantID != assertion.TenantID || validateFederatedAssertion(assertion) != nil {
		return nil, fmt.Errorf("provision federated membership: %w", storeport.ErrInvalidPrincipal)
	}
	rolesJSON, err := validatedRolesJSON(roles)
	if err != nil {
		return nil, fmt.Errorf("provision federated membership: %w", err)
	}
	var membership *models.TenantMembership
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var providerIssuer, providerStatus string
		if err := txs.queryRow(txCtx, `SELECT issuer, status FROM tenant_identity_providers
			WHERE tenant_id = ? AND id = ?`, assertion.TenantID, assertion.ProviderID).
			Scan(&providerIssuer, &providerStatus); err != nil || providerIssuer != assertion.Issuer || providerStatus != "active" {
			return fmt.Errorf("provision federated membership: inactive or mismatched provider")
		}
		var userID, membershipID string
		err := txs.queryRow(txCtx, `SELECT user_id, membership_id FROM federated_identity_links
			WHERE tenant_id = ? AND provider_id = ? AND subject = ?`, assertion.TenantID,
			assertion.ProviderID, assertion.Subject).Scan(&userID, &membershipID)
		switch {
		case err == nil:
			result, updateErr := txs.exec(txCtx, `UPDATE tenant_memberships
				SET roles_json = ?, status = 'active', version = version + 1, updated_at = ?
				WHERE tenant_id = ? AND id = ?`, rolesJSON, time.Now().UTC(), assertion.TenantID, membershipID)
			if updateErr != nil {
				return updateErr
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return fmt.Errorf("provision federated membership: membership missing")
			}
			if _, err := txs.exec(txCtx, `UPDATE federated_identity_links
				SET status = 'active', last_seen_at = ? WHERE tenant_id = ? AND provider_id = ? AND subject = ?`,
				time.Now().UTC(), assertion.TenantID, assertion.ProviderID, assertion.Subject); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			userID, err = generateID()
			if err != nil {
				return err
			}
			membershipID, err = generateID()
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			email := strings.TrimSpace(strings.ToLower(assertion.Email))
			if email == "" {
				email = userID + "@federated.invalid"
			}
			if _, err := txs.exec(txCtx, `INSERT INTO users
				(id, email, normalized_email, password_hash, status, email_verified_at,
				 token_version, created_at, updated_at)
				VALUES (?, ?, ?, '', 'active', ?, 1, ?, ?)`, userID, email, email, now, now, now); err != nil {
				return err
			}
			learnerID := ""
			if containsRole(roles, models.RoleLearner) {
				learnerID, err = generateID()
				if err != nil {
					return err
				}
				profileEmail := assertion.TenantID + "+" + learnerID + "@profile.invalid"
				if _, err := txs.exec(txCtx, `INSERT INTO learners
					(id, email, password_hash, objective, profile_json, created_at, email_verified_at,
					 tenant_id, user_id, membership_id)
					VALUES (?, ?, '', '', '{}', ?, ?, ?, ?, ?)`, learnerID, profileEmail, now,
					now, assertion.TenantID, userID, membershipID); err != nil {
					return err
				}
				if _, err := txs.exec(txCtx, `UPDATE tenant_memberships SET roles_json = ?, status = 'active',
					version = 1, updated_at = ? WHERE tenant_id = ? AND id = ?`, rolesJSON, now,
					assertion.TenantID, membershipID); err != nil {
					return err
				}
			} else if _, err := txs.exec(txCtx, `INSERT INTO tenant_memberships
				(id, tenant_id, user_id, roles_json, status, version, created_at, updated_at)
				VALUES (?, ?, ?, ?, 'active', 1, ?, ?)`, membershipID, assertion.TenantID,
				userID, rolesJSON, now, now); err != nil {
				return err
			}
			if _, err := txs.exec(txCtx, `INSERT INTO external_identities
				(id, user_id, provider, issuer, subject, email_at_link, created_at, last_seen_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, stableLearningID("external_identity_",
				assertion.ProviderID, assertion.Subject), userID, assertion.ProviderID,
				assertion.Issuer, assertion.Subject, assertion.Email, now, now); err != nil {
				return err
			}
			if _, err := txs.exec(txCtx, `INSERT INTO federated_identity_links
				(tenant_id, provider_id, issuer, subject, user_id, membership_id,
				 status, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
				assertion.TenantID, assertion.ProviderID, assertion.Issuer, assertion.Subject,
				userID, membershipID, now, now); err != nil {
				return err
			}
		default:
			return err
		}
		var learnerID sql.NullString
		var currentRolesJSON, status string
		var version int64
		var createdAt, updatedAt time.Time
		if err := txs.queryRow(txCtx, `SELECT learner_id, roles_json, status, version, created_at, updated_at
			FROM tenant_memberships WHERE tenant_id = ? AND id = ?`, assertion.TenantID, membershipID).
			Scan(&learnerID, &currentRolesJSON, &status, &version, &createdAt, &updatedAt); err != nil {
			return err
		}
		var currentRoles []string
		if err := json.Unmarshal([]byte(currentRolesJSON), &currentRoles); err != nil {
			return err
		}
		membership = &models.TenantMembership{ID: membershipID, TenantID: assertion.TenantID,
			UserID: userID, LearnerID: learnerID.String, Roles: currentRoles, Status: status,
			Version: version, CreatedAt: createdAt, UpdatedAt: updatedAt}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "federated_membership.provision", TargetType: "membership", TargetID: membershipID,
		})
	})
	return membership, err
}

func (s *Store) GetFederatedPrincipal(ctx context.Context, assertion models.VerifiedFederatedIdentityAssertion, scopes []string) (models.Principal, error) {
	if validateFederatedAssertion(assertion) != nil {
		return models.Principal{}, fmt.Errorf("federated principal: invalid assertion")
	}
	var principal models.Principal
	err := s.withTenantControlTx(ctx, assertion.TenantID, assertion.Subject, func(txs *Store) error {
		var rolesJSON string
		var learnerID sql.NullString
		err := txs.queryRow(ctx, `SELECT link.user_id, link.membership_id, membership.learner_id,
			membership.roles_json, membership.version
			FROM federated_identity_links link
			JOIN tenant_identity_providers provider ON provider.tenant_id = link.tenant_id
			 AND provider.id = link.provider_id AND provider.status = 'active' AND provider.issuer = link.issuer
			JOIN tenant_memberships membership ON membership.tenant_id = link.tenant_id
			 AND membership.id = link.membership_id AND membership.user_id = link.user_id
			 AND membership.status = 'active'
			JOIN tenants tenant ON tenant.id = link.tenant_id AND tenant.status = 'active'
			WHERE link.tenant_id = ? AND link.provider_id = ? AND link.issuer = ?
			 AND link.subject = ? AND link.status = 'active'`, assertion.TenantID,
			assertion.ProviderID, assertion.Issuer, assertion.Subject).Scan(&principal.UserID,
			&principal.MembershipID, &learnerID, &rolesJSON, &principal.TokenVersion)
		if err != nil {
			return fmt.Errorf("federated principal: inactive identity")
		}
		principal.TenantID, principal.LearnerID = assertion.TenantID, learnerID.String
		if err := json.Unmarshal([]byte(rolesJSON), &principal.Roles); err != nil {
			return err
		}
		principal.Scopes = append([]string(nil), scopes...)
		if err := principal.Validate(); err != nil {
			return fmt.Errorf("federated principal: %w", storeport.ErrInvalidPrincipal)
		}
		_, err = txs.exec(ctx, `UPDATE federated_identity_links SET last_seen_at = ?
			WHERE tenant_id = ? AND provider_id = ? AND subject = ?`, time.Now().UTC(),
			assertion.TenantID, assertion.ProviderID, assertion.Subject)
		return err
	})
	return principal, err
}

func (s *Store) SetIdentityProviderStatus(ctx context.Context, actor models.Principal, providerID, status string) error {
	if !actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		providerID == "" || (status != "active" && status != "suspended" && status != "revoked") {
		return fmt.Errorf("set identity provider status: invalid authorization or input")
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		result, err := txs.exec(txCtx, `UPDATE tenant_identity_providers
			SET status = ?, version = version + 1, updated_at = ? WHERE tenant_id = ? AND id = ?`,
			status, time.Now().UTC(), actor.TenantID, providerID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("set identity provider status: provider not found")
		}
		if status != "active" {
			if _, err := txs.exec(txCtx, `UPDATE federated_identity_links SET status = ?
				WHERE tenant_id = ? AND provider_id = ?`, status, actor.TenantID, providerID); err != nil {
				return err
			}
			if _, err := txs.exec(txCtx, `UPDATE tenant_memberships
				SET status = 'suspended', version = version + 1, updated_at = ?
				WHERE tenant_id = ? AND id IN (SELECT membership_id FROM federated_identity_links
				 WHERE tenant_id = ? AND provider_id = ?)`, time.Now().UTC(), actor.TenantID,
				actor.TenantID, providerID); err != nil {
				return err
			}
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "identity_provider.status." + status, TargetType: "identity_provider", TargetID: providerID,
		})
	})
}
