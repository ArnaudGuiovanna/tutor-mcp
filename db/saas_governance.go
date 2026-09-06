// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const sqliteSaaSGovernanceMigration = `
ALTER TABLE audit_events ADD COLUMN result TEXT NOT NULL DEFAULT 'success';
ALTER TABLE audit_events ADD COLUMN trace_id TEXT NOT NULL DEFAULT '';
CREATE TABLE tenant_dsar_requests (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    learner_id TEXT NOT NULL REFERENCES learners(id),
    kind TEXT NOT NULL CHECK (kind IN ('export','erase','rectify')),
    status TEXT NOT NULL CHECK (status IN ('pending','processing','completed','blocked','failed')),
    requested_by TEXT NOT NULL,
    reason TEXT NOT NULL,
    result_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(result_json)),
    created_at DATETIME NOT NULL,
    completed_at DATETIME,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX idx_tenant_dsar_status ON tenant_dsar_requests(tenant_id, status, created_at);
CREATE TABLE tenant_dsar_phases (
    tenant_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    phase TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','running','completed','blocked','failed')),
    affected_rows INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, request_id, position),
    FOREIGN KEY (tenant_id, request_id) REFERENCES tenant_dsar_requests(tenant_id, id)
);
`

const postgresSaaSGovernanceMigration = `
ALTER TABLE audit_events ADD COLUMN result TEXT NOT NULL DEFAULT 'success';
ALTER TABLE audit_events ADD COLUMN trace_id TEXT NOT NULL DEFAULT '';
CREATE TABLE tenant_dsar_requests (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    learner_id TEXT NOT NULL REFERENCES learners(id),
    kind TEXT NOT NULL CHECK (kind IN ('export','erase','rectify')),
    status TEXT NOT NULL CHECK (status IN ('pending','processing','completed','blocked','failed')),
    requested_by TEXT NOT NULL,
    reason TEXT NOT NULL,
    result_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX idx_tenant_dsar_status ON tenant_dsar_requests(tenant_id, status, created_at);
CREATE TABLE tenant_dsar_phases (
    tenant_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    phase TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','running','completed','blocked','failed')),
    affected_rows BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, request_id, position),
    FOREIGN KEY (tenant_id, request_id) REFERENCES tenant_dsar_requests(tenant_id, id)
);
ALTER TABLE tenant_dsar_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_dsar_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_dsar_requests
    USING (tenant_id = current_setting('app.current_tenant', true))
    WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
