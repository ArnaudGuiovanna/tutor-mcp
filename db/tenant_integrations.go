// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func ConfigureTenantIntegrationAllowedHosts(s *Store, hosts []string) error {
	if s == nil {
		return fmt.Errorf("tenant integration store is required")
	}
	allowed := make(map[string]struct{}, len(hosts))
	for _, raw := range hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" || net.ParseIP(host) != nil || strings.ContainsAny(host, "/:@") || strings.Contains(host, "..") {
			return fmt.Errorf("invalid tenant integration host")
		}
		allowed[host] = struct{}{}
	}
	s.integrationAllowedHosts = allowed
	return nil
}

func (s *Store) validateTenantIntegrationURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" ||
		u.Hostname() == "" || (u.Port() != "" && u.Port() != "443") || net.ParseIP(u.Hostname()) != nil {
		return fmt.Errorf("tenant integration endpoint must be an allowlisted HTTPS hostname")
	}
	if _, ok := s.integrationAllowedHosts[strings.ToLower(u.Hostname())]; !ok {
		return fmt.Errorf("tenant integration endpoint host is not allowlisted")
	}
	return nil
}

func tenantIntegrationAAD(tenantID, integrationID string, version int64) []byte {
	return []byte("tutor-mcp\x00tenant_integrations\x00" + tenantID + "\x00" + integrationID + "\x00" + strconv.FormatInt(version, 10))
}

const sqliteTenantIntegrationSecretHistoryMigration = `
CREATE TABLE tenant_integration_secret_versions (
    tenant_id TEXT NOT NULL,
    integration_id TEXT NOT NULL,
    secret_version INTEGER NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    key_id TEXT NOT NULL,
    active_from DATETIME NOT NULL,
    valid_until DATETIME,
    created_by TEXT NOT NULL,
    PRIMARY KEY (tenant_id, integration_id, secret_version),
    FOREIGN KEY (tenant_id, integration_id) REFERENCES tenant_integrations(tenant_id, id)
);
INSERT INTO tenant_integration_secret_versions
    (tenant_id, integration_id, secret_version, secret_ciphertext, key_id,
     active_from, valid_until, created_by)
SELECT tenant_id, id, secret_version, secret_ciphertext, key_id,
       created_at, NULL, created_by
FROM tenant_integrations;
`

const postgresTenantIntegrationSecretHistoryMigration = `
CREATE TABLE tenant_integration_secret_versions (
    tenant_id TEXT NOT NULL,
    integration_id TEXT NOT NULL,
    secret_version BIGINT NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    key_id TEXT NOT NULL,
    active_from TIMESTAMPTZ NOT NULL,
    valid_until TIMESTAMPTZ,
    created_by TEXT NOT NULL,
    PRIMARY KEY (tenant_id, integration_id, secret_version),
    FOREIGN KEY (tenant_id, integration_id) REFERENCES tenant_integrations(tenant_id, id)
);
INSERT INTO tenant_integration_secret_versions
    (tenant_id, integration_id, secret_version, secret_ciphertext, key_id,
     active_from, valid_until, created_by)
SELECT tenant_id, id, secret_version, secret_ciphertext, key_id,
       created_at, NULL, created_by
FROM tenant_integrations;
ALTER TABLE tenant_integration_secret_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_integration_secret_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_tenant_integration_secret_versions ON tenant_integration_secret_versions
    USING (tenant_id = current_setting('app.current_tenant', true))
    WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`

