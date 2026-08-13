// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"go.opentelemetry.io/otel/trace"
)

func (s *Store) CreateTenantInvitation(ctx context.Context, actor models.Principal, email string, roles []string, expiresAt time.Time) (*models.TenantInvitation, string, error) {
	if !actor.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, "", fmt.Errorf("create invitation: %w", storeport.ErrInvalidPrincipal)
	}
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	if normalizedEmail == "" || !expiresAt.After(time.Now().UTC()) {
		return nil, "", fmt.Errorf("create invitation: email and future expiration are required")
	}
	rolesJSON, err := validatedRolesJSON(roles)
	if err != nil {
		return nil, "", fmt.Errorf("create invitation: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	rawToken := base64.RawURLEncoding.EncodeToString(raw)
	tokenHash := opaqueTokenHash(rawToken)
	id, err := generateID()
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	invitation := &models.TenantInvitation{
		ID: id, TenantID: actor.TenantID, Email: normalizedEmail,
		NormalizedEmail: normalizedEmail, Roles: append([]string(nil), roles...),
		Status: "pending", CreatedBy: actor.UserID, CreatedAt: now, ExpiresAt: expiresAt.UTC(),
	}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.exec(txCtx, `INSERT INTO tenant_invitations
            (id, token_hash, tenant_id, email, normalized_email, roles_json, status,
             created_by, created_at, expires_at)
            VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
			id, tokenHash, actor.TenantID, normalizedEmail, normalizedEmail, rolesJSON,
			actor.UserID, now, expiresAt.UTC()); err != nil {
			return fmt.Errorf("persist invitation: %w", err)
		}
		if _, err := txs.exec(txCtx, `INSERT INTO invitation_tenant_routes (token_hash, tenant_id, expires_at)
            VALUES (?, ?, ?)`, tokenHash, actor.TenantID, expiresAt.UTC()); err != nil {
			return fmt.Errorf("persist invitation route: %w", err)
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "membership.invite", TargetType: "invitation", TargetID: id,
			DetailsJSON: `{"roles":` + rolesJSON + `}`,
		})
	})
	if err != nil {
		return nil, "", err
	}
	return invitation, rawToken, nil
}

func (s *Store) AcceptTenantInvitation(ctx context.Context, rawToken, userID string) (*models.TenantMembership, error) {
	tokenHash := opaqueTokenHash(rawToken)
	var tenantID string
	err := s.queryRow(ctx, `SELECT tenant_id FROM invitation_tenant_routes
        WHERE token_hash = ? AND expires_at > ?`, tokenHash, time.Now().UTC()).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("accept invitation: invalid invitation")
	}
	if err != nil {
		return nil, fmt.Errorf("accept invitation route: %w", err)
	}
	var membership *models.TenantMembership
	err = s.withTenantControlTx(ctx, tenantID, userID, func(txs *Store) error {
		var invitationID, normalizedEmail, rolesJSON, createdBy string
		var expiresAt time.Time
		if err := txs.queryRow(ctx, `SELECT id, normalized_email, roles_json, created_by, expires_at
            FROM tenant_invitations
            WHERE token_hash = ? AND tenant_id = ? AND status = 'pending' AND expires_at > ?`,
			tokenHash, tenantID, time.Now().UTC()).Scan(
			&invitationID, &normalizedEmail, &rolesJSON, &createdBy, &expiresAt); err != nil {
			return fmt.Errorf("accept invitation: invalid invitation")
		}
		var user models.User
		var verified sql.NullTime
		if err := txs.queryRow(ctx, `SELECT id, email, normalized_email, password_hash, status,
                email_verified_at, token_version, created_at, updated_at
            FROM users WHERE id = ?`, userID).Scan(
			&user.ID, &user.Email, &user.NormalizedEmail, &user.PasswordHash, &user.Status,
			&verified, &user.TokenVersion, &user.CreatedAt, &user.UpdatedAt); err != nil {
			return fmt.Errorf("accept invitation: invalid user")
		}
		if user.Status != models.UserStatusActive || !verified.Valid || user.NormalizedEmail != normalizedEmail {
			return fmt.Errorf("accept invitation: verified identity does not match invitation")
		}
		var roles []string
		if err := json.Unmarshal([]byte(rolesJSON), &roles); err != nil {
			return err
		}
		membershipID, err := generateID()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		learnerIDString := ""
		if containsRole(roles, models.RoleLearner) {
			learnerIDString, err = generateID()
			if err != nil {
				return err
			}
		}
		mfaRequired := 0
		if containsRole(roles, models.RoleOwner) || containsRole(roles, models.RoleAdmin) {
			mfaRequired = 1
		}
		if learnerIDString != "" {
			syntheticEmail := tenantID + "+" + learnerIDString + "@profile.invalid"
			if _, err := txs.exec(ctx, `INSERT INTO learners
                (id, email, password_hash, objective, profile_json, created_at, email_verified_at,
                 tenant_id, user_id, membership_id)
                VALUES (?, ?, '', '', '{}', ?, ?, ?, ?, ?)`,
				learnerIDString, syntheticEmail, now, verified.Time, tenantID, userID, membershipID); err != nil {
				return fmt.Errorf("create invited learner profile: %w", err)
			}
			// The rolling-deployment learner trigger materializes the membership
			// first to satisfy its learner FK. Replace its compatibility defaults
			// with the exact invitation authorization in the same transaction.
			if _, err := txs.exec(ctx, `UPDATE tenant_memberships
				SET roles_json = ?, status = 'active', version = 1,
				    mfa_required = ?, updated_at = ?
				WHERE tenant_id = ? AND id = ? AND user_id = ? AND learner_id = ?`,
				rolesJSON, mfaRequired, now, tenantID, membershipID, userID, learnerIDString); err != nil {
				return fmt.Errorf("authorize invited membership: %w", err)
			}
		} else if _, err := txs.exec(ctx, `INSERT INTO tenant_memberships
			(id, tenant_id, user_id, learner_id, roles_json, status, version,
			 mfa_required, created_at, updated_at)
			VALUES (?, ?, ?, NULL, ?, 'active', 1, ?, ?, ?)`,
			membershipID, tenantID, userID, rolesJSON, mfaRequired, now, now); err != nil {
			return fmt.Errorf("create invited membership: %w", err)
		}
		if _, err := txs.exec(ctx, `UPDATE tenant_invitations
            SET status = 'accepted', accepted_at = ?, accepted_user_id = ?, accepted_membership_id = ?
            WHERE id = ? AND status = 'pending'`, now, userID, membershipID, invitationID); err != nil {
			return err
		}
		if _, err := txs.exec(ctx, `DELETE FROM invitation_tenant_routes WHERE token_hash = ?`, tokenHash); err != nil {
			return err
		}
		membership = &models.TenantMembership{
			ID: membershipID, TenantID: tenantID, UserID: userID, LearnerID: learnerIDString,
			Roles: roles, Status: models.MembershipStatusActive, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		return txs.AppendAuditEvent(ctx, models.Principal{
			UserID: userID, TenantID: tenantID, MembershipID: membershipID,
			LearnerID: learnerIDString, Roles: roles, Scopes: []string{models.OAuthScopeLearner}, TokenVersion: 1,
		}, models.AuditEvent{Action: "membership.accept", TargetType: "membership", TargetID: membershipID})
	})
	if err != nil {
		return nil, err
	}
	return membership, nil
}

func (s *Store) LinkExternalIdentity(ctx context.Context, userID string, identity models.ExternalIdentityInput) (*models.ExternalIdentity, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(identity.Provider) == "" ||
		strings.TrimSpace(identity.Issuer) == "" || strings.TrimSpace(identity.Subject) == "" {
		return nil, fmt.Errorf("link external identity: user, provider, issuer and subject are required")
	}
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	result := &models.ExternalIdentity{
		ID: id, UserID: userID, Provider: identity.Provider, Issuer: identity.Issuer,
		Subject: identity.Subject, EmailAtLink: identity.EmailAtLink, CreatedAt: now, LastSeenAt: now,
	}
	_, err = s.exec(ctx, `INSERT INTO external_identities
        (id, user_id, provider, issuer, subject, email_at_link, created_at, last_seen_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, userID, identity.Provider,
		identity.Issuer, identity.Subject, identity.EmailAtLink, now, now)
	if err != nil {
		return nil, fmt.Errorf("link external identity: %w", err)
	}
	return result, nil
}

func (s *Store) AppendAuditEvent(ctx context.Context, actor models.Principal, event models.AuditEvent) error {
	if actor.Validate() != nil || actor.TenantID == "" {
		return fmt.Errorf("append audit event: %w", storeport.ErrInvalidPrincipal)
	}
	if strings.TrimSpace(event.Action) == "" || strings.TrimSpace(event.TargetType) == "" || strings.TrimSpace(event.TargetID) == "" {
		return fmt.Errorf("append audit event: action and target are required")
	}
	if event.DetailsJSON == "" {
		event.DetailsJSON = "{}"
	}
	if !json.Valid([]byte(event.DetailsJSON)) {
		return fmt.Errorf("append audit event: invalid details JSON")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.Result == "" {
		event.Result = "success"
	}
	if event.Result != "success" && event.Result != "denied" && event.Result != "failed" {
		return fmt.Errorf("append audit event: invalid result")
	}
	if event.TraceID == "" {
		if span := trace.SpanContextFromContext(ctx); span.IsValid() {
			event.TraceID = span.TraceID().String()
		}
	}
	_, err := s.exec(ctx, `INSERT INTO audit_events
        (tenant_id, actor_user_id, membership_id, action, target_type, target_id,
         request_id, reason, details_json, result, trace_id, occurred_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, actor.TenantID, actor.UserID,
		actor.MembershipID, event.Action, event.TargetType, event.TargetID,
		event.RequestID, event.Reason, event.DetailsJSON, event.Result, event.TraceID, event.OccurredAt)
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

func (s *Store) withTenantControlTx(ctx context.Context, tenantID, userID string, fn func(*Store) error) error {
	if tenantID == "" || userID == "" {
		return fmt.Errorf("tenant control transaction: tenant and user are required")
	}
	if s.root == nil {
		return fn(s)
	}
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == DialectPostgres {
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', $1, true)`, tenantID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_user', $1, true)`, userID); err != nil {
			return err
		}
	}
	if err := fn(&Store{db: tx, dialect: s.dialect, secretKeyring: s.secretKeyring}); err != nil {
		return err
	}
	return tx.Commit()
}

func validatedRolesJSON(roles []string) (string, error) {
	if len(roles) == 0 {
		return "", fmt.Errorf("at least one role is required")
	}
	seen := map[string]bool{}
	for _, role := range roles {
		if !models.ValidTenantRole(role) || seen[role] || role == models.RoleServiceAccount {
			return "", fmt.Errorf("invalid invitation role %q", role)
		}
		seen[role] = true
	}
	raw, _ := json.Marshal(roles)
	return string(raw), nil
}

func containsRole(roles []string, wanted string) bool {
	for _, role := range roles {
		if role == wanted {
			return true
		}
	}
	return false
}

func opaqueTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}
