// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	if containsRole(roles, models.RoleOwner) && !containsRole(actor.Roles, models.RoleOwner) {
		return nil, "", fmt.Errorf("create invitation: %w", storeport.ErrInvalidPrincipal)
	}
	now := time.Now().UTC()
	invitation := &models.TenantInvitation{
		TenantID: actor.TenantID, Email: normalizedEmail,
		NormalizedEmail: normalizedEmail, Roles: append([]string(nil), roles...),
		Status: "pending", CreatedBy: actor.UserID, CreatedAt: now, ExpiresAt: expiresAt.UTC(),
	}
	var rawToken string
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		id, token, err := txs.insertInvitation(txCtx, actor.TenantID, normalizedEmail, rolesJSON, actor.UserID, expiresAt)
		if err != nil {
			return err
		}
		invitation.ID, rawToken = id, token
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
	tenantID, err := s.invitationTenant(ctx, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("accept invitation: invalid invitation")
	}
	var membership *models.TenantMembership
	err = s.withTenantControlTx(ctx, tenantID, userID, func(txs *Store) error {
		var acceptErr error
		membership, acceptErr = txs.acceptInvitationTx(ctx, tokenHash, tenantID, userID)
		return acceptErr
	})
	if err != nil {
		return nil, err
	}
	return membership, nil
}

// acceptInvitationTx binds a pending invitation to an existing verified user
// whose address matches it. It runs inside a tenant control transaction.
func (s *Store) acceptInvitationTx(ctx context.Context, tokenHash, tenantID, userID string) (*models.TenantMembership, error) {
	var invitationID, normalizedEmail, rolesJSON, createdBy string
	var expiresAt time.Time
	lock := ""
	if s.dialect == DialectPostgres {
		// Concurrent acceptances of one link queue here; the second then sees
		// the invitation accepted.
		lock = " FOR UPDATE"
	}
	if err := s.queryRow(ctx, `SELECT id, normalized_email, roles_json, created_by, expires_at
        FROM tenant_invitations
        WHERE token_hash = ? AND tenant_id = ? AND status = 'pending' AND expires_at > ?`+lock,
		tokenHash, tenantID, time.Now().UTC()).Scan(
		&invitationID, &normalizedEmail, &rolesJSON, &createdBy, &expiresAt); err != nil {
		return nil, fmt.Errorf("accept invitation: invalid invitation")
	}
	var user models.User
	var verified sql.NullTime
	if err := s.queryRow(ctx, `SELECT id, email, normalized_email, password_hash, status,
            email_verified_at, token_version, created_at, updated_at
        FROM users WHERE id = ?`, userID).Scan(
		&user.ID, &user.Email, &user.NormalizedEmail, &user.PasswordHash, &user.Status,
		&verified, &user.TokenVersion, &user.CreatedAt, &user.UpdatedAt); err != nil {
		return nil, fmt.Errorf("accept invitation: invalid user")
	}
	if user.Status != models.UserStatusActive || !verified.Valid || user.NormalizedEmail != normalizedEmail {
		return nil, fmt.Errorf("accept invitation: verified identity does not match invitation")
	}
	var existing int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM tenant_memberships
		WHERE tenant_id = ? AND user_id = ?`, tenantID, userID).Scan(&existing); err != nil {
		return nil, err
	}
	if existing > 0 {
		return nil, storeport.ErrAlreadyMember
	}
	var roles []string
	if err := json.Unmarshal([]byte(rolesJSON), &roles); err != nil {
		return nil, err
	}
	membershipID, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	mfaRequired := 0
	if models.RolesRequireMFA(roles) {
		mfaRequired = 1
	}
	if _, err := s.exec(ctx, `INSERT INTO tenant_memberships
		(id, tenant_id, user_id, learner_id, roles_json, status, version,
		 mfa_required, created_at, updated_at)
		VALUES (?, ?, ?, NULL, ?, 'active', 1, ?, ?, ?)`,
		membershipID, tenantID, userID, rolesJSON, mfaRequired, now, now); err != nil {
		return nil, fmt.Errorf("create invited membership: %w", err)
	}
	scope := models.TenantScope{TenantID: tenantID, UserID: userID, MembershipID: membershipID}
	learnerID := ""
	if containsRole(roles, models.RoleLearner) {
		if err := s.createInvitedLearnerProfile(ctx, scope, verified.Time); err != nil {
			return nil, err
		}
		if err := s.queryRow(ctx, `SELECT COALESCE(learner_id, '') FROM tenant_memberships
			WHERE tenant_id = ? AND id = ?`, tenantID, membershipID).Scan(&learnerID); err != nil {
			return nil, err
		}
	}
	accepted, err := s.exec(ctx, `UPDATE tenant_invitations
        SET status = 'accepted', accepted_at = ?, accepted_user_id = ?, accepted_membership_id = ?
        WHERE id = ? AND status = 'pending'`, now, userID, membershipID, invitationID)
	if err != nil {
		return nil, err
	}
	if count, _ := accepted.RowsAffected(); count != 1 {
		return nil, fmt.Errorf("accept invitation: invalid invitation")
	}
	if _, err := s.exec(ctx, `DELETE FROM invitation_tenant_routes WHERE token_hash = ?`, tokenHash); err != nil {
		return nil, err
	}
	membership := &models.TenantMembership{
		ID: membershipID, TenantID: tenantID, UserID: userID, LearnerID: learnerID,
		Roles: roles, Status: models.MembershipStatusActive, Version: 1,
		MFARequired: mfaRequired == 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.AppendAuditEvent(ctx, models.Principal{
		UserID: userID, TenantID: tenantID, MembershipID: membershipID,
		LearnerID: learnerID, Roles: roles, Scopes: []string{models.OAuthScopeLearner}, TokenVersion: 1,
	}, models.AuditEvent{Action: "membership.accept", TargetType: "membership", TargetID: membershipID}); err != nil {
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
