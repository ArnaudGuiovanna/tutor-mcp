// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var _ storeport.NativeIdentityStore = (*Store)(nil)

const nativeChallengeTTL = 2 * time.Minute
const nativeSessionTTL = 30 * time.Minute

func validNativePublicKey(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	_, err = schnorr.ParsePubKey(raw)
	return err == nil
}

func validNativeAction(action string) bool {
	return action == "session" || action == "rotate" || action == "recover" || action == "logout"
}
func validNativeDigest(value string) bool {
	raw, err := hex.DecodeString(value)
	return len(value) == 64 && value == strings.ToLower(value) && err == nil && len(raw) == 32
}

// Bind only trusted values obtained from verified proof/state. In particular,
// a rotation may fence every membership of the one authenticated global user.
func (s *Store) withNativeTx(ctx context.Context, tenantID, userID, challengeHash string, fn func(*Store) error) error {
	if s.root == nil {
		return fn(s)
	}
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.dialect == DialectPostgres {
		for _, binding := range [][2]string{{"app.current_tenant", tenantID}, {"app.native_user", userID}, {"app.identity_user", userID}, {"app.native_challenge", challengeHash}} {
			if _, err = tx.ExecContext(ctx, `SELECT set_config($1,$2,true)`, binding[0], binding[1]); err != nil {
				return err
			}
		}
	}
	scoped := &Store{db: tx, dialect: s.dialect, secretKeyring: s.secretKeyring}
	if err = fn(scoped); err != nil {
		return err
	}
	return tx.Commit()
}

const nativeIdentitySelect = `SELECT n.id,n.user_id,n.public_key,n.recovery_public_key,n.display_name,n.status,n.version,n.created_at,n.updated_at FROM native_identities n JOIN users u ON u.id=n.user_id AND u.status='active' AND u.identity_mode='pubkey' WHERE n.id=? AND n.status='active'`

