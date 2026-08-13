// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"tutor-mcp/models"

	"go.opentelemetry.io/otel/trace"
)

var tenantSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func (s *Store) ProvisionTenant(ctx context.Context, actor models.ControlPlanePrincipal, slug, name, region, planID string) (*models.Tenant, error) {
	if !actor.Validate() || !tenantSlugPattern.MatchString(slug) || strings.TrimSpace(name) == "" ||
		strings.TrimSpace(region) == "" || planID == "" {
		return nil, fmt.Errorf("provision tenant: invalid authority or input")
	}
	id := stableLearningID("tenant_", slug)
	now := time.Now().UTC()
	var tenant *models.Tenant
	err := s.withTenantControlTx(ctx, id, actor.ActorID, func(txs *Store) error {
		if _, err := txs.exec(ctx, `INSERT INTO tenants
			(id, slug, name, status, region, policy_json, created_at, updated_at)
			VALUES (?, ?, ?, 'active', ?, '{}', ?, ?)
			ON CONFLICT (slug) DO NOTHING`, id, slug, name, region, now, now); err != nil {
			return err
		}
		var current models.Tenant
		if err := txs.queryRow(ctx, `SELECT id, slug, name, status, region, policy_json, created_at, updated_at
			FROM tenants WHERE slug = ?`, slug).Scan(&current.ID, &current.Slug, &current.Name,
			&current.Status, &current.Region, &current.PolicyJSON, &current.CreatedAt, &current.UpdatedAt); err != nil {
			return err
		}
		if current.ID != id || current.Name != name || current.Region != region {
			return fmt.Errorf("provision tenant: idempotency conflict")
		}
		var entitlementsJSON string
		if err := txs.queryRow(ctx, `SELECT entitlements_json FROM plans
			WHERE id = ? AND status = 'active'`, planID).Scan(&entitlementsJSON); err != nil {
			return fmt.Errorf("provision tenant plan: %w", err)
		}
		periodEnd := now.AddDate(0, 1, 0)
		if _, err := txs.exec(ctx, `INSERT INTO tenant_subscriptions
			(tenant_id, plan_id, status, current_period_start, current_period_end, updated_at)
			VALUES (?, ?, 'active', ?, ?, ?)
			ON CONFLICT (tenant_id) DO NOTHING`, id, planID, now, periodEnd, now); err != nil {
			return err
		}
		var entitlements map[string]int64
		if err := json.Unmarshal([]byte(entitlementsJSON), &entitlements); err != nil {
			return fmt.Errorf("provision tenant entitlements: %w", err)
		}
		for key, limit := range entitlements {
			if _, err := txs.exec(ctx, `INSERT INTO tenant_entitlements
				(tenant_id, entitlement_key, hard_limit, used_value, reserved_value,
				 period_start, period_end, version, updated_at)
				VALUES (?, ?, ?, 0, 0, ?, ?, 1, ?)
				ON CONFLICT (tenant_id, entitlement_key) DO NOTHING`,
				id, key, limit, now, periodEnd, now); err != nil {
				return err
			}
		}
		if err := txs.appendControlPlaneAudit(ctx, id, actor, "tenant.provision", "tenant", id); err != nil {
			return err
		}
		tenant = &current
		return nil
	})
	return tenant, err
}

