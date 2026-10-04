// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
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

// consoleScopes is the canonical scope carried by console principals. The
// console never issues OAuth tokens; Principal.Validate requires a scope.
var consoleScopes = []string{models.OAuthScopeLearner}

// ---------------------------------------------------------------------------
// MFA verification per membership
// ---------------------------------------------------------------------------

// EnsureMembershipMFAVerified records the first MFA verification of a
// membership and returns its current version. Later verifications leave the
// version unchanged so a console sign-in never revokes AI-client tokens.
func (s *Store) EnsureMembershipMFAVerified(ctx context.Context, scope models.TenantScope, at time.Time) (int64, error) {
	if err := scope.Validate(); err != nil || at.IsZero() {
		return 0, fmt.Errorf("ensure MFA verification: invalid scope or timestamp")
	}
	var version int64
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		err := txs.queryRow(txCtx, `UPDATE tenant_memberships
			SET mfa_verified_at = ?, version = version + 1, updated_at = ?
			WHERE tenant_id = ? AND id = ? AND user_id = ? AND status = 'active'
			  AND mfa_verified_at IS NULL
			RETURNING version`, at.UTC(), at.UTC(), scope.TenantID, scope.MembershipID,
			scope.UserID).Scan(&version)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return txs.queryRow(txCtx, `SELECT version FROM tenant_memberships
			WHERE tenant_id = ? AND id = ? AND user_id = ? AND status = 'active'`,
			scope.TenantID, scope.MembershipID, scope.UserID).Scan(&version)
	})
	if err != nil {
		return 0, fmt.Errorf("ensure MFA verification: %w", err)
	}
	return version, nil
}

// ---------------------------------------------------------------------------
// Console sessions
// ---------------------------------------------------------------------------

