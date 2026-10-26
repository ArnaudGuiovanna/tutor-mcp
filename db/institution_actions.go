// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var _ storeport.InstitutionActionsStore = (*Store)(nil)

func (s *Store) InstitutionAction(ctx context.Context, p models.Principal, action string, in models.InstitutionRequest) (any, error) {
	if !models.ValidInstitutionAction(action) || len(in.Query) > 200 || len(in.After) > 255 || len(in.MemberID) > 255 || len(in.Reason) > 4000 || len(in.Name) > 160 {
		return nil, fmt.Errorf("invalid institution request")
	}
	if !models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeFormationContribute) {
		return nil, storeport.ErrInvalidPrincipal
	}
	read := models.InstitutionReadAction(action)
	if !read && (!in.Confirm || in.Key == "" || len(in.Key) > 128) {
		return nil, fmt.Errorf("confirmation and key required")
	}
	if action != "overview" && action != "request_erasure" && action != "get_erasure_status" && !p.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: p.TenantID}) {
		return nil, storeport.ErrInvalidPrincipal
	}
	var out any
	err := s.WithTenantTx(ctx, p.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		tx := scoped.(*Store)
		if err := tx.ValidatePrincipal(txCtx, p); err != nil {
			return err
		}
		var hash string
		if !read {
			if err := tx.lockMembershipChanges(txCtx, p.TenantID); err != nil {
				return err
			}
			// Hold the tenant lock before receipt lookup and revalidate after a
			// concurrent role change. Identical retries observe the first receipt.
			if tx.dialect == DialectPostgres {
				var id string
				if err := tx.queryRow(txCtx, `SELECT id FROM tenants WHERE id=? FOR UPDATE`, p.TenantID).Scan(&id); err != nil {
					return err
				}
				if err := tx.ValidatePrincipal(txCtx, p); err != nil {
					return err
				}
			}
			var err error
			hash, err = catalogRequestHash(struct {
				Action string
				Input  models.InstitutionRequest
			}{action, in})
			if err != nil {
				return err
			}
			var oldAction, oldHash, actor, raw string
			err = tx.queryRow(txCtx, `SELECT operation,request_hash,actor_user_id,response_json FROM training_receipts WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, "institution:"+in.Key).Scan(&oldAction, &oldHash, &actor, &raw)
			if err == nil {
				if oldAction != action || oldHash != hash || actor != p.UserID {
					return storeport.ErrIdempotencyKeyConflict
				}
				return json.Unmarshal([]byte(raw), &out)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var err error
		switch action {
		case "overview":
			out, err = tx.institutionOverview(txCtx, p)
		case "list_members":
			out, err = tx.institutionMembers(txCtx, p, in)
		case "invite_member":
			out, err = tx.inviteNativeInstitutionMember(txCtx, p, in)
		case "set_member_roles", "remove_member":
			var raw string
			if err = tx.queryRow(txCtx, `SELECT roles_json FROM tenant_memberships WHERE tenant_id=? AND id=? AND status<>'revoked'`, p.TenantID, in.MemberID).Scan(&raw); err != nil {
				return storeport.WrapNotFound(err)
			}
			roles := in.Roles
			status := models.MembershipStatusActive
			if action == "remove_member" {
				if err = json.Unmarshal([]byte(raw), &roles); err != nil {
					return err
				}
				status = models.MembershipStatusRevoked
			}
			if err = tx.UpdateTenantMember(txCtx, p, in.MemberID, status, roles); err != nil {
				return err
			}
			if err = tx.revokeInstitutionMemberSessions(txCtx, p.TenantID, in.MemberID); err != nil {
				return err
			}
			out = map[string]any{"status": status, "member_id": in.MemberID, "reconnect_required": in.MemberID == p.MembershipID}
		case "transfer_ownership":
			out, err = tx.transferInstitutionOwnership(txCtx, p, in)
		case "request_erasure":
			out, err = tx.requestOwnInstitutionErasure(txCtx, p, in)
		case "get_erasure_status":
			var requests []map[string]any
			requests, err = tx.ownInstitutionErasures(txCtx, p, in.RequestID)
			if err == nil {
				if len(requests) != 1 {
					return storeport.ErrNotFound
				}
				out = map[string]any{"request": requests[0]}
			}
		}
		if err != nil {
			return err
		}
		if !read {
			raw, err := json.Marshal(out)
			if err != nil {
				return err
			}
			_, err = tx.exec(txCtx, `INSERT INTO training_receipts(tenant_id,membership_id,learner_id,idempotency_key,operation,request_hash,response_json,actor_user_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, p.TenantID, p.MembershipID, nullableTrainingLearner(p.LearnerID), "institution:"+in.Key, action, hash, string(raw), p.UserID, time.Now().UTC())
			return err
		}
		return nil
	})
	return out, err
}