func (s *Store) nativeIdentityByID(ctx context.Context, id string, lock bool) (*models.NativeIdentity, error) {
	query := nativeIdentitySelect
	if lock && s.dialect == DialectPostgres {
		query += ` FOR UPDATE OF n`
	}
	var n models.NativeIdentity
	err := s.queryRow(ctx, query, id).Scan(&n.ID, &n.UserID, &n.PublicKey, &n.RecoveryPublicKey, &n.DisplayName, &n.Status, &n.Version, &n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storeport.ErrInvalidNativeIdentity
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *Store) GetNativeIdentity(ctx context.Context, tenantID, publicKey, action string) (*models.NativeIdentity, error) {
	if strings.TrimSpace(tenantID) == "" || !validNativePublicKey(publicKey) || !validNativeAction(action) {
		return nil, storeport.ErrInvalidNativeIdentity
	}
	var result *models.NativeIdentity
	err := s.withNativeTx(ctx, tenantID, "", "", func(tx *Store) error {
		column := "n.public_key"
		if action == "recover" {
			column = "n.recovery_public_key"
		}
		var n models.NativeIdentity
		err := tx.queryRow(ctx, `SELECT n.id,n.user_id,tm.tenant_id,tm.id,COALESCE(tm.learner_id,''),n.public_key,n.recovery_public_key,n.display_name,n.status,n.version,n.created_at,n.updated_at
   FROM native_identities n JOIN users u ON u.id=n.user_id AND u.status='active' AND u.identity_mode='pubkey'
   JOIN tenant_memberships tm ON tm.user_id=u.id AND tm.tenant_id=? AND tm.status='active'
   JOIN tenants t ON t.id=tm.tenant_id AND t.status='active'
   WHERE `+column+`=? AND n.status='active' AND (tm.mfa_required=0 OR tm.mfa_verified_at IS NOT NULL)`, tenantID, publicKey).Scan(&n.ID, &n.UserID, &n.TenantID, &n.MembershipID, &n.LearnerID, &n.PublicKey, &n.RecoveryPublicKey, &n.DisplayName, &n.Status, &n.Version, &n.CreatedAt, &n.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return storeport.ErrInvalidNativeIdentity
		}
		if err != nil {
			return err
		}
		result = &n
		return nil
	})
	return result, err
}

func (s *Store) CreateNativeChallenge(ctx context.Context, c *models.NativeChallenge) error {
	if c == nil || c.IdentityID == "" || c.TenantID == "" || !validNativePublicKey(c.PublicKey) || !validNativeDigest(c.TokenHash) || !validNativeDigest(c.PayloadHash) || !validNativeAction(c.Action) || c.Method != "POST" || c.URL == "" || len(c.URL) > 2048 || c.ConsumedAt != nil || c.KeyVersion < 1 || c.CreatedAt.IsZero() || !c.ExpiresAt.After(c.CreatedAt) || c.ExpiresAt.Sub(c.CreatedAt) > nativeChallengeTTL {
		return storeport.ErrInvalidNativeChallenge
	}
	scopes, err := json.Marshal(c.Scopes)
	if err != nil {
		return err
	}
	return s.withNativeTx(ctx, c.TenantID, "", "", func(tx *Store) error {
		n, err := tx.nativeIdentityByID(ctx, c.IdentityID, true)
		if err != nil {
			return err
		}
		signing := n.PublicKey
		if c.Action == "recover" {
			signing = n.RecoveryPublicKey
		}
		if n.Version != c.KeyVersion || signing != c.PublicKey {
			return storeport.ErrInvalidNativeChallenge
		}
		member, err := tx.GetNativeIdentity(ctx, c.TenantID, c.PublicKey, c.Action)
		if err != nil {
			return err
		}
		if _, err = tx.exec(ctx, `DELETE FROM native_challenges WHERE tenant_id=? AND identity_id=? AND (expires_at<=? OR consumed_at IS NOT NULL)`, c.TenantID, c.IdentityID, c.CreatedAt); err != nil {
			return err
		}
		var count int
		if err = tx.queryRow(ctx, `SELECT COUNT(*) FROM native_challenges WHERE tenant_id=? AND identity_id=?`, c.TenantID, c.IdentityID).Scan(&count); err != nil {
			return err
		}
		if count >= 16 {
			return storeport.ErrInvalidNativeChallenge
		}
		_, err = tx.exec(ctx, `INSERT INTO native_challenges(token_hash,tenant_id,identity_id,user_id,learner_id,public_key,action,scopes_json,key_version,url,method,payload_hash,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.TokenHash, c.TenantID, c.IdentityID, n.UserID, nullString(member.LearnerID), c.PublicKey, c.Action, string(scopes), c.KeyVersion, c.URL, c.Method, c.PayloadHash, c.CreatedAt.UTC(), c.ExpiresAt.UTC())
		return err
	})
}

func (s *Store) ConsumeNativeChallenge(ctx context.Context, tokenHash string, now time.Time) (*models.NativeChallenge, error) {
	if !validNativeDigest(tokenHash) || now.IsZero() {
		return nil, storeport.ErrInvalidNativeChallenge
	}
	var c models.NativeChallenge
	var scopes string
	var consumed sql.NullTime
	err := s.withNativeTx(ctx, "", "", tokenHash, func(tx *Store) error {
		return tx.queryRow(ctx, `UPDATE native_challenges SET consumed_at=? WHERE token_hash=? AND consumed_at IS NULL AND created_at<=? AND expires_at>? RETURNING token_hash,identity_id,tenant_id,public_key,action,scopes_json,key_version,url,method,payload_hash,created_at,expires_at,consumed_at`, now.UTC(), tokenHash, now.UTC(), now.UTC()).Scan(&c.TokenHash, &c.IdentityID, &c.TenantID, &c.PublicKey, &c.Action, &scopes, &c.KeyVersion, &c.URL, &c.Method, &c.PayloadHash, &c.CreatedAt, &c.ExpiresAt, &consumed)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storeport.ErrInvalidNativeChallenge
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(scopes), &c.Scopes); err != nil {
		return nil, err
	}
	if consumed.Valid {
		c.ConsumedAt = &consumed.Time
	}
	return &c, nil
}

func nativeScopesAllowed(p models.Principal) bool {
	canonical, err := models.CanonicalOAuthScope(strings.Join(p.Scopes, " "))
	if err != nil {
		return false
	}
	for _, scope := range strings.Fields(canonical) {
		switch scope {
		case models.OAuthScopeLearner, models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite:
			if p.LearnerID == "" || !slices.Contains(p.Roles, models.RoleLearner) {
				return false
			}
		case models.OAuthScopeFormationContribute:
			if p.Validate() != nil {
				return false
			}
		case models.OAuthScopeFormationRead, models.OAuthScopeFormationWrite:
			if !p.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: p.TenantID}) {
				return false
			}
		case models.OAuthScopeProgressRead:
			if !p.CanReadInstitutionProgress() {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *Store) CreateNativeSession(ctx context.Context, session *models.NativeSession) (models.Principal, error) {
	if session == nil || session.ID == "" || len(session.ID) > 128 || session.IdentityID == "" || session.CreatedAt.IsZero() || session.RevokedAt != nil || !session.ExpiresAt.After(session.CreatedAt) || session.ExpiresAt.Sub(session.CreatedAt) > nativeSessionTTL {
		return models.Principal{}, storeport.ErrInvalidNativeSession
	}
	var result models.Principal
	err := s.withNativeTx(ctx, session.TenantID, "", "", func(tx *Store) error {
		n, err := tx.nativeIdentityByID(ctx, session.IdentityID, true)
		if err != nil {
			return err
		}
		if n.UserID != session.UserID || n.Version != session.KeyVersion {
			return storeport.ErrInvalidNativeSession
		}
		if tx.dialect == DialectPostgres {
			var membership string
			if err := tx.queryRow(ctx, `SELECT id FROM tenant_memberships WHERE tenant_id=? AND id=? AND user_id=? FOR SHARE`, session.TenantID, session.MembershipID, session.UserID).Scan(&membership); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return storeport.ErrInvalidNativeSession
				}
				return err
			}
		}
		p, err := tx.GetPrincipal(ctx, models.TenantScope{TenantID: session.TenantID, UserID: session.UserID, MembershipID: session.MembershipID, LearnerID: session.LearnerID}, session.Scopes)
		if err != nil {
			return err
		}
		if !nativeScopesAllowed(p) {
			return storeport.ErrInvalidOAuthScope
		}
		if _, err = tx.exec(ctx, `DELETE FROM native_sessions WHERE tenant_id=? AND identity_id=? AND (expires_at<=? OR revoked_at IS NOT NULL)`, session.TenantID, session.IdentityID, session.CreatedAt); err != nil {
			return err
		}
		var count int
		if err = tx.queryRow(ctx, `SELECT COUNT(*) FROM native_sessions WHERE tenant_id=? AND identity_id=?`, session.TenantID, session.IdentityID).Scan(&count); err != nil {
			return err
		}
		if count >= 32 {
			return storeport.ErrInvalidNativeSession
		}
		scopes, err := json.Marshal(session.Scopes)
		if err != nil {
			return err
		}
		_, err = tx.exec(ctx, `INSERT INTO native_sessions(id,identity_id,tenant_id,user_id,membership_id,learner_id,key_version,scopes_json,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, session.ID, session.IdentityID, session.TenantID, session.UserID, session.MembershipID, nullString(session.LearnerID), session.KeyVersion, string(scopes), session.CreatedAt.UTC(), session.ExpiresAt.UTC())
		if err == nil {
			result = p
		}
		return err
	})
	return result, err
}

func (s *Store) ValidateNativeSession(ctx context.Context, id string, p models.Principal, now time.Time) error {
	if id == "" || p.Validate() != nil || now.IsZero() {
		return storeport.ErrInvalidNativeSession
	}
	return s.withNativeTx(ctx, p.TenantID, "", "", func(tx *Store) error {
		var scopes string
		err := tx.queryRow(ctx, `SELECT s.scopes_json FROM native_sessions s JOIN native_identities n ON n.id=s.identity_id AND n.user_id=s.user_id AND n.status='active' AND n.version=s.key_version WHERE s.id=? AND s.tenant_id=? AND s.user_id=? AND s.membership_id=? AND COALESCE(s.learner_id,'')=? AND s.revoked_at IS NULL AND s.created_at<=? AND s.expires_at>?`, id, p.TenantID, p.UserID, p.MembershipID, p.LearnerID, now.UTC(), now.UTC()).Scan(&scopes)
		if errors.Is(err, sql.ErrNoRows) {
			return storeport.ErrInvalidNativeSession
		}
		if err != nil {
			return err
		}
		var allowed []string
		if err = json.Unmarshal([]byte(scopes), &allowed); err != nil {
			return err
		}
		if !slices.Equal(allowed, p.Scopes) {
			return storeport.ErrInvalidNativeSession
		}
		return tx.ValidatePrincipal(ctx, p)
	})
}

func (s *Store) RevokeNativeSession(ctx context.Context, identityID, sessionID string, now time.Time) error {
	if identityID == "" || sessionID == "" || now.IsZero() {
		return storeport.ErrInvalidNativeSession
	}
	n, err := s.nativeIdentityByID(ctx, identityID, false)
	if err != nil {
		return err
	}
	return s.withNativeTx(ctx, "", n.UserID, "", func(tx *Store) error {
		result, err := tx.exec(ctx, `UPDATE native_sessions SET revoked_at=COALESCE(revoked_at,?) WHERE id=? AND identity_id=?`, now.UTC(), sessionID, identityID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count != 1 {
			return storeport.ErrInvalidNativeSession
		}
		return err
	})
}

func (s *Store) RotateNativeIdentity(ctx context.Context, identityID string, expectedVersion int64, newPub, newRecovery string, now time.Time) (*models.NativeIdentity, error) {
	if !validNativePublicKey(newPub) || !validNativePublicKey(newRecovery) || newPub == newRecovery || expectedVersion < 1 || now.IsZero() {
		return nil, storeport.ErrInvalidNativeIdentity
	}
	current, err := s.nativeIdentityByID(ctx, identityID, false)
	if err != nil {
		return nil, err
	}
	var result *models.NativeIdentity
	err = s.withNativeTx(ctx, "", current.UserID, "", func(tx *Store) error {
		n, err := tx.nativeIdentityByID(ctx, identityID, true)
		if err != nil {
			return err
		}
		if n.Version != expectedVersion {
			return storeport.ErrInvalidNativeIdentity
		}
		if n.PublicKey == newPub || n.RecoveryPublicKey == newRecovery {
			return storeport.ErrNativeKeyConflict
		}
		for _, key := range []string{newPub, newRecovery} {
			var count int
			if err = tx.queryRow(ctx, `SELECT COUNT(*) FROM native_identity_keys WHERE public_key=?`, key).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return storeport.ErrNativeKeyConflict
			}
		}
		for _, key := range [][2]string{{newPub, "identity"}, {newRecovery, "recovery"}} {
			if _, err = tx.exec(ctx, `INSERT INTO native_identity_keys(public_key,identity_id,purpose,created_at) VALUES(?,?,?,?)`, key[0], identityID, key[1], now.UTC()); err != nil {
				return nativeKeyError(err)
			}
		}
		if _, err = tx.exec(ctx, `UPDATE native_identity_keys SET retired_at=? WHERE identity_id=? AND retired_at IS NULL AND public_key NOT IN (?,?)`, now.UTC(), identityID, newPub, newRecovery); err != nil {
			return err
		}
		changed, err := tx.exec(ctx, `UPDATE native_identities SET public_key=?,recovery_public_key=?,version=version+1,updated_at=? WHERE id=? AND version=?`, newPub, newRecovery, now.UTC(), identityID, expectedVersion)
		if err != nil {
			return nativeKeyError(err)
		}
		count, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return storeport.ErrInvalidNativeIdentity
		}
		for _, q := range []string{`UPDATE native_sessions SET revoked_at=COALESCE(revoked_at,?) WHERE identity_id=?`, `UPDATE native_challenges SET consumed_at=COALESCE(consumed_at,?) WHERE identity_id=?`} {
			if _, err = tx.exec(ctx, q, now.UTC(), identityID); err != nil {
				return err
			}
		}
		if _, err = tx.exec(ctx, `UPDATE tenant_memberships SET version=version+1,updated_at=? WHERE user_id=?`, now.UTC(), n.UserID); err != nil {
			return err
		}
		if _, err = tx.exec(ctx, `UPDATE users SET token_version=token_version+1,updated_at=? WHERE id=?`, now.UTC(), n.UserID); err != nil {
			return err
		}
		result, err = tx.nativeIdentityByID(ctx, identityID, false)
		return err
	})
	return result, err
}

func nativeKeyError(err error) error {
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(err.Error(), "23505")) {
		return fmt.Errorf("%w: key registration rejected", storeport.ErrNativeKeyConflict)
	}
	return err
}