func (s *Store) CreateConsoleSession(ctx context.Context, credential models.ConsoleSessionCredential, scope models.TenantScope, membershipVersion int64, mfaVerifiedAt *time.Time, expiresAt time.Time) error {
	now := time.Now().UTC()
	if credential.Token == "" || scope.Validate() != nil || membershipVersion < 1 || !expiresAt.After(now) {
		return fmt.Errorf("create console session: invalid session")
	}
	return s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx, `DELETE FROM console_sessions WHERE expires_at <= ?`, now); err != nil {
			return err
		}
		_, err := txs.exec(ctx, `INSERT INTO console_sessions
			(token_hash, user_id, tenant_id, membership_id, membership_version,
			 mfa_verified_at, created_at, last_seen_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, opaqueTokenHash(credential.Token), scope.UserID,
			scope.TenantID, scope.MembershipID, membershipVersion,
			nullableTime(mfaVerifiedAt), now, now, expiresAt.UTC())
		if err != nil {
			return fmt.Errorf("create console session: %w", err)
		}
		return nil
	})
}

func (s *Store) GetConsoleSession(ctx context.Context, credential models.ConsoleSessionCredential) (*models.ConsoleSession, error) {
	tokenHash := opaqueTokenHash(credential.Token)
	var session models.ConsoleSession
	var mfaAt sql.NullTime
	err := s.queryRow(ctx, `SELECT token_hash, user_id, tenant_id, membership_id, membership_version,
			mfa_verified_at, created_at, last_seen_at, expires_at
		FROM console_sessions WHERE token_hash = ?`, tokenHash).Scan(
		&session.TokenHash, &session.UserID, &session.TenantID, &session.MembershipID,
		&session.MembershipVersion, &mfaAt, &session.CreatedAt, &session.LastSeenAt, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storeport.WrapNotFound(err)
	}
	if err != nil {
		return nil, fmt.Errorf("get console session: %w", err)
	}
	if mfaAt.Valid {
		stamp := mfaAt.Time
		session.MFAVerifiedAt = &stamp
	}
	return &session, nil
}

// UpdateConsoleSession refreshes the idle clock and, when given, the bound
// membership version and MFA verification time.
func (s *Store) UpdateConsoleSession(ctx context.Context, credential models.ConsoleSessionCredential, seenAt time.Time, membershipVersion int64, mfaVerifiedAt *time.Time) error {
	tokenHash := opaqueTokenHash(credential.Token)
	result, err := s.exec(ctx, `UPDATE console_sessions
		SET last_seen_at = ?,
		    membership_version = CASE WHEN ? > 0 THEN ? ELSE membership_version END,
		    mfa_verified_at = COALESCE(?, mfa_verified_at)
		WHERE token_hash = ?`, seenAt.UTC(), membershipVersion, membershipVersion,
		nullableTime(mfaVerifiedAt), tokenHash)
	if err != nil {
		return fmt.Errorf("update console session: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return storeport.WrapNotFound(sql.ErrNoRows)
	}
	return nil
}

func (s *Store) DeleteConsoleSession(ctx context.Context, credential models.ConsoleSessionCredential) error {
	if _, err := s.exec(ctx, `DELETE FROM console_sessions WHERE token_hash = ?`, opaqueTokenHash(credential.Token)); err != nil {
		return fmt.Errorf("delete console session: %w", err)
	}
	return nil
}

// GetConsoleMembership loads the live authorization state of an active
// membership in an active tenant for an active user.
func (s *Store) GetConsoleMembership(ctx context.Context, scope models.TenantScope) (*models.ConsoleMembership, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("get console membership: %w", storeport.ErrInvalidPrincipal)
	}
	var membership models.ConsoleMembership
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var rolesJSON string
		var mfaRequired int
		var mfaVerified sql.NullTime
		var enrolled int
		err := txs.queryRow(txCtx, `SELECT t.name, u.email, COALESCE(tm.learner_id, ''), tm.roles_json,
				tm.version, tm.mfa_required, tm.mfa_verified_at,
				(SELECT COUNT(*) FROM mfa_credentials c
				  WHERE c.user_id = u.id AND c.kind = 'totp'
				    AND c.confirmed_at IS NOT NULL AND c.revoked_at IS NULL)
			FROM tenant_memberships tm
			JOIN tenants t ON t.id = tm.tenant_id AND t.status = 'active'
			JOIN users u ON u.id = tm.user_id AND u.status = 'active'
			WHERE tm.tenant_id = ? AND tm.id = ? AND tm.user_id = ? AND tm.status = 'active'`,
			scope.TenantID, scope.MembershipID, scope.UserID).Scan(
			&membership.TenantName, &membership.Email, &membership.LearnerID, &rolesJSON,
			&membership.Version, &mfaRequired, &mfaVerified, &enrolled)
		if err != nil {
			return err
		}
		membership.MFARequired = mfaRequired == 1
		membership.MFAVerified = mfaVerified.Valid
		membership.MFAEnrolled = enrolled > 0
		return json.Unmarshal([]byte(rolesJSON), &membership.Roles)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get console membership: %w", storeport.ErrInvalidPrincipal)
	}
	if err != nil {
		return nil, fmt.Errorf("get console membership: %w", err)
	}
	return &membership, nil
}

// ConsolePrincipal builds the principal of a console membership.
func ConsolePrincipal(scope models.TenantScope, membership models.ConsoleMembership) models.Principal {
	return models.Principal{
		UserID: scope.UserID, TenantID: scope.TenantID, MembershipID: scope.MembershipID,
		LearnerID: membership.LearnerID, Roles: append([]string(nil), membership.Roles...),
		Scopes: append([]string(nil), consoleScopes...), TokenVersion: membership.Version,
	}
}

// ---------------------------------------------------------------------------
// Member administration
// ---------------------------------------------------------------------------

var errMembershipManageDenied = fmt.Errorf("%w: membership management denied", storeport.ErrInvalidPrincipal)

func (s *Store) ListTenantMembers(ctx context.Context, actor models.Principal) ([]models.TenantMember, error) {
	if !actor.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, errMembershipManageDenied
	}
	var members []models.TenantMember
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		rows, err := txs.query(txCtx, `SELECT tm.id, tm.user_id, u.email, tm.roles_json, tm.status, tm.created_at,
				(SELECT COUNT(*) FROM mfa_credentials c
				  WHERE c.user_id = u.id AND c.kind = 'totp'
				    AND c.confirmed_at IS NOT NULL AND c.revoked_at IS NULL)
			FROM tenant_memberships tm
			JOIN users u ON u.id = tm.user_id
			WHERE tm.tenant_id = ? AND tm.status <> 'revoked'
			ORDER BY u.normalized_email, tm.id`, actor.TenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var member models.TenantMember
			var rolesJSON string
			var enrolled int
			if err := rows.Scan(&member.MembershipID, &member.UserID, &member.Email, &rolesJSON,
				&member.Status, &member.CreatedAt, &enrolled); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(rolesJSON), &member.Roles); err != nil {
				return err
			}
			member.MFAEnrolled = enrolled > 0
			members = append(members, member)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list tenant members: %w", err)
	}
	return members, nil
}

func (s *Store) ListTenantInvitations(ctx context.Context, actor models.Principal) ([]models.TenantInvitation, error) {
	if !actor.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, errMembershipManageDenied
	}
	var invitations []models.TenantInvitation
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		rows, err := txs.query(txCtx, `SELECT id, email, roles_json, status, created_by, created_at, expires_at
			FROM tenant_invitations
			WHERE tenant_id = ? AND status = 'pending' AND expires_at > ?
			ORDER BY created_at DESC, id`, actor.TenantID, time.Now().UTC())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			invitation := models.TenantInvitation{TenantID: actor.TenantID}
			var rolesJSON string
			if err := rows.Scan(&invitation.ID, &invitation.Email, &rolesJSON, &invitation.Status,
				&invitation.CreatedBy, &invitation.CreatedAt, &invitation.ExpiresAt); err != nil {
				return err
			}
			invitation.NormalizedEmail = invitation.Email
			if err := json.Unmarshal([]byte(rolesJSON), &invitation.Roles); err != nil {
				return err
			}
			invitations = append(invitations, invitation)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list tenant invitations: %w", err)
	}
	return invitations, nil
}

func (s *Store) RevokeTenantInvitation(ctx context.Context, actor models.Principal, invitationID string) error {
	if !actor.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return errMembershipManageDenied
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var tokenHash, rolesJSON string
		err := txs.queryRow(txCtx, `SELECT token_hash, roles_json FROM tenant_invitations
			WHERE tenant_id = ? AND id = ? AND status = 'pending'`, actor.TenantID, invitationID).Scan(&tokenHash, &rolesJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return storeport.WrapNotFound(err)
		}
		if err != nil {
			return err
		}
		var roles []string
		if err := json.Unmarshal([]byte(rolesJSON), &roles); err != nil {
			return err
		}
		if slices.Contains(roles, models.RoleOwner) && !slices.Contains(actor.Roles, models.RoleOwner) {
			return errMembershipManageDenied
		}
		if _, err := txs.exec(txCtx, `UPDATE tenant_invitations SET status = 'revoked'
			WHERE tenant_id = ? AND id = ? AND status = 'pending'`, actor.TenantID, invitationID); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `DELETE FROM invitation_tenant_routes WHERE token_hash = ?`, tokenHash); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "membership.invitation_revoke", TargetType: "invitation", TargetID: invitationID,
		})
	})
}

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
		if status != models.MembershipStatusActive {
			if _, err := txs.exec(txCtx, `DELETE FROM console_sessions
				WHERE tenant_id = ? AND membership_id = ?`, actor.TenantID, membershipID); err != nil {
				return err
			}
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

// ---------------------------------------------------------------------------
// Invitations
// ---------------------------------------------------------------------------

func (s *Store) invitationTenant(ctx context.Context, tokenHash string) (string, error) {
	var tenantID string
	err := s.queryRow(ctx, `SELECT tenant_id FROM invitation_tenant_routes
		WHERE token_hash = ? AND expires_at > ?`, tokenHash, time.Now().UTC()).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("invitation: %w", storeport.ErrInvalidAccountToken)
	}
	if err != nil {
		return "", fmt.Errorf("invitation route: %w", err)
	}
	return tenantID, nil
}

func (s *Store) PreviewTenantInvitation(ctx context.Context, credential models.InvitationCredential) (*models.InvitationPreview, error) {
	tokenHash := opaqueTokenHash(credential.Token)
	tenantID, err := s.invitationTenant(ctx, tokenHash)
	if err != nil {
		return nil, err
	}
	var preview models.InvitationPreview
	err = s.withTenantControlTx(ctx, tenantID, "invitation", func(txs *Store) error {
		var rolesJSON string
		if err := txs.queryRow(ctx, `SELECT t.name, i.normalized_email, i.roles_json, i.expires_at
			FROM tenant_invitations i JOIN tenants t ON t.id = i.tenant_id AND t.status = 'active'
			WHERE i.token_hash = ? AND i.tenant_id = ? AND i.status = 'pending' AND i.expires_at > ?`,
			tokenHash, tenantID, time.Now().UTC()).Scan(&preview.TenantName, &preview.Email,
			&rolesJSON, &preview.ExpiresAt); err != nil {
			return fmt.Errorf("invitation: %w", storeport.ErrInvalidAccountToken)
		}
		if err := json.Unmarshal([]byte(rolesJSON), &preview.Roles); err != nil {
			return err
		}
		_, lookupErr := txs.GetLocalUserByEmail(ctx, preview.Email)
		switch {
		case lookupErr == nil:
			preview.ExistingUser = true
		case errors.Is(lookupErr, storeport.ErrNotFound):
		default:
			return lookupErr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &preview, nil
}

// AcceptTenantInvitationWithNewUser creates the invitee's account and its
// membership atomically. Holding the invitation link stands for mailbox
// verification: the link was issued for exactly this address.
func (s *Store) AcceptTenantInvitationWithNewUser(ctx context.Context, credential models.InvitationCredential, passwordHash string) (*models.TenantMembership, error) {
	if strings.TrimSpace(passwordHash) == "" {
		return nil, fmt.Errorf("accept invitation: password is required")
	}
	tokenHash := opaqueTokenHash(credential.Token)
	tenantID, err := s.invitationTenant(ctx, tokenHash)
	if err != nil {
		return nil, err
	}
	userID, err := generateID()
	if err != nil {
		return nil, err
	}
	var membership *models.TenantMembership
	err = s.withTenantControlTx(ctx, tenantID, userID, func(txs *Store) error {
		var email string
		if err := txs.queryRow(ctx, `SELECT normalized_email FROM tenant_invitations
			WHERE token_hash = ? AND tenant_id = ? AND status = 'pending' AND expires_at > ?`,
			tokenHash, tenantID, time.Now().UTC()).Scan(&email); err != nil {
			return fmt.Errorf("invitation: %w", storeport.ErrInvalidAccountToken)
		}
		if err := txs.insertLocalUser(ctx, userID, email, passwordHash, time.Now().UTC()); err != nil {
			return err
		}
		var acceptErr error
		membership, acceptErr = txs.acceptInvitationTx(ctx, tokenHash, tenantID, userID)
		return acceptErr
	})
	if err != nil {
		return nil, err
	}
	return membership, nil
}

// insertLocalUser creates an active, verified email identity, refusing a
// second account for an address that already has one.
func (s *Store) insertLocalUser(ctx context.Context, userID, email, passwordHash string, now time.Time) error {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if s.dialect == DialectPostgres {
		// Serialize account creation per address: there is no unique index on
		// users.normalized_email, and two concurrent flows must not both insert.
		if _, err := s.exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(?))`, "users.email:"+normalized); err != nil {
			return err
		}
	}
	var existing int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM users
		WHERE normalized_email = ? AND identity_mode = 'email'`, normalized).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return storeport.ErrAccountExists
	}
	if _, err := s.exec(ctx, `INSERT INTO users
		(id, email, normalized_email, password_hash, status, email_verified_at, token_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, 1, ?, ?)`, userID, normalized, normalized, passwordHash,
		now, now, now); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// insertInvitation stores a pending invitation and its tenant route.
func (s *Store) insertInvitation(ctx context.Context, tenantID, email, rolesJSON, createdBy string, expiresAt time.Time) (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	rawToken := base64.RawURLEncoding.EncodeToString(raw)
	tokenHash := opaqueTokenHash(rawToken)
	id, err := generateID()
	if err != nil {
		return "", "", err
	}
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	if _, err := s.exec(ctx, `INSERT INTO tenant_invitations
		(id, token_hash, tenant_id, email, normalized_email, roles_json, status,
		 created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
		id, tokenHash, tenantID, normalizedEmail, normalizedEmail, rolesJSON,
		createdBy, time.Now().UTC(), expiresAt.UTC()); err != nil {
		return "", "", fmt.Errorf("persist invitation: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO invitation_tenant_routes (token_hash, tenant_id, expires_at)
		VALUES (?, ?, ?)`, tokenHash, tenantID, expiresAt.UTC()); err != nil {
		return "", "", fmt.Errorf("persist invitation route: %w", err)
	}
	return id, rawToken, nil
}

// CreateOwnerInvitation lets the platform operator bootstrap the first owner
// of a provisioned tenant.
func (s *Store) CreateOwnerInvitation(ctx context.Context, actor models.ControlPlanePrincipal, tenantID, email string, expiresAt time.Time) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if !actor.Validate() || tenantID == "" || normalized == "" || !strings.Contains(normalized, "@") ||
		!expiresAt.After(time.Now().UTC()) {
		return "", fmt.Errorf("create owner invitation: invalid authority or input")
	}
	var rawToken string
	err := s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		var active int
		if err := txs.queryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE id = ? AND status = 'active'`, tenantID).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return fmt.Errorf("create owner invitation: tenant not found")
		}
		id, token, err := txs.insertInvitation(ctx, tenantID, normalized, `["owner"]`, actor.ActorID, expiresAt)
		if err != nil {
			return err
		}
		rawToken = token
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "membership.invite_owner", "invitation", id)
	})
	if err != nil {
		return "", err
	}
	return rawToken, nil
}