func (s *Store) institutionOverview(ctx context.Context, p models.Principal) (any, error) {
	var name, status string
	if err := s.queryRow(ctx, `SELECT t.name,m.status FROM tenants t JOIN tenant_memberships m ON m.tenant_id=t.id WHERE t.id=? AND m.id=? AND m.user_id=?`, p.TenantID, p.MembershipID, p.UserID).Scan(&name, &status); err != nil {
		return nil, err
	}
	owners, err := s.countActiveOwners(ctx, p.TenantID, p.MembershipID)
	if err != nil {
		return nil, err
	}
	lastOwner := trainingOwner(p) && owners == 0
	policy, err := s.trainingPolicy(ctx, p, models.TrainingRequest{}, false)
	if err != nil {
		return nil, err
	}
	requests, err := s.ownInstitutionErasures(ctx, p, "")
	if err != nil {
		return nil, err
	}
	return map[string]any{"institution": map[string]any{"id": p.TenantID, "name": name}, "membership": map[string]any{"id": p.MembershipID, "roles": p.Roles, "status": status}, "capabilities": map[string]bool{"manage_members": p.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: p.TenantID}), "transfer_ownership": trainingOwner(p), "request_erasure": !lastOwner, "last_owner": lastOwner}, "personal_learning": policy.(map[string]any)["personal_learning"], "erasure_requests": requests}, nil
}

