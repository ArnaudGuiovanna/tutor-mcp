// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"

	"go.opentelemetry.io/otel/trace"
)

const sqlitePlatformAuditMigration = `CREATE TABLE platform_audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id TEXT NOT NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    details_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(details_json)),
    result TEXT NOT NULL DEFAULT 'success',
    trace_id TEXT NOT NULL DEFAULT '',
    occurred_at DATETIME NOT NULL
);
CREATE INDEX idx_platform_audit_events_time ON platform_audit_events(occurred_at, id);
CREATE TRIGGER platform_audit_events_no_update BEFORE UPDATE ON platform_audit_events
BEGIN SELECT RAISE(ABORT, 'platform audit events are append-only'); END;
CREATE TRIGGER platform_audit_events_no_delete BEFORE DELETE ON platform_audit_events
BEGIN SELECT RAISE(ABORT, 'platform audit events are append-only'); END;`

const postgresPlatformAuditMigration = `CREATE TABLE platform_audit_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id TEXT NOT NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    details_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    result TEXT NOT NULL DEFAULT 'success',
    trace_id TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_platform_audit_events_time ON platform_audit_events(occurred_at, id);
CREATE TRIGGER platform_audit_events_append_only BEFORE UPDATE OR DELETE ON platform_audit_events
FOR EACH ROW EXECUTE FUNCTION tutor_append_only();`

func (s *Store) appendPlatformAudit(ctx context.Context, actor models.ControlPlanePrincipal, action, targetType, targetID, detailsJSON string) error {
	if !actor.Validate() || strings.TrimSpace(action) == "" || strings.TrimSpace(targetType) == "" || strings.TrimSpace(targetID) == "" {
		return fmt.Errorf("append platform audit: invalid authority or event")
	}
	if detailsJSON == "" {
		detailsJSON = "{}"
	}
	if !json.Valid([]byte(detailsJSON)) {
		return fmt.Errorf("append platform audit: invalid details JSON")
	}
	traceID := ""
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		traceID = span.TraceID().String()
	}
	_, err := s.exec(ctx, `INSERT INTO platform_audit_events
		(actor_id, action, target_type, target_id, request_id, reason,
		 details_json, result, trace_id, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'success', ?, ?)`, actor.ActorID, action,
		targetType, targetID, actor.RequestID, actor.Reason, detailsJSON, traceID, time.Now().UTC())
	return err
}
