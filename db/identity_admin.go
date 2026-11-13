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
			return "", fmt.Errorf("invalid membership role %q", role)
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