ALTER TABLE tenant_dsar_phases ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_dsar_phases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_dsar_phases
    USING (tenant_id = current_setting('app.current_tenant', true))
    WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`

func (s *Store) ListAuditEvents(ctx context.Context, actor models.Principal, filter models.AuditEventFilter) (*models.AuditEventPage, error) {
	if !actor.Authorize(models.PermissionAuditRead, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		filter.Limit < 1 || filter.Limit > 500 || (!filter.OccurredTo.IsZero() && !filter.OccurredFrom.IsZero() && !filter.OccurredTo.After(filter.OccurredFrom)) {
		return nil, fmt.Errorf("list audit events: invalid authorization or page")
	}
	page := &models.AuditEventPage{}
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		query := `SELECT id, tenant_id, actor_user_id, membership_id, action,
			target_type, target_id, request_id, reason, details_json, result, trace_id, occurred_at
			FROM audit_events WHERE tenant_id = ? AND id > ?`
		args := []any{actor.TenantID, filter.AfterID}
		if filter.ActionPrefix != "" {
			query += ` AND action LIKE ?`
			args = append(args, filter.ActionPrefix+"%")
		}
		if filter.TargetType != "" {
			query += ` AND target_type = ?`
			args = append(args, filter.TargetType)
		}
		if !filter.OccurredFrom.IsZero() {
			query += ` AND occurred_at >= ?`
			args = append(args, filter.OccurredFrom.UTC())
		}
		if !filter.OccurredTo.IsZero() {
			query += ` AND occurred_at < ?`
			args = append(args, filter.OccurredTo.UTC())
		}
		query += ` ORDER BY id LIMIT ?`
		args = append(args, filter.Limit+1)
		rows, err := scoped.(*Store).query(txCtx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item models.AuditEvent
			if err := rows.Scan(&item.ID, &item.TenantID, &item.ActorUserID, &item.MembershipID,
				&item.Action, &item.TargetType, &item.TargetID, &item.RequestID, &item.Reason,
				&item.DetailsJSON, &item.Result, &item.TraceID, &item.OccurredAt); err != nil {
				return err
			}
			page.Items = append(page.Items, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(page.Items) > filter.Limit {
		page.Items = page.Items[:filter.Limit]
		page.NextAfterID = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}

func (s *Store) SetTenantRetentionPolicy(ctx context.Context, actor models.Principal, policy models.TenantRetentionPolicy) error {
	if !actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		!commercialKeyPattern.MatchString(policy.DataClass) || policy.RetentionDays < 1 || policy.RetentionDays > 3650 {
		return fmt.Errorf("set tenant retention policy: invalid authorization or input")
	}
	now := time.Now().UTC()
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		_, err := txs.exec(txCtx, `INSERT INTO tenant_retention_policies
			(tenant_id, data_class, retention_days, legal_hold, version, updated_at)
			VALUES (?, ?, ?, ?, 1, ?) ON CONFLICT (tenant_id, data_class) DO UPDATE SET
			retention_days = excluded.retention_days, legal_hold = excluded.legal_hold,
			version = tenant_retention_policies.version + 1, updated_at = excluded.updated_at`,
			actor.TenantID, policy.DataClass, policy.RetentionDays, policy.LegalHold, now)
		if err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "retention.policy.set", TargetType: "retention_policy", TargetID: policy.DataClass})
	})
}

func (s *Store) ListTenantRetentionPolicies(ctx context.Context, actor models.Principal) ([]models.TenantRetentionPolicy, error) {
	if !actor.Authorize(models.PermissionAuditRead, models.AuthorizationResource{TenantID: actor.TenantID}) &&
		!actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, fmt.Errorf("list tenant retention policies: unauthorized")
	}
	var policies []models.TenantRetentionPolicy
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		rows, err := scoped.(*Store).query(txCtx, `SELECT data_class, retention_days, legal_hold,
			version, updated_at FROM tenant_retention_policies WHERE tenant_id = ? ORDER BY data_class`, actor.TenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item := models.TenantRetentionPolicy{TenantID: actor.TenantID}
			if err := rows.Scan(&item.DataClass, &item.RetentionDays, &item.LegalHold, &item.Version, &item.UpdatedAt); err != nil {
				return err
			}
			policies = append(policies, item)
		}
		return rows.Err()
	})
	return policies, err
}

type tenantChecksumSpec struct{ table, keyExpression string }

var tenantChecksumSpecs = []tenantChecksumSpec{
	{"cohorts", "id"},
	{"enrollments", "id"},
	{"formation_versions", "id"},
	{"formations", "id"},
	{"integration_deliveries", "id"},
	{"learner_concept_states", "enrollment_id || ':' || formation_concept_id"},
	{"narrative_objects", "enrollment_id || ':' || scope || ':' || domain_id || ':' || object_key || ':' || checksum"},
	{"usage_events", "CAST(id AS TEXT) || ':' || event_key || ':' || CAST(quantity AS TEXT)"},
}

func (s *Store) ComputeTenantChecksums(ctx context.Context, actor models.ControlPlanePrincipal, tenantID string) (string, string, error) {
	if !actor.Validate() || tenantID == "" {
		return "", "", fmt.Errorf("compute tenant checksums: invalid authority")
	}
	tableChecksums := make(map[string]string, len(tenantChecksumSpecs))
	objectChecksums := make(map[string]string)
	err := s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		for _, spec := range tenantChecksumSpecs {
			rows, err := txs.query(ctx, `SELECT `+spec.keyExpression+` FROM `+spec.table+` WHERE tenant_id = ? ORDER BY 1`, tenantID)
			if err != nil {
				return fmt.Errorf("checksum %s: %w", spec.table, err)
			}
			h := sha256.New()
			count := int64(0)
			for rows.Next() {
				var key string
				if err := rows.Scan(&key); err != nil {
					rows.Close()
					return err
				}
				_, _ = h.Write([]byte(key))
				_, _ = h.Write([]byte{0})
				count++
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			tableChecksums[spec.table] = fmt.Sprintf("%d:%s", count, hex.EncodeToString(h.Sum(nil)))
		}
		rows, err := txs.query(ctx, `SELECT enrollment_id || ':' || scope || ':' || domain_id || ':' || object_key,
			checksum FROM narrative_objects WHERE tenant_id = ? ORDER BY 1`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key, checksum string
			if err := rows.Scan(&key, &checksum); err != nil {
				return err
			}
			objectChecksums[key] = checksum
		}
		return rows.Err()
	})
	if err != nil {
		return "", "", err
	}
	tablesJSON, _ := json.Marshal(tableChecksums)
	objectsJSON, _ := json.Marshal(objectChecksums)
	return string(tablesJSON), string(objectsJSON), nil
}

func (s *Store) RequestTenantRestoreVerification(ctx context.Context, actor models.ControlPlanePrincipal, tenantID, backupID, expectedTablesJSON, expectedObjectsJSON string) (*models.TenantRestoreManifest, error) {
	if !actor.Validate() || tenantID == "" || backupID == "" || !json.Valid([]byte(expectedTablesJSON)) || !json.Valid([]byte(expectedObjectsJSON)) {
		return nil, fmt.Errorf("request tenant restore verification: invalid input")
	}
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	manifest := &models.TenantRestoreManifest{ID: id, TenantID: tenantID, BackupID: backupID,
		Status: "requested", TableChecksumsJSON: expectedTablesJSON, ObjectChecksumsJSON: expectedObjectsJSON,
		RequestedBy: actor.ActorID, RequestedAt: now}
	err = s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		_, err := txs.exec(ctx, `INSERT INTO tenant_restore_manifests
			(id, tenant_id, backup_id, status, table_checksums_json, object_checksums_json,
			 requested_by, requested_at) VALUES (?, ?, ?, 'requested', ?, ?, ?, ?)`, id,
			tenantID, backupID, expectedTablesJSON, expectedObjectsJSON, actor.ActorID, now)
		if err != nil {
			return err
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "restore.verify.request", "restore_manifest", id)
	})
	return manifest, err
}

func (s *Store) VerifyTenantRestore(ctx context.Context, actor models.ControlPlanePrincipal, tenantID, manifestID string) (bool, error) {
	if !actor.Validate() || tenantID == "" || manifestID == "" {
		return false, fmt.Errorf("verify tenant restore: invalid input")
	}
	actualTables, actualObjects, err := s.ComputeTenantChecksums(ctx, actor, tenantID)
	if err != nil {
		return false, err
	}
	matched := false
	err = s.withTenantControlTx(ctx, tenantID, actor.ActorID, func(txs *Store) error {
		var expectedTables, expectedObjects, status string
		if err := txs.queryRow(ctx, `SELECT table_checksums_json, object_checksums_json, status
			FROM tenant_restore_manifests WHERE tenant_id = ? AND id = ?`, tenantID, manifestID).Scan(&expectedTables, &expectedObjects, &status); err != nil {
			return err
		}
		if status != "requested" {
			return fmt.Errorf("verify tenant restore: manifest is terminal")
		}
		matched = canonicalJSONEqual(expectedTables, actualTables) && canonicalJSONEqual(expectedObjects, actualObjects)
		newStatus := "failed"
		if matched {
			newStatus = "verified"
		}
		now := time.Now().UTC()
		_, err := txs.exec(ctx, `UPDATE tenant_restore_manifests SET status = ?, verified_at = ?
			WHERE tenant_id = ? AND id = ? AND status = 'requested'`, newStatus, now, tenantID, manifestID)
		if err != nil {
			return err
		}
		return txs.appendControlPlaneAudit(ctx, tenantID, actor, "restore.verify."+newStatus, "restore_manifest", manifestID)
	})
	return matched, err
}

func canonicalJSONEqual(left, right string) bool {
	var a, b any
	return json.Unmarshal([]byte(left), &a) == nil && json.Unmarshal([]byte(right), &b) == nil && reflect.DeepEqual(a, b)
}

func (s *Store) RequestTenantDSAR(ctx context.Context, actor models.Principal, learnerID, kind, reason string) (*models.TenantDSARRequest, error) {
	if (!actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) &&
		!(kind == "export" && actor.LearnerID == learnerID && actor.Authorize(models.PermissionLearningSelf, models.AuthorizationResource{TenantID: actor.TenantID, OwnerUserID: actor.UserID}))) ||
		learnerID == "" || (kind != "export" && kind != "erase" && kind != "rectify") || strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("request tenant DSAR: invalid authorization or input")
	}
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	request := &models.TenantDSARRequest{ID: id, TenantID: actor.TenantID, LearnerID: learnerID,
		Kind: kind, Status: "pending", RequestedBy: actor.UserID, Reason: reason, ResultJSON: "{}", CreatedAt: now}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var exists int
		if err := txs.queryRow(txCtx, `SELECT COUNT(*) FROM learners WHERE tenant_id = ? AND id = ?`, actor.TenantID, learnerID).Scan(&exists); err != nil || exists != 1 {
			return fmt.Errorf("request tenant DSAR: learner not found")
		}
		_, err := txs.exec(txCtx, `INSERT INTO tenant_dsar_requests
			(id, tenant_id, learner_id, kind, status, requested_by, reason, result_json, created_at)
			VALUES (?, ?, ?, ?, 'pending', ?, ?, '{}', ?)`, id, actor.TenantID, learnerID, kind, actor.UserID, reason, now)
		if err != nil {
			return err
		}
		phases := []string{"export_manifest"}
		if kind == "erase" {
			phases = append([]string(nil), dsarErasurePhases...)
		} else if kind == "rectify" {
			phases = []string{"manual_rectification"}
		}
		for position, phase := range phases {
			if _, err := txs.exec(txCtx, `INSERT INTO tenant_dsar_phases
				(tenant_id, request_id, position, phase, status, affected_rows, updated_at)
				VALUES (?, ?, ?, ?, 'pending', 0, ?)`, actor.TenantID, id, position, phase, now); err != nil {
				return err
			}
		}
		if kind == "export" || kind == "erase" {
			payload, _ := json.Marshal(models.TenantDSARJob{RequestID: id, Kind: kind})
			if err := s.EnqueueAsyncJob(txCtx, actor.TenantScope(), models.AsyncJob{
				TenantID: actor.TenantID, Kind: "tenant_dsar", IdempotencyKey: id,
				PayloadJSON: string(payload), MaxAttempts: 32, CreatedAt: now, AvailableAt: now,
			}); err != nil {
				return err
			}
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "dsar.request." + kind, TargetType: "learner", TargetID: learnerID})
	})
	return request, err
}

func (s *Store) CompleteTenantDSARExport(ctx context.Context, scope models.TenantScope, worker models.WorkerPrincipal, requestID string) error {
	if err := scope.Validate(); err != nil || !worker.Validate() || requestID == "" {
		return fmt.Errorf("complete tenant DSAR export: invalid input")
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		query := `SELECT learner_id FROM tenant_dsar_requests WHERE tenant_id = ? AND id = ? AND kind = 'export' AND status = 'pending'`
		if txs.dialect == DialectPostgres {
			query += ` FOR UPDATE`
		}
		var learnerID string
		if err := txs.queryRow(txCtx, query, scope.TenantID, requestID).Scan(&learnerID); err != nil {
			return err
		}
		counts := make(map[string]int64)
		for _, table := range []string{"interactions", "concept_states", "narrative_objects", "pedagogical_decisions", "audit_events"} {
			var count int64
			predicate := "learner_id = ?"
			args := []any{learnerID}
			if table == "audit_events" {
				predicate = "tenant_id = ? AND target_id = ?"
				args = []any{scope.TenantID, learnerID}
			}
			if err := txs.queryRow(txCtx, `SELECT COUNT(*) FROM `+table+` WHERE `+predicate, args...).Scan(&count); err != nil {
				return err
			}
			counts[table] = count
		}
		payload, _ := json.Marshal(map[string]any{"learner_id": learnerID, "counts": counts, "generated_at": time.Now().UTC()})
		now := time.Now().UTC()
		result, err := txs.exec(txCtx, `UPDATE tenant_dsar_requests SET status = 'completed', result_json = ?, completed_at = ?
			WHERE tenant_id = ? AND id = ? AND status = 'pending'`, string(payload), now, scope.TenantID, requestID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return errors.New("complete tenant DSAR export: concurrent transition")
		}
		_, err = txs.exec(txCtx, `UPDATE tenant_dsar_phases SET status = 'completed', affected_rows = 1, updated_at = ?
			WHERE tenant_id = ? AND request_id = ? AND phase = 'export_manifest'`, now, scope.TenantID, requestID)
		if err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) ResumeTenantDSAR(ctx context.Context, actor models.Principal, requestID string) error {
	if !actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) || requestID == "" {
		return fmt.Errorf("resume tenant DSAR: invalid authorization or input")
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		result, err := txs.exec(txCtx, `UPDATE tenant_dsar_requests SET status = 'pending'
			WHERE tenant_id = ? AND id = ? AND status IN ('blocked','failed')`, actor.TenantID, requestID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("resume tenant DSAR: blocked request not found")
		}
		if _, err := txs.exec(txCtx, `UPDATE tenant_dsar_phases SET status = 'pending', updated_at = ?
			WHERE tenant_id = ? AND request_id = ? AND status IN ('blocked','failed')`, time.Now().UTC(), actor.TenantID, requestID); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "dsar.resume", TargetType: "dsar_request", TargetID: requestID})
	})
}

// Erasure is intentionally phased and bounded. Append-only audit evidence and
// the minimum relational identity skeleton are retained, while pedagogical
// evidence, narrative content and direct learner profile data are removed.
var dsarErasurePhases = []string{
	"webhook_delivery_transitions", "webhook_push_log", "webhook_message_queue",
	"narrative_mutations", "narrative_objects", "pedagogical_snapshots",
	"transfer_records", "interactions", "assessment_attempts", "pedagogical_decisions", "affect_states",
	"implementation_intentions", "learning_sessions", "concept_states",
	"scheduled_alerts", "availability", "scrub_learner",
}

var dsarLearnerTables = map[string]string{
	"webhook_delivery_transitions": "learner_id", "webhook_push_log": "learner_id",
	"webhook_message_queue": "learner_id", "narrative_mutations": "learner_id",
	"narrative_objects": "learner_id", "pedagogical_snapshots": "learner_id",
	"transfer_records": "learner_id", "interactions": "learner_id",
	"assessment_attempts": "learner_id", "pedagogical_decisions": "learner_id", "affect_states": "learner_id",
	"implementation_intentions": "learner_id", "learning_sessions": "learner_id",
	"concept_states": "learner_id", "scheduled_alerts": "learner_id",
	"availability": "learner_id",
}

func (s *Store) ProcessTenantDSARErasureBatch(ctx context.Context, scope models.TenantScope, worker models.WorkerPrincipal, requestID string, limit int, now time.Time) (bool, int64, error) {
	if err := scope.Validate(); err != nil || !worker.Validate() || requestID == "" || limit < 1 || limit > 1000 {
		return false, 0, fmt.Errorf("process tenant DSAR erasure: invalid input")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	completed := false
	blockedByHold := false
	affected := int64(0)
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var learnerID, requestStatus string
		requestQuery := `SELECT learner_id, status FROM tenant_dsar_requests
			WHERE tenant_id = ? AND id = ? AND kind = 'erase'`
		if txs.dialect == DialectPostgres {
			requestQuery += ` FOR UPDATE`
		}
		if err := txs.queryRow(txCtx, requestQuery, scope.TenantID, requestID).Scan(&learnerID, &requestStatus); err != nil {
			return err
		}
		if requestStatus == "completed" {
			completed = true
			return nil
		}
		if requestStatus != "pending" && requestStatus != "processing" {
			return fmt.Errorf("process tenant DSAR erasure: request is not runnable")
		}
		// Requests created before the decision journal existed must also erase
		// its rows. Append a checkpoint without rewriting existing positions;
		// execution follows the current dependency order, not insertion order.
		if _, err := txs.exec(txCtx, `INSERT INTO tenant_dsar_phases
			(tenant_id, request_id, position, phase, status, affected_rows, updated_at)
			SELECT ?, ?, COALESCE(MAX(position), -1) + 1, 'pedagogical_decisions', 'pending', 0, ?
			FROM tenant_dsar_phases WHERE tenant_id = ? AND request_id = ?
			HAVING NOT EXISTS (SELECT 1 FROM tenant_dsar_phases
			 WHERE tenant_id = ? AND request_id = ? AND phase = 'pedagogical_decisions')`,
			scope.TenantID, requestID, now, scope.TenantID, requestID, scope.TenantID, requestID); err != nil {
			return err
		}
		var holds int
		if err := txs.queryRow(txCtx, `SELECT COUNT(*) FROM retention_legal_holds
			WHERE learner_id = ? AND released_at IS NULL`, learnerID).Scan(&holds); err != nil {
			return err
		}
		if holds != 0 {
			_, _ = txs.exec(txCtx, `UPDATE tenant_dsar_requests SET status = 'blocked' WHERE tenant_id = ? AND id = ?`, scope.TenantID, requestID)
			_, _ = txs.exec(txCtx, `UPDATE tenant_dsar_phases SET status = 'blocked', updated_at = ?
				WHERE tenant_id = ? AND request_id = ? AND status IN ('pending','running')`, now, scope.TenantID, requestID)
			blockedByHold = true
			return nil
		}
		var position int
		var phase string
		var priorAffected int64
		phaseOrder := "CASE phase"
		for index, name := range dsarErasurePhases {
			phaseOrder += fmt.Sprintf(" WHEN '%s' THEN %d", name, index)
		}
		phaseOrder += " ELSE -1 END"
		err := txs.queryRow(txCtx, `SELECT position, phase, affected_rows FROM tenant_dsar_phases
			WHERE tenant_id = ? AND request_id = ? AND status IN ('pending','running')
			ORDER BY `+phaseOrder+`, position LIMIT 1`, scope.TenantID, requestID).Scan(&position, &phase, &priorAffected)
		if errors.Is(err, sql.ErrNoRows) {
			completed = true
			_, err = txs.exec(txCtx, `UPDATE tenant_dsar_requests SET status = 'completed', result_json = ?, completed_at = ?
				WHERE tenant_id = ? AND id = ?`, `{"erased":true}`, now, scope.TenantID, requestID)
			return err
		}
		if err != nil {
			return err
		}
		_, err = txs.exec(txCtx, `UPDATE tenant_dsar_requests SET status = 'processing' WHERE tenant_id = ? AND id = ? AND status = 'pending'`, scope.TenantID, requestID)
		if err != nil {
			return err
		}
		phaseComplete := false
		if phase == "scrub_learner" {
			anon := "erased+" + opaqueTokenHash(scope.TenantID + ":" + learnerID)[:24] + "@invalid.local"
			result, err := txs.exec(txCtx, `UPDATE learners SET email = ?, password_hash = 'erased', objective = '',
				profile_json = '{}', webhook_url = '', email_verified_at = NULL WHERE tenant_id = ? AND id = ?`,
				anon, scope.TenantID, learnerID)
			if err != nil {
				return err
			}
			affected, _ = result.RowsAffected()
			phaseComplete = true
		} else {
			column, ok := dsarLearnerTables[phase]
			if !ok {
				return fmt.Errorf("process tenant DSAR erasure: unknown phase")
			}
			selector := "rowid"
			if txs.dialect == DialectPostgres {
				selector = "ctid"
			}
			result, err := txs.exec(txCtx, `DELETE FROM `+phase+` WHERE `+selector+` IN
				(SELECT `+selector+` FROM `+phase+` WHERE tenant_id = ? AND `+column+` = ? LIMIT ?)`,
				scope.TenantID, learnerID, limit)
			if err != nil {
				return err
			}
			affected, _ = result.RowsAffected()
			phaseComplete = affected < int64(limit)
		}
		phaseStatus := "running"
		if phaseComplete {
			phaseStatus = "completed"
		}
		_, err = txs.exec(txCtx, `UPDATE tenant_dsar_phases SET status = ?, affected_rows = ?, updated_at = ?
			WHERE tenant_id = ? AND request_id = ? AND position = ?`, phaseStatus, priorAffected+affected,
			now, scope.TenantID, requestID, position)
		return err
	})
	if err == nil && blockedByHold {
		err = fmt.Errorf("process tenant DSAR erasure: active legal hold")
	}
	return completed, affected, err
}