func (s *Store) SetTenantStatus(ctx context.Context, actor models.ControlPlanePrincipal, tenantID, status string) error {
	if !actor.Validate() || tenantID == "" ||
		(status != models.TenantStatusActive && status != models.TenantStatusSuspended && status != models.TenantStatusClosed) {
		return fmt.Errorf("set tenant status: invalid authority or input")
	}
	return s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		result, err := txs.exec(ctx, `UPDATE tenants SET status = ?, updated_at = ?
			WHERE id = ? AND status <> ?`, status, time.Now().UTC(), tenantID, status)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			var exists int
			if err := txs.queryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE id = ? AND status = ?`, tenantID, status).Scan(&exists); err != nil || exists != 1 {
				return fmt.Errorf("set tenant status: tenant not found")
			}
			return nil
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "tenant.status."+status, "tenant", tenantID)
	})
}

func (s *Store) SetTenantFeatureFlag(ctx context.Context, actor models.ControlPlanePrincipal, tenantID, key string, enabled bool) error {
	if !actor.Validate() || tenantID == "" || key == "" {
		return fmt.Errorf("set tenant feature flag: invalid authority or input")
	}
	return s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		_, err := txs.exec(ctx, `INSERT INTO tenant_feature_flags
			(tenant_id, flag_key, enabled, version, updated_at) VALUES (?, ?, ?, 1, ?)
			ON CONFLICT (tenant_id, flag_key) DO UPDATE SET enabled = excluded.enabled,
			version = tenant_feature_flags.version + 1, updated_at = excluded.updated_at`,
			tenantID, key, enabled, time.Now().UTC())
		if err != nil {
			return err
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "tenant.flag.set", "feature_flag", key)
	})
}

func (s *Store) BeginTenantDomainVerification(ctx context.Context, actor models.ControlPlanePrincipal, tenantID, hostname string) (string, error) {
	if !actor.Validate() || tenantID == "" {
		return "", fmt.Errorf("begin tenant domain verification: invalid authority")
	}
	hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	if hostname == "" || strings.ContainsAny(hostname, "/:@") || strings.Contains(hostname, "..") {
		return "", fmt.Errorf("begin tenant domain verification: invalid hostname")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := opaqueTokenHash(raw)
	now := time.Now().UTC()
	err := s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		_, err := txs.exec(ctx, `INSERT INTO tenant_domains
			(hostname, tenant_id, status, verification_hash, created_at)
			VALUES (?, ?, 'pending', ?, ?)
			ON CONFLICT (hostname) DO UPDATE SET tenant_id = excluded.tenant_id,
			 status = 'pending', verification_hash = excluded.verification_hash,
			 created_at = excluded.created_at, verified_at = NULL`, hostname, tenantID, hash, now)
		return err
	})
	return raw, err
}

func (s *Store) CompleteTenantDomainVerification(ctx context.Context, actor models.ControlPlanePrincipal, hostname, rawToken string) error {
	if !actor.Validate() || hostname == "" || rawToken == "" {
		return fmt.Errorf("complete tenant domain verification: invalid authority or input")
	}
	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	var tenantID string
	if err := s.queryRow(ctx, `SELECT tenant_id FROM tenant_domains WHERE hostname = ?`, hostname).Scan(&tenantID); err != nil {
		return fmt.Errorf("complete tenant domain verification: domain not found")
	}
	return s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		result, err := txs.exec(ctx, `UPDATE tenant_domains SET status = 'verified', verified_at = ?
			WHERE hostname = ? AND tenant_id = ? AND status = 'pending' AND verification_hash = ?`,
			time.Now().UTC(), hostname, tenantID, opaqueTokenHash(rawToken))
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("complete tenant domain verification: invalid token")
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "tenant.domain.verify", "tenant_domain", hostname)
	})
}

func ResolveTenantByHostname(ctx context.Context, s *Store, hostname string) (string, error) {
	hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	var tenantID string
	if err := s.queryRow(ctx, `SELECT route.tenant_id FROM tenant_domains route
		JOIN tenants tenant ON tenant.id = route.tenant_id AND tenant.status = 'active'
		WHERE route.hostname = ? AND route.status = 'verified'`, hostname).Scan(&tenantID); err != nil {
		return "", fmt.Errorf("resolve tenant hostname: not found")
	}
	return tenantID, nil
}

func (s *Store) appendControlPlaneAudit(ctx context.Context, tenantID string, actor models.ControlPlanePrincipal, action, targetType, targetID string) error {
	traceID := ""
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		traceID = span.TraceID().String()
	}
	_, err := s.exec(ctx, `INSERT INTO audit_events
		(tenant_id, actor_user_id, membership_id, action, target_type, target_id,
		 request_id, reason, details_json, result, trace_id, occurred_at)
		VALUES (?, ?, 'control_plane', ?, ?, ?, ?, ?, '{}', 'success', ?, ?)`, tenantID,
		actor.ActorID, action, targetType, targetID, actor.RequestID, actor.Reason, traceID, time.Now().UTC())
	return err
}