// ---------------------------------------------------------------------------
// Self-service institution signup
// ---------------------------------------------------------------------------

// SignupRegion is the region recorded for self-service institutions.
const SignupRegion = "default"

func (s *Store) TenantSlugAvailable(ctx context.Context, slug string) (bool, error) {
	if !tenantSlugPattern.MatchString(slug) {
		return false, nil
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE slug = ? OR id = ?`,
		slug, stableLearningID("tenant_", slug)).Scan(&count); err != nil {
		return false, fmt.Errorf("tenant slug availability: %w", err)
	}
	return count == 0, nil
}

func (s *Store) PlanIsActive(ctx context.Context, planID string) (bool, error) {
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM plans WHERE id = ? AND status = 'active'`, planID).Scan(&count); err != nil {
		return false, fmt.Errorf("plan lookup: %w", err)
	}
	return count == 1, nil
}

func (s *Store) CreatePendingSignup(ctx context.Context, credential models.SignupCredential, email, tenantName, slug string, expiresAt time.Time) error {
	tokenHash := opaqueTokenHash(credential.Token)
	normalized := strings.ToLower(strings.TrimSpace(email))
	tenantName = strings.TrimSpace(tenantName)
	if credential.Token == "" || normalized == "" || tenantName == "" || !tenantSlugPattern.MatchString(slug) {
		return fmt.Errorf("create pending signup: invalid input")
	}
	now := time.Now().UTC()
	return s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx, `DELETE FROM pending_signups WHERE expires_at <= ?`, now); err != nil {
			return err
		}
		_, err := txs.exec(ctx, `INSERT INTO pending_signups
			(token_hash, email, normalized_email, tenant_name, tenant_slug, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, tokenHash, normalized, normalized, tenantName, slug,
			now, expiresAt.UTC())
		if err != nil {
			return fmt.Errorf("create pending signup: %w", err)
		}
		return nil
	})
}

func (s *Store) GetPendingSignup(ctx context.Context, credential models.SignupCredential) (*models.PendingSignup, error) {
	tokenHash := opaqueTokenHash(credential.Token)
	var signup models.PendingSignup
	err := s.queryRow(ctx, `SELECT normalized_email, tenant_name, tenant_slug, expires_at
		FROM pending_signups
		WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ?`,
		tokenHash, time.Now().UTC()).Scan(&signup.Email, &signup.TenantName, &signup.TenantSlug, &signup.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("pending signup: %w", storeport.ErrInvalidAccountToken)
	}
	if err != nil {
		return nil, fmt.Errorf("pending signup: %w", err)
	}
	return &signup, nil
}

// CompleteSignup consumes a verified signup and creates, in one transaction,
// the tenant with its plan, the owner's account when none exists, and the
// owner membership. existingUserID is set when the mailbox holder already has
// an account and proved it with its password; otherwise newPasswordHash
// creates one.
func (s *Store) CompleteSignup(ctx context.Context, credential models.SignupCredential, planID, existingUserID, newPasswordHash string) (*models.TenantMembership, error) {
	if (existingUserID == "") == (newPasswordHash == "") || planID == "" {
		return nil, fmt.Errorf("complete signup: invalid input")
	}
	tokenHash := opaqueTokenHash(credential.Token)
	signup, err := s.GetPendingSignup(ctx, credential)
	if err != nil {
		return nil, err
	}
	tenantID := stableLearningID("tenant_", signup.TenantSlug)
	userID := existingUserID
	if userID == "" {
		if userID, err = generateID(); err != nil {
			return nil, err
		}
	}
	membershipID, err := generateID()
	if err != nil {
		return nil, err
	}
	actor := models.ControlPlanePrincipal{
		ActorID: "self-signup:" + userID, Roles: []string{models.RolePlatformAdmin},
		Reason: "self-service institution signup", RequestID: membershipID,
	}
	now := time.Now().UTC()
	var membership *models.TenantMembership
	err = s.withTenantControlTx(ctx, tenantID, userID, func(txs *Store) error {
		consumed, err := txs.exec(ctx, `UPDATE pending_signups SET consumed_at = ?
			WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ?`, now, tokenHash, now)
		if err != nil {
			return err
		}
		if count, _ := consumed.RowsAffected(); count != 1 {
			return fmt.Errorf("pending signup: %w", storeport.ErrInvalidAccountToken)
		}
		created, err := txs.exec(ctx, `INSERT INTO tenants
			(id, slug, name, status, region, policy_json, created_at, updated_at)
			VALUES (?, ?, ?, 'active', ?, '{}', ?, ?)
			ON CONFLICT DO NOTHING`, tenantID, signup.TenantSlug, signup.TenantName, SignupRegion, now, now)
		if err != nil {
			return err
		}
		if count, _ := created.RowsAffected(); count != 1 {
			return storeport.ErrTenantSlugTaken
		}
		if err := txs.attachTenantPlan(ctx, tenantID, planID, now); err != nil {
			return err
		}
		if existingUserID != "" {
			var email string
			if err := txs.queryRow(ctx, `SELECT normalized_email FROM users
				WHERE id = ? AND status = 'active' AND email_verified_at IS NOT NULL`, existingUserID).Scan(&email); err != nil ||
				email != signup.Email {
				return fmt.Errorf("complete signup: account does not match the verified email")
			}
		} else if err := txs.insertLocalUser(ctx, userID, signup.Email, newPasswordHash, now); err != nil {
			return err
		}
		if _, err := txs.exec(ctx, `INSERT INTO tenant_memberships
			(id, tenant_id, user_id, learner_id, roles_json, status, version,
			 mfa_required, created_at, updated_at)
			VALUES (?, ?, ?, NULL, '["owner"]', 'active', 1, 1, ?, ?)`,
			membershipID, tenantID, userID, now, now); err != nil {
			return fmt.Errorf("create owner membership: %w", err)
		}
		membership = &models.TenantMembership{
			ID: membershipID, TenantID: tenantID, TenantName: signup.TenantName, UserID: userID,
			Roles: []string{models.RoleOwner}, Status: models.MembershipStatusActive, Version: 1,
			MFARequired: true, CreatedAt: now, UpdatedAt: now,
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "tenant.signup", "tenant", tenantID)
	})
	if err != nil {
		return nil, err
	}
	return membership, nil
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

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

// ---------------------------------------------------------------------------
// Password reset for global email identities
// ---------------------------------------------------------------------------

const passwordResetTTL = 15 * time.Minute

// CreateUserPasswordReset stores a reset link for the active email identity
// at email. It returns false, without error, when no such identity exists so
// callers can answer identically either way.
func (s *Store) CreateUserPasswordReset(ctx context.Context, credential models.PasswordResetCredential, email string) (bool, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if credential.Token == "" || normalized == "" {
		return false, fmt.Errorf("create password reset: invalid input")
	}
	now := time.Now().UTC()
	created := false
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx, `DELETE FROM user_password_resets WHERE expires_at <= ?`, now); err != nil {
			return err
		}
		result, err := txs.exec(ctx, `INSERT INTO user_password_resets (token_hash, user_id, created_at, expires_at)
			SELECT ?, id, ?, ? FROM users
			WHERE normalized_email = ? AND identity_mode = 'email' AND status = 'active'
			  AND email_verified_at IS NOT NULL`, opaqueTokenHash(credential.Token), now,
			now.Add(passwordResetTTL), normalized)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		created = count == 1
		if count > 1 {
			return storeport.ErrAmbiguousIdentity
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("create password reset: %w", err)
	}
	return created, nil
}

func (s *Store) UserPasswordResetValid(ctx context.Context, credential models.PasswordResetCredential) bool {
	var count int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM user_password_resets
		WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ?`,
		opaqueTokenHash(credential.Token), time.Now().UTC()).Scan(&count)
	return err == nil && count == 1
}