func (s *Store) CreateTenantIntegration(ctx context.Context, actor models.Principal, kind, endpointURL string, eventTypes []string) (*models.TenantIntegration, string, error) {
	if !actor.Authorize(models.PermissionIntegrationManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		strings.TrimSpace(kind) == "" || len(eventTypes) == 0 {
		return nil, "", fmt.Errorf("create tenant integration: invalid authorization or input")
	}
	if err := s.validateTenantIntegrationURL(endpointURL); err != nil {
		return nil, "", err
	}
	if s.secretKeyring == nil {
		return nil, "", fmt.Errorf("create tenant integration: secret keyring is required")
	}
	events := append([]string(nil), eventTypes...)
	sort.Strings(events)
	for i, eventType := range events {
		if strings.TrimSpace(eventType) == "" || (i > 0 && eventType == events[i-1]) {
			return nil, "", fmt.Errorf("create tenant integration: invalid event types")
		}
	}
	eventsJSON, _ := json.Marshal(events)
	id, err := generateID()
	if err != nil {
		return nil, "", err
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, "", err
	}
	rawSecret := base64.RawURLEncoding.EncodeToString(secretBytes)
	ciphertext, err := s.secretKeyring.encryptWithAAD(rawSecret, tenantIntegrationAAD(actor.TenantID, id, 1))
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	integration := &models.TenantIntegration{
		ID: id, TenantID: actor.TenantID, Kind: kind, EndpointURL: endpointURL,
		EventTypesJSON: string(eventsJSON), SecretVersion: 1, Status: "active",
		CreatedBy: actor.UserID, CreatedAt: now, UpdatedAt: now,
	}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.exec(txCtx, `INSERT INTO tenant_integrations
			(id, tenant_id, kind, endpoint_url, event_types_json, secret_ciphertext,
			 key_id, secret_version, status, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, 'active', ?, ?, ?)`, id, actor.TenantID,
			kind, endpointURL, string(eventsJSON), ciphertext, integrationSecretKeyID(ciphertext),
			actor.UserID, now, now); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `INSERT INTO tenant_integration_secret_versions
			(tenant_id, integration_id, secret_version, secret_ciphertext, key_id,
			 active_from, valid_until, created_by)
			VALUES (?, ?, 1, ?, ?, ?, NULL, ?)`, actor.TenantID, id, ciphertext,
			integrationSecretKeyID(ciphertext), now, actor.UserID); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "integration.create", TargetType: "tenant_integration", TargetID: id,
		})
	})
	if err != nil {
		return nil, "", err
	}
	return integration, rawSecret, nil
}

// RotateTenantIntegrationSecret returns the new plaintext exactly once and
// retains the preceding encrypted version for a bounded receiver overlap.
// Consumers should accept both advertised versions until ValidUntil.
func (s *Store) RotateTenantIntegrationSecret(ctx context.Context, actor models.Principal, integrationID string, overlap time.Duration) (*models.TenantIntegrationSecretVersion, string, error) {
	if !actor.Authorize(models.PermissionIntegrationManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		integrationID == "" || overlap < 5*time.Minute || overlap > 7*24*time.Hour {
		return nil, "", fmt.Errorf("rotate tenant integration secret: invalid authorization or input")
	}
	if s.secretKeyring == nil {
		return nil, "", fmt.Errorf("rotate tenant integration secret: secret keyring is required")
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, "", err
	}
	rawSecret := base64.RawURLEncoding.EncodeToString(secretBytes)
	now := time.Now().UTC()
	var result *models.TenantIntegrationSecretVersion
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		query := `SELECT secret_version, status FROM tenant_integrations
			WHERE tenant_id = ? AND id = ?`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE`
		}
		var previousVersion int64
		var status string
		if err := txs.queryRow(txCtx, query, actor.TenantID, integrationID).Scan(&previousVersion, &status); err != nil || status != "active" {
			return fmt.Errorf("rotate tenant integration secret: active integration not found")
		}
		version := previousVersion + 1
		ciphertext, err := txs.secretKeyring.encryptWithAAD(rawSecret, tenantIntegrationAAD(actor.TenantID, integrationID, version))
		if err != nil {
			return err
		}
		validUntil := now.Add(overlap)
		update, err := txs.exec(txCtx, `UPDATE tenant_integration_secret_versions
			SET valid_until = ? WHERE tenant_id = ? AND integration_id = ?
			AND secret_version = ? AND valid_until IS NULL`, validUntil, actor.TenantID,
			integrationID, previousVersion)
		if err != nil {
			return err
		}
		if count, _ := update.RowsAffected(); count != 1 {
			return fmt.Errorf("rotate tenant integration secret: current history version missing")
		}
		if _, err := txs.exec(txCtx, `INSERT INTO tenant_integration_secret_versions
			(tenant_id, integration_id, secret_version, secret_ciphertext, key_id,
			 active_from, valid_until, created_by)
			VALUES (?, ?, ?, ?, ?, ?, NULL, ?)`, actor.TenantID, integrationID, version,
			ciphertext, integrationSecretKeyID(ciphertext), now, actor.UserID); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE tenant_integrations SET
			secret_ciphertext = ?, key_id = ?, secret_version = ?, updated_at = ?
			WHERE tenant_id = ? AND id = ? AND secret_version = ?`, ciphertext,
			integrationSecretKeyID(ciphertext), version, now, actor.TenantID,
			integrationID, previousVersion); err != nil {
			return err
		}
		if err := txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "integration.secret.rotate", TargetType: "tenant_integration",
			TargetID: integrationID, DetailsJSON: fmt.Sprintf(`{"secret_version":%d,"overlap_seconds":%d}`, version, int64(overlap.Seconds())),
		}); err != nil {
			return err
		}
		result = &models.TenantIntegrationSecretVersion{
			TenantID: actor.TenantID, IntegrationID: integrationID, Version: version,
			KeyID: integrationSecretKeyID(ciphertext), ActiveFrom: now, CreatedBy: actor.UserID,
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return result, rawSecret, nil
}