func (s *Store) institutionMembers(ctx context.Context, p models.Principal, in models.InstitutionRequest) (any, error) {
	if in.Limit == 0 {
		in.Limit = 20
	}
	if validatePage(in.Limit) != nil {
		return nil, fmt.Errorf("invalid page")
	}
	publicQuery := in.Query
	if normalized, err := NormalizeNativePublicKey(in.Query); err == nil {
		publicQuery = normalized
	}
	rows, err := s.query(ctx, `SELECT m.id,m.user_id,COALESCE(n.display_name,u.email,''),COALESCE(n.public_key,''),m.roles_json,m.status FROM tenant_memberships m JOIN users u ON u.id=m.user_id LEFT JOIN native_identities n ON n.user_id=m.user_id WHERE m.tenant_id=? AND m.status<>'revoked' AND m.id>? AND (lower(COALESCE(n.display_name,u.email,'')) LIKE ? OR COALESCE(n.public_key,'') LIKE ?) ORDER BY m.id LIMIT ?`, p.TenantID, in.After, "%"+strings.ToLower(in.Query)+"%", "%"+publicQuery+"%", in.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []models.InstitutionMember{}
	for rows.Next() {
		var m models.InstitutionMember
		var raw string
		if err = rows.Scan(&m.MembershipID, &m.UserID, &m.Name, &m.PublicKey, &raw, &m.Status); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &m.Roles); err != nil {
			return nil, err
		}
		m.IsSelf = m.MembershipID == p.MembershipID
		items = append(items, m)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	after := ""
	if len(items) > in.Limit {
		items = items[:in.Limit]
		after = items[len(items)-1].MembershipID
	}
	return map[string]any{"items": items, "next_after": after}, nil
}

// Accept public identifiers only. Private keys are neither needed nor accepted.
func institutionPublicKey(raw string) (string, error) {
	key, err := NormalizeNativePublicKey(raw)
	if err != nil {
		return "", storeport.ErrInvalidNativeIdentity
	}
	return key, nil
}

func (s *Store) inviteNativeInstitutionMember(ctx context.Context, p models.Principal, in models.InstitutionRequest) (any, error) {
	primary, err := institutionPublicKey(in.Npub)
	if err != nil {
		return nil, err
	}
	recovery, err := institutionPublicKey(in.RecoveryNpub)
	if err != nil {
		return nil, err
	}
	if primary == recovery || strings.TrimSpace(in.Name) == "" {
		return nil, storeport.ErrInvalidNativeIdentity
	}
	for _, role := range in.Roles {
		switch role {
		case models.RoleLearner, models.RoleAdmin, models.RolePedagogyManager, models.RoleTrainer, models.RoleAuditor, models.RoleBillingAdmin:
		default:
			return nil, storeport.ErrInvalidPrincipal
		}
	}
	roles, err := validatedRolesJSON(in.Roles)
	if err != nil {
		return nil, err
	}
	var userID string
	err = s.queryRow(ctx, `SELECT user_id FROM native_identities WHERE public_key=? AND recovery_public_key=? AND status='active'`, primary, recovery).Scan(&userID)
	now := time.Now().UTC()
	if errors.Is(err, sql.ErrNoRows) {
		for _, key := range []string{primary, recovery} {
			var count int
			if err = s.queryRow(ctx, `SELECT COUNT(*) FROM native_identity_keys WHERE public_key=?`, key).Scan(&count); err != nil {
				return nil, err
			}
			if count != 0 {
				return nil, storeport.ErrNativeKeyConflict
			}
		}
		identityID, err := generateID()
		if err != nil {
			return nil, err
		}
		userID, err = generateID()
		if err != nil {
			return nil, err
		}
		if _, err = s.exec(ctx, `INSERT INTO users(id,email,normalized_email,password_hash,status,email_verified_at,token_version,created_at,updated_at,identity_mode,login_name) VALUES(?,NULL,NULL,NULL,'active',NULL,1,?,?,'pubkey',NULL)`, userID, now, now); err != nil {
			return nil, err
		}
		if _, err = s.exec(ctx, `INSERT INTO native_identities(id,user_id,public_key,recovery_public_key,display_name,status,version,created_at,updated_at) VALUES(?,?,?,?,?,'active',1,?,?)`, identityID, userID, primary, recovery, strings.TrimSpace(in.Name), now, now); err != nil {
			return nil, nativeKeyError(err)
		}
		for _, entry := range [][2]string{{primary, "identity"}, {recovery, "recovery"}} {
			if _, err = s.exec(ctx, `INSERT INTO native_identity_keys(public_key,identity_id,purpose,created_at) VALUES(?,?,?,?)`, entry[0], identityID, entry[1], now); err != nil {
				return nil, nativeKeyError(err)
			}
		}
	} else if err != nil {
		return nil, err
	}
	var existing int
	if err = s.queryRow(ctx, `SELECT COUNT(*) FROM tenant_memberships WHERE tenant_id=? AND user_id=?`, p.TenantID, userID).Scan(&existing); err != nil {
		return nil, err
	}
	if existing != 0 {
		return nil, storeport.ErrAlreadyMember
	}
	member, err := generateID()
	if err != nil {
		return nil, err
	}
	if _, err = s.exec(ctx, `INSERT INTO tenant_memberships(id,tenant_id,user_id,learner_id,roles_json,status,version,created_at,updated_at,mfa_required) VALUES(?,?,?,NULL,?,'active',1,?,?,0)`, member, p.TenantID, userID, roles, now, now); err != nil {
		return nil, err
	}
	if slices.Contains(in.Roles, models.RoleLearner) {
		if err = s.createInvitedLearnerProfile(ctx, models.TenantScope{TenantID: p.TenantID, UserID: userID, MembershipID: member}, now); err != nil {
			return nil, err
		}
	}
	if err = s.AppendAuditEvent(ctx, p, models.AuditEvent{Action: "membership.invite_native", TargetType: "membership", TargetID: member, Reason: in.Reason}); err != nil {
		return nil, err
	}
	return map[string]any{"status": "active", "member_id": member}, nil
}

func (s *Store) revokeInstitutionMemberSessions(ctx context.Context, tenantID, memberID string) error {
	if _, err := s.exec(ctx, `UPDATE native_sessions SET revoked_at=? WHERE tenant_id=? AND membership_id=? AND revoked_at IS NULL`, time.Now().UTC(), tenantID, memberID); err != nil {
		return err
	}
	_, err := s.exec(ctx, `DELETE FROM console_sessions WHERE tenant_id=? AND membership_id=?`, tenantID, memberID)
	return err
}

func (s *Store) transferInstitutionOwnership(ctx context.Context, p models.Principal, in models.InstitutionRequest) (any, error) {
	if !trainingOwner(p) || in.MemberID == p.MembershipID {
		return nil, storeport.ErrInvalidPrincipal
	}
	var user, learner, raw string
	if err := s.queryRow(ctx, `SELECT user_id,COALESCE(learner_id,''),roles_json FROM tenant_memberships WHERE tenant_id=? AND id=? AND status='active'`, p.TenantID, in.MemberID).Scan(&user, &learner, &raw); err != nil {
		return nil, storeport.WrapNotFound(err)
	}
	var roles []string
	if err := json.Unmarshal([]byte(raw), &roles); err != nil {
		return nil, err
	}
	if !slices.Contains(roles, models.RoleOwner) {
		roles = append(roles, models.RoleOwner)
	}
	target := models.TenantScope{TenantID: p.TenantID, UserID: user, MembershipID: in.MemberID, LearnerID: learner}
	if _, err := s.SetMembershipAuthorization(ctx, target, models.MembershipStatusActive, roles); err != nil {
		return nil, err
	}
	previous := []string{}
	for _, role := range p.Roles {
		if role != models.RoleOwner {
			previous = append(previous, role)
		}
	}
	if len(previous) == 0 {
		previous = []string{models.RoleAdmin}
	}
	if _, err := s.SetMembershipAuthorization(ctx, p.TenantScope(), models.MembershipStatusActive, previous); err != nil {
		return nil, err
	}
	if _, err := s.exec(ctx, `UPDATE formations SET owner_membership_id=?,updated_at=? WHERE tenant_id=? AND owner_membership_id=?`, in.MemberID, time.Now().UTC(), p.TenantID, p.MembershipID); err != nil {
		return nil, err
	}
	for _, member := range []string{p.MembershipID, in.MemberID} {
		if err := s.revokeInstitutionMemberSessions(ctx, p.TenantID, member); err != nil {
			return nil, err
		}
	}
	if err := s.AppendAuditEvent(ctx, p, models.AuditEvent{Action: "membership.transfer_owner", TargetType: "membership", TargetID: in.MemberID, Reason: in.Reason}); err != nil {
		return nil, err
	}
	return map[string]any{"status": "active", "member_id": in.MemberID, "reconnect_required": true}, nil
}

func (s *Store) ownInstitutionErasures(ctx context.Context, p models.Principal, id string) ([]map[string]any, error) {
	items := []map[string]any{}
	rows, err := s.query(ctx, `SELECT id,status,created_at,completed_at FROM tenant_dsar_requests WHERE tenant_id=? AND learner_id=? AND requested_by=? AND kind='erase' AND (?='' OR id=?) ORDER BY created_at DESC LIMIT 20`, p.TenantID, p.LearnerID, p.UserID, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		var created time.Time
		var completed sql.NullTime
		if err = rows.Scan(&id, &status, &created, &completed); err != nil {
			return nil, err
		}
		item := map[string]any{"id": id, "status": status, "created_at": created}
		if completed.Valid {
			item["completed_at"] = completed.Time
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) requestOwnInstitutionErasure(ctx context.Context, p models.Principal, in models.InstitutionRequest) (any, error) {
	owners, err := s.countActiveOwners(ctx, p.TenantID, p.MembershipID)
	if err != nil {
		return nil, err
	}
	if trainingOwner(p) && owners == 0 {
		return nil, fmt.Errorf("transfer ownership before erasure")
	}
	if p.LearnerID == "" {
		if _, err = s.SetMembershipAuthorization(ctx, p.TenantScope(), models.MembershipStatusRevoked, p.Roles); err != nil {
			return nil, err
		}
		if err = s.revokeInstitutionMemberSessions(ctx, p.TenantID, p.MembershipID); err != nil {
			return nil, err
		}
		if err = s.AppendAuditEvent(ctx, p, models.AuditEvent{Action: "membership.self_remove", TargetType: "membership", TargetID: p.MembershipID, Reason: in.Reason}); err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		return map[string]any{"request": map[string]any{"id": "membership:" + p.MembershipID, "status": "completed", "created_at": now, "completed_at": now}, "reconnect_required": true}, nil
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "member requested erasure of their institution data"
	}
	request, err := s.RequestTenantDSAR(ctx, p, p.LearnerID, "erase", reason)
	if err != nil {
		return nil, err
	}
	return map[string]any{"request": map[string]any{"id": request.ID, "status": request.Status, "created_at": request.CreatedAt}}, nil
}