// ResetUserPassword consumes a reset link, replaces the password and revokes
// every existing sign-in: membership versions are bumped in each tenant and
// console sessions are deleted.
func (s *Store) ResetUserPassword(ctx context.Context, credential models.PasswordResetCredential, passwordHash string) error {
	if credential.Token == "" || strings.TrimSpace(passwordHash) == "" {
		return fmt.Errorf("reset password: %w", storeport.ErrInvalidAccountToken)
	}
	now := time.Now().UTC()
	return s.inTx(ctx, nil, func(txs *Store) error {
		var userID string
		err := txs.queryRow(ctx, `UPDATE user_password_resets SET consumed_at = ?
			WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ?
			RETURNING user_id`, now, opaqueTokenHash(credential.Token), now).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reset password: %w", storeport.ErrInvalidAccountToken)
		}
		if err != nil {
			return err
		}
		if _, err := txs.exec(ctx, `UPDATE users SET password_hash = ?, token_version = token_version + 1, updated_at = ?
			WHERE id = ? AND status = 'active'`, passwordHash, now, userID); err != nil {
			return err
		}
		if txs.dialect == DialectPostgres {
			if _, err := txs.exec(ctx, `SELECT set_config('app.identity_user', ?, true)`, userID); err != nil {
				return err
			}
		}
		rows, err := txs.query(ctx, `SELECT DISTINCT tenant_id FROM tenant_memberships WHERE user_id = ?`, userID)
		if err != nil {
			return err
		}
		var tenants []string
		for rows.Next() {
			var tenantID string
			if err := rows.Scan(&tenantID); err != nil {
				rows.Close()
				return err
			}
			tenants = append(tenants, tenantID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, tenantID := range tenants {
			if txs.dialect == DialectPostgres {
				if _, err := txs.exec(ctx, `SELECT set_config('app.current_tenant', ?, true)`, tenantID); err != nil {
					return err
				}
			}
			if _, err := txs.exec(ctx, `UPDATE tenant_memberships SET version = version + 1, updated_at = ?
				WHERE tenant_id = ? AND user_id = ? AND status IN ('invited','active')`, now, tenantID, userID); err != nil {
				return err
			}
		}
		_, err = txs.exec(ctx, `DELETE FROM console_sessions WHERE user_id = ?`, userID)
		return err
	})
}