func (s *Store) BootstrapNativeOwner(ctx context.Context, actor models.ControlPlanePrincipal, request models.NativeOwnerProvision) (*models.NativeIdentity, error) {
	return s.provisionNativeIdentity(ctx, actor, request, []string{models.RoleOwner}, true)
}

func (s *Store) ProvisionNativeMember(ctx context.Context, actor models.ControlPlanePrincipal, request models.NativeOwnerProvision, roles []string) (*models.NativeIdentity, error) {
	for _, role := range roles {
		switch role {
		case models.RoleLearner, models.RoleAdmin, models.RolePedagogyManager, models.RoleTrainer, models.RoleAuditor, models.RoleBillingAdmin:
		default:
			return nil, storeport.ErrInvalidNativeIdentity
		}
	}
	return s.provisionNativeIdentity(ctx, actor, request, roles, false)
}

func (s *Store) provisionNativeIdentity(ctx context.Context, actor models.ControlPlanePrincipal, request models.NativeOwnerProvision, roles []string, owner bool) (*models.NativeIdentity, error) {
	if !actor.Validate() || request.TenantID == "" || !validNativePublicKey(request.PublicKey) || !validNativePublicKey(request.RecoveryPublicKey) || request.PublicKey == request.RecoveryPublicKey || len(strings.TrimSpace(request.DisplayName)) == 0 || len(request.DisplayName) > 160 {
		return nil, storeport.ErrInvalidNativeIdentity
	}
	rolesJSON, err := validatedRolesJSON(roles)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var result *models.NativeIdentity
	err = s.withTenantControlTx(ctx, request.TenantID, actor.ActorID, func(tx *Store) error {
		query := `SELECT id FROM tenants WHERE id=? AND status='active'`
		if tx.dialect == DialectPostgres {
			query += ` FOR UPDATE`
		}
		var tenantID string
		if err := tx.queryRow(ctx, query, request.TenantID).Scan(&tenantID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storeport.ErrInvalidNativeIdentity
			}
			return err
		}
		if owner {
			rows, err := tx.query(ctx, `SELECT roles_json FROM tenant_memberships WHERE tenant_id=?`, tenantID)
			if err != nil {
				return err
			}
			hasOwner := false
			for rows.Next() {
				var raw string
				var existing []string
				if err = rows.Scan(&raw); err != nil {
					rows.Close()
					return err
				}
				if err = json.Unmarshal([]byte(raw), &existing); err != nil {
					rows.Close()
					return err
				}
				hasOwner = hasOwner || slices.Contains(existing, models.RoleOwner)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if hasOwner {
				return fmt.Errorf("native owner already provisioned: %w", storeport.ErrAlreadyMember)
			}
			var prior int
			if err = tx.queryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id=? AND action='membership.bootstrap_native_owner'`, tenantID).Scan(&prior); err != nil {
				return err
			}
			if prior != 0 {
				return fmt.Errorf("native owner bootstrap already consumed: %w", storeport.ErrAlreadyMember)
			}
		}
		var identityID string
		err := tx.queryRow(ctx, `SELECT id FROM native_identities WHERE public_key=? AND recovery_public_key=? AND status='active'`, request.PublicKey, request.RecoveryPublicKey).Scan(&identityID)
		if errors.Is(err, sql.ErrNoRows) {
			for _, key := range []string{request.PublicKey, request.RecoveryPublicKey} {
				var count int
				if err = tx.queryRow(ctx, `SELECT COUNT(*) FROM native_identity_keys WHERE public_key=?`, key).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					return storeport.ErrNativeKeyConflict
				}
			}
			identityID, err = generateID()
			if err != nil {
				return err
			}
			userID, err := generateID()
			if err != nil {
				return err
			}
			if _, err = tx.exec(ctx, `INSERT INTO users(id,email,normalized_email,password_hash,status,email_verified_at,token_version,created_at,updated_at,identity_mode,login_name) VALUES(?,NULL,NULL,NULL,'active',NULL,1,?,?,'pubkey',NULL)`, userID, now, now); err != nil {
				return err
			}
			if _, err = tx.exec(ctx, `INSERT INTO native_identities(id,user_id,public_key,recovery_public_key,display_name,status,version,created_at,updated_at) VALUES(?,?,?,?,?,'active',1,?,?)`, identityID, userID, request.PublicKey, request.RecoveryPublicKey, strings.TrimSpace(request.DisplayName), now, now); err != nil {
				return nativeKeyError(err)
			}
			for _, key := range [][2]string{{request.PublicKey, "identity"}, {request.RecoveryPublicKey, "recovery"}} {
				if _, err = tx.exec(ctx, `INSERT INTO native_identity_keys(public_key,identity_id,purpose,created_at) VALUES(?,?,?,?)`, key[0], identityID, key[1], now); err != nil {
					return nativeKeyError(err)
				}
			}
		} else if err != nil {
			return err
		}
		n, err := tx.nativeIdentityByID(ctx, identityID, true)
		if err != nil {
			return err
		}
		var membershipCount int
		if err = tx.queryRow(ctx, `SELECT COUNT(*) FROM tenant_memberships WHERE tenant_id=? AND user_id=?`, tenantID, n.UserID).Scan(&membershipCount); err != nil {
			return err
		}
		if membershipCount != 0 {
			return storeport.ErrAlreadyMember
		}
		membershipID, err := generateID()
		if err != nil {
			return err
		}
		learnerID := ""
		if slices.Contains(roles, models.RoleLearner) {
			learnerID, err = generateID()
			if err != nil {
				return err
			}
			profile, _ := json.Marshal(map[string]string{"name": strings.TrimSpace(request.DisplayName)})
			if _, err = tx.exec(ctx, `INSERT INTO learners(id,email,password_hash,objective,profile_json,created_at,last_active,email_verified_at,tenant_id,user_id,membership_id,identity_mode) VALUES(?,NULL,NULL,'',?,?,?,NULL,?,?,?,'pubkey')`, learnerID, string(profile), now, now, tenantID, n.UserID, membershipID); err != nil {
				return err
			}
		}
		if _, err = tx.exec(ctx, `INSERT INTO tenant_memberships(id,tenant_id,user_id,learner_id,roles_json,status,version,created_at,updated_at,mfa_required) VALUES(?,?,?,?,?,'active',1,?,?,0)`, membershipID, tenantID, n.UserID, nullString(learnerID), rolesJSON, now, now); err != nil {
			return err
		}
		action := "membership.provision_native_member"
		if owner {
			action = "membership.bootstrap_native_owner"
		}
		if err = tx.appendControlPlaneAudit(ctx, tenantID, actor, action, "membership", membershipID); err != nil {
			return err
		}
		n.TenantID = tenantID
		n.MembershipID = membershipID
		n.LearnerID = learnerID
		result = n
		return nil
	})
	return result, err
}

// BindNativeIdentity is an audited operator-only migration by exact user ID.
// It preserves memberships, roles and learning data, with no address matching.
func (s *Store) BindNativeIdentity(ctx context.Context, actor models.ControlPlanePrincipal, userID string, request models.NativeOwnerProvision) (*models.NativeIdentity, error) {
	if !actor.Validate() || userID == "" || request.TenantID == "" || !validNativePublicKey(request.PublicKey) || !validNativePublicKey(request.RecoveryPublicKey) || request.PublicKey == request.RecoveryPublicKey {
		return nil, storeport.ErrInvalidNativeIdentity
	}
	var result *models.NativeIdentity
	now := time.Now().UTC()
	err := s.withNativeTx(ctx, request.TenantID, userID, "", func(tx *Store) error {
		query := `SELECT u.id FROM users u JOIN tenant_memberships tm ON tm.user_id=u.id JOIN tenants t ON t.id=tm.tenant_id AND t.status='active' WHERE u.id=? AND u.status='active' AND u.identity_mode<>'pubkey' AND tm.tenant_id=? AND tm.status='active'`
		if tx.dialect == DialectPostgres {
			query += ` FOR UPDATE OF u,tm`
		}
		var existing string
		if err := tx.queryRow(ctx, query, userID, request.TenantID).Scan(&existing); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storeport.ErrInvalidNativeIdentity
			}
			return err
		}
		for _, key := range []string{request.PublicKey, request.RecoveryPublicKey} {
			var count int
			if err := tx.queryRow(ctx, `SELECT COUNT(*) FROM native_identity_keys WHERE public_key=?`, key).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return storeport.ErrNativeKeyConflict
			}
		}
		id, err := generateID()
		if err != nil {
			return err
		}
		name := strings.TrimSpace(request.DisplayName)
		if name == "" {
			name = "Membre"
		}
		if len(name) > 160 {
			return storeport.ErrInvalidNativeIdentity
		}
		if _, err = tx.exec(ctx, `UPDATE users SET identity_mode='pubkey',email=NULL,normalized_email=NULL,password_hash=NULL,email_verified_at=NULL,login_name=NULL,token_version=token_version+1,updated_at=? WHERE id=?`, now, userID); err != nil {
			return err
		}
		if _, err = tx.exec(ctx, `INSERT INTO native_identities(id,user_id,public_key,recovery_public_key,display_name,status,version,created_at,updated_at) VALUES(?,?,?,?,?,'active',1,?,?)`, id, userID, request.PublicKey, request.RecoveryPublicKey, name, now, now); err != nil {
			return nativeKeyError(err)
		}
		for _, key := range [][2]string{{request.PublicKey, "identity"}, {request.RecoveryPublicKey, "recovery"}} {
			if _, err = tx.exec(ctx, `INSERT INTO native_identity_keys(public_key,identity_id,purpose,created_at) VALUES(?,?,?,?)`, key[0], id, key[1], now); err != nil {
				return nativeKeyError(err)
			}
		}
		rows, err := tx.query(ctx, `SELECT DISTINCT tenant_id FROM tenant_memberships WHERE user_id=? ORDER BY tenant_id`, userID)
		if err != nil {
			return err
		}
		var tenants []string
		for rows.Next() {
			var tenant string
			if err = rows.Scan(&tenant); err != nil {
				rows.Close()
				return err
			}
			tenants = append(tenants, tenant)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, tenant := range tenants {
			if tx.dialect == DialectPostgres {
				if _, err = tx.exec(ctx, `SELECT set_config('app.current_tenant',?,true)`, tenant); err != nil {
					return err
				}
			}
			if _, err = tx.exec(ctx, `UPDATE learners SET identity_mode='pubkey',email=NULL,password_hash=NULL,email_verified_at=NULL WHERE tenant_id=? AND user_id=?`, tenant, userID); err != nil {
				return err
			}
			if _, err = tx.exec(ctx, `UPDATE tenant_memberships SET mfa_required=0,mfa_verified_at=NULL,version=version+1,updated_at=? WHERE tenant_id=? AND user_id=?`, now, tenant, userID); err != nil {
				return err
			}
			for _, table := range []string{"oauth_codes", "refresh_tokens", "account_tokens", "login_challenges", "learner_approved_clients", "credential_tenant_routes"} {
				if _, err = tx.exec(ctx, `DELETE FROM `+table+` WHERE tenant_id=? AND user_id=?`, tenant, userID); err != nil {
					return err
				}
			}
		}
		for _, table := range []string{"console_sessions", "user_password_resets", "mfa_credentials", "mfa_recovery_codes", "external_identities"} {
			if _, err = tx.exec(ctx, `DELETE FROM `+table+` WHERE user_id=?`, userID); err != nil {
				return err
			}
		}
		if tx.dialect == DialectPostgres {
			if _, err = tx.exec(ctx, `SELECT set_config('app.current_tenant',?,true)`, request.TenantID); err != nil {
				return err
			}
		}
		if err = tx.appendControlPlaneAudit(ctx, request.TenantID, actor, "identity.bind_native_key", "user", userID); err != nil {
			return err
		}
		result, err = tx.GetNativeIdentity(ctx, request.TenantID, request.PublicKey, "session")
		return err
	})
	return result, err
}