func (s *Store) ListTenantIntegrationSecretVersions(ctx context.Context, actor models.Principal, integrationID string) ([]models.TenantIntegrationSecretVersion, error) {
	if !actor.Authorize(models.PermissionIntegrationManage, models.AuthorizationResource{TenantID: actor.TenantID}) || integrationID == "" {
		return nil, fmt.Errorf("list tenant integration secret versions: invalid authorization or input")
	}
	var versions []models.TenantIntegrationSecretVersion
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		rows, err := scoped.(*Store).query(txCtx, `SELECT secret_version, key_id, active_from,
			valid_until, created_by FROM tenant_integration_secret_versions
			WHERE tenant_id = ? AND integration_id = ? ORDER BY secret_version DESC`, actor.TenantID, integrationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item := models.TenantIntegrationSecretVersion{TenantID: actor.TenantID, IntegrationID: integrationID}
			var validUntil sql.NullTime
			if err := rows.Scan(&item.Version, &item.KeyID, &item.ActiveFrom, &validUntil, &item.CreatedBy); err != nil {
				return err
			}
			if validUntil.Valid {
				until := validUntil.Time
				item.ValidUntil = &until
			}
			versions = append(versions, item)
		}
		return rows.Err()
	})
	return versions, err
}

func (s *Store) PrepareTenantWebhook(ctx context.Context, scope models.TenantScope, integrationID, eventID, eventType string, payload []byte, now time.Time) (*models.SignedWebhook, error) {
	if err := scope.Validate(); err != nil || integrationID == "" || eventID == "" || eventType == "" || !json.Valid(payload) {
		return nil, fmt.Errorf("prepare tenant webhook: invalid input")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var result *models.SignedWebhook
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var endpoint, eventsJSON, ciphertext string
		var version int64
		if err := txs.queryRow(txCtx, `SELECT endpoint_url, event_types_json, secret_ciphertext, secret_version
			FROM tenant_integrations WHERE tenant_id = ? AND id = ? AND status = 'active'`,
			scope.TenantID, integrationID).Scan(&endpoint, &eventsJSON, &ciphertext, &version); err != nil {
			return fmt.Errorf("prepare tenant webhook: integration not found")
		}
		if err := s.validateTenantIntegrationURL(endpoint); err != nil {
			return fmt.Errorf("prepare tenant webhook: stored endpoint rejected: %w", err)
		}
		var events []string
		if err := json.Unmarshal([]byte(eventsJSON), &events); err != nil {
			return err
		}
		if index := sort.SearchStrings(events, eventType); index >= len(events) || events[index] != eventType {
			return fmt.Errorf("prepare tenant webhook: event type is not subscribed")
		}
		secret, err := txs.secretKeyring.decryptWithAAD(ciphertext, tenantIntegrationAAD(scope.TenantID, integrationID, version))
		if err != nil {
			return err
		}
		timestamp := now.UTC().Unix()
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
		_, _ = mac.Write([]byte("." + eventID + "."))
		_, _ = mac.Write(payload)
		result = &models.SignedWebhook{
			IntegrationID: integrationID, EventID: eventID, EndpointURL: endpoint,
			Timestamp: timestamp, Signature: "v1=" + hex.EncodeToString(mac.Sum(nil)),
			SecretVersion: version, Payload: append([]byte(nil), payload...),
		}
		return nil
	})
	return result, err
}
