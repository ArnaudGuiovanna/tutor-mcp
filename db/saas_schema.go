// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

const sqliteSaaSControlPlaneMigration = `CREATE TABLE plans (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active','retired')),
    entitlements_json TEXT NOT NULL CHECK (json_valid(entitlements_json)),
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
INSERT INTO plans (id, name, status, entitlements_json, created_at, updated_at)
VALUES ('plan_legacy', 'Legacy', 'active',
        '{"active_learners":100000,"enrollments":100000,"published_formations":10000,"cohorts":10000,"mcp_calls_month":10000000,"storage_bytes":107374182400,"notifications_month":1000000,"exports_month":10000}',
        CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
CREATE TABLE tenant_subscriptions (
    tenant_id TEXT PRIMARY KEY REFERENCES tenants(id),
    plan_id TEXT NOT NULL REFERENCES plans(id),
    status TEXT NOT NULL CHECK (status IN ('trialing','active','past_due','grace','suspended','cancelled')),
    provider TEXT NOT NULL DEFAULT '',
    provider_customer_id TEXT NOT NULL DEFAULT '',
    provider_subscription_id TEXT NOT NULL DEFAULT '',
    current_period_start DATETIME NOT NULL,
    current_period_end DATETIME NOT NULL,
    grace_until DATETIME,
    version INTEGER NOT NULL DEFAULT 1,
    updated_at DATETIME NOT NULL
);
INSERT INTO tenant_subscriptions
    (tenant_id, plan_id, status, current_period_start, current_period_end, updated_at)
SELECT id, 'plan_legacy', 'active', CURRENT_TIMESTAMP,
       datetime(CURRENT_TIMESTAMP, '+100 years'), CURRENT_TIMESTAMP FROM tenants;
CREATE TABLE tenant_entitlements (
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    entitlement_key TEXT NOT NULL,
    hard_limit INTEGER NOT NULL CHECK (hard_limit >= 0),
    used_value INTEGER NOT NULL DEFAULT 0 CHECK (used_value >= 0),
    reserved_value INTEGER NOT NULL DEFAULT 0 CHECK (reserved_value >= 0),
    period_start DATETIME NOT NULL,
    period_end DATETIME NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, entitlement_key),
    CHECK (used_value + reserved_value <= hard_limit)
);
CREATE TABLE usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    event_key TEXT NOT NULL,
    metric TEXT NOT NULL,
    quantity INTEGER NOT NULL CHECK (quantity >= 0),
    source_type TEXT NOT NULL,
    source_id TEXT NOT NULL,
    correction_of TEXT NOT NULL DEFAULT '',
    dimensions_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(dimensions_json)),
    occurred_at DATETIME NOT NULL,
    recorded_at DATETIME NOT NULL,
    UNIQUE (tenant_id, event_key)
);
CREATE INDEX idx_usage_events_tenant_metric_time ON usage_events(tenant_id, metric, occurred_at);
CREATE TRIGGER usage_events_no_update BEFORE UPDATE ON usage_events
BEGIN SELECT RAISE(ABORT, 'usage events are append-only'); END;
CREATE TRIGGER usage_events_no_delete BEFORE DELETE ON usage_events
BEGIN SELECT RAISE(ABORT, 'usage events are append-only'); END;
CREATE TABLE usage_rollups (
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    metric TEXT NOT NULL,
    period_start DATETIME NOT NULL,
    period_end DATETIME NOT NULL,
    quantity INTEGER NOT NULL CHECK (quantity >= 0),
    source_max_id INTEGER NOT NULL,
    reconciled_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, metric, period_start, period_end)
);
CREATE TABLE billing_provider_events (
    provider TEXT NOT NULL,
    event_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    event_type TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    occurred_at DATETIME NOT NULL,
    processed_at DATETIME NOT NULL,
    PRIMARY KEY (provider, event_id)
);
CREATE TRIGGER billing_provider_events_no_update BEFORE UPDATE ON billing_provider_events
BEGIN SELECT RAISE(ABORT, 'billing provider events are append-only'); END;
CREATE TRIGGER billing_provider_events_no_delete BEFORE DELETE ON billing_provider_events
BEGIN SELECT RAISE(ABORT, 'billing provider events are append-only'); END;
CREATE TABLE outbox_events (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    aggregate_type TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    status TEXT NOT NULL CHECK (status IN ('pending','processing','delivered','dead_letter')) DEFAULT 'pending',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    available_at DATETIME NOT NULL,
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_expires_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    delivered_at DATETIME,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX idx_outbox_claim ON outbox_events(status, available_at, lease_expires_at, created_at);
CREATE TABLE async_jobs (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    kind TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    status TEXT NOT NULL CHECK (status IN ('pending','processing','completed','dead_letter')) DEFAULT 'pending',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 8 CHECK (max_attempts > 0),
    available_at DATETIME NOT NULL,
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_expires_at DATETIME,
    heartbeat_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    completed_at DATETIME,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, kind, idempotency_key)
);
CREATE INDEX idx_async_jobs_claim ON async_jobs(status, available_at, lease_expires_at, created_at);
CREATE TABLE tenant_integrations (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    kind TEXT NOT NULL,
    endpoint_url TEXT NOT NULL,
    event_types_json TEXT NOT NULL CHECK (json_valid(event_types_json)),
    secret_ciphertext TEXT NOT NULL,
    key_id TEXT NOT NULL,
    secret_version INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL CHECK (status IN ('active','suspended','revoked')),
    created_by TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE TABLE integration_deliveries (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    integration_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','delivered','failed','dead_letter')),
    response_code INTEGER,
    response_hash TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    delivered_at DATETIME,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, integration_id, event_id, attempt),
    FOREIGN KEY (tenant_id, integration_id) REFERENCES tenant_integrations(tenant_id, id)
);
CREATE TABLE tenant_domains (
    hostname TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    status TEXT NOT NULL CHECK (status IN ('pending','verified','revoked')),
    verification_hash TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    verified_at DATETIME
);
CREATE TABLE tenant_feature_flags (
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    flag_key TEXT NOT NULL,
    enabled INTEGER NOT NULL CHECK (enabled IN (0,1)),
    version INTEGER NOT NULL DEFAULT 1,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, flag_key)
);
CREATE TABLE tenant_retention_policies (
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    data_class TEXT NOT NULL,
    retention_days INTEGER NOT NULL CHECK (retention_days > 0),
    legal_hold INTEGER NOT NULL DEFAULT 0 CHECK (legal_hold IN (0,1)),
    version INTEGER NOT NULL DEFAULT 1,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, data_class)
);
CREATE TABLE tenant_restore_manifests (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    backup_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('requested','verified','failed')),
    table_checksums_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(table_checksums_json)),
    object_checksums_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(object_checksums_json)),
    requested_by TEXT NOT NULL,
    requested_at DATETIME NOT NULL,
    verified_at DATETIME,
    PRIMARY KEY (tenant_id, id)
);`

const postgresSaaSControlPlaneMigration = `CREATE TABLE plans (
    id TEXT PRIMARY KEY, name TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('active','retired')),
    entitlements_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL
);
INSERT INTO plans (id, name, status, entitlements_json, created_at, updated_at)
VALUES ('plan_legacy', 'Legacy', 'active',
        '{"active_learners":100000,"enrollments":100000,"published_formations":10000,"cohorts":10000,"mcp_calls_month":10000000,"storage_bytes":107374182400,"notifications_month":1000000,"exports_month":10000}'::jsonb,
        CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
CREATE TABLE tenant_subscriptions (
    tenant_id TEXT PRIMARY KEY REFERENCES tenants(id), plan_id TEXT NOT NULL REFERENCES plans(id),
    status TEXT NOT NULL CHECK (status IN ('trialing','active','past_due','grace','suspended','cancelled')),
    provider TEXT NOT NULL DEFAULT '', provider_customer_id TEXT NOT NULL DEFAULT '',
    provider_subscription_id TEXT NOT NULL DEFAULT '', current_period_start TIMESTAMPTZ NOT NULL,
    current_period_end TIMESTAMPTZ NOT NULL, grace_until TIMESTAMPTZ,
    version BIGINT NOT NULL DEFAULT 1, updated_at TIMESTAMPTZ NOT NULL
);
INSERT INTO tenant_subscriptions
    (tenant_id, plan_id, status, current_period_start, current_period_end, updated_at)
SELECT id, 'plan_legacy', 'active', CURRENT_TIMESTAMP,
       CURRENT_TIMESTAMP + interval '100 years', CURRENT_TIMESTAMP FROM tenants;
CREATE TABLE tenant_entitlements (
    tenant_id TEXT NOT NULL REFERENCES tenants(id), entitlement_key TEXT NOT NULL,
    hard_limit BIGINT NOT NULL CHECK (hard_limit >= 0), used_value BIGINT NOT NULL DEFAULT 0 CHECK (used_value >= 0),
    reserved_value BIGINT NOT NULL DEFAULT 0 CHECK (reserved_value >= 0),
    period_start TIMESTAMPTZ NOT NULL, period_end TIMESTAMPTZ NOT NULL,
    version BIGINT NOT NULL DEFAULT 1, updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, entitlement_key), CHECK (used_value + reserved_value <= hard_limit)
);
CREATE TABLE usage_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id),
    event_key TEXT NOT NULL, metric TEXT NOT NULL, quantity BIGINT NOT NULL CHECK (quantity >= 0),
    source_type TEXT NOT NULL, source_id TEXT NOT NULL, correction_of TEXT NOT NULL DEFAULT '',
    dimensions_json JSONB NOT NULL DEFAULT '{}'::jsonb, occurred_at TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL, UNIQUE (tenant_id, event_key)
);
CREATE INDEX idx_usage_events_tenant_metric_time ON usage_events(tenant_id, metric, occurred_at);
CREATE TABLE usage_rollups (
    tenant_id TEXT NOT NULL REFERENCES tenants(id), metric TEXT NOT NULL,
    period_start TIMESTAMPTZ NOT NULL, period_end TIMESTAMPTZ NOT NULL,
    quantity BIGINT NOT NULL CHECK (quantity >= 0), source_max_id BIGINT NOT NULL,
    reconciled_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (tenant_id, metric, period_start, period_end)
);
CREATE TABLE billing_provider_events (
    provider TEXT NOT NULL, event_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL REFERENCES tenants(id), event_type TEXT NOT NULL,
    payload_hash TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (provider, event_id)
);
CREATE TABLE outbox_events (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), aggregate_type TEXT NOT NULL,
    aggregate_id TEXT NOT NULL, event_type TEXT NOT NULL, idempotency_key TEXT NOT NULL,
    payload_json JSONB NOT NULL, status TEXT NOT NULL CHECK (status IN ('pending','processing','delivered','dead_letter')) DEFAULT 'pending',
    attempt_count INTEGER NOT NULL DEFAULT 0, available_at TIMESTAMPTZ NOT NULL,
    lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at TIMESTAMPTZ, last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL, delivered_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id), UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX idx_outbox_claim ON outbox_events(status, available_at, lease_expires_at, created_at);
CREATE TABLE async_jobs (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), kind TEXT NOT NULL,
    idempotency_key TEXT NOT NULL, payload_json JSONB NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','processing','completed','dead_letter')) DEFAULT 'pending',
    attempt_count INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 8 CHECK (max_attempts > 0),
    available_at TIMESTAMPTZ NOT NULL, lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ, last_error TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ, PRIMARY KEY (tenant_id, id), UNIQUE (tenant_id, kind, idempotency_key)
);
CREATE INDEX idx_async_jobs_claim ON async_jobs(status, available_at, lease_expires_at, created_at);
CREATE TABLE tenant_integrations (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), kind TEXT NOT NULL,
    endpoint_url TEXT NOT NULL, event_types_json JSONB NOT NULL, secret_ciphertext TEXT NOT NULL,
    key_id TEXT NOT NULL, secret_version BIGINT NOT NULL DEFAULT 1,
    status TEXT NOT NULL CHECK (status IN ('active','suspended','revoked')),
    created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE TABLE integration_deliveries (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL, integration_id TEXT NOT NULL, event_id TEXT NOT NULL,
    attempt INTEGER NOT NULL, status TEXT NOT NULL CHECK (status IN ('pending','delivered','failed','dead_letter')),
    response_code INTEGER, response_hash TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL, delivered_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id), UNIQUE (tenant_id, integration_id, event_id, attempt),
    FOREIGN KEY (tenant_id, integration_id) REFERENCES tenant_integrations(tenant_id, id)
);
CREATE TABLE tenant_domains (
    hostname TEXT PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id),
    status TEXT NOT NULL CHECK (status IN ('pending','verified','revoked')),
    verification_hash TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, verified_at TIMESTAMPTZ
);
CREATE TABLE tenant_feature_flags (
    tenant_id TEXT NOT NULL REFERENCES tenants(id), flag_key TEXT NOT NULL, enabled BOOLEAN NOT NULL,
    version BIGINT NOT NULL DEFAULT 1, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (tenant_id, flag_key)
);
CREATE TABLE tenant_retention_policies (
    tenant_id TEXT NOT NULL REFERENCES tenants(id), data_class TEXT NOT NULL,
    retention_days INTEGER NOT NULL CHECK (retention_days > 0), legal_hold BOOLEAN NOT NULL DEFAULT false,
    version BIGINT NOT NULL DEFAULT 1, updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, data_class)
);
CREATE TABLE tenant_restore_manifests (
    id TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id), backup_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('requested','verified','failed')),
    table_checksums_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    object_checksums_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    requested_by TEXT NOT NULL, requested_at TIMESTAMPTZ NOT NULL, verified_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
);
CREATE OR REPLACE FUNCTION tutor_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION '% is append-only', TG_TABLE_NAME; END $$;
CREATE TRIGGER usage_events_append_only BEFORE UPDATE OR DELETE ON usage_events
FOR EACH ROW EXECUTE FUNCTION tutor_append_only();
CREATE TRIGGER billing_provider_events_append_only BEFORE UPDATE OR DELETE ON billing_provider_events
FOR EACH ROW EXECUTE FUNCTION tutor_append_only();
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'tenant_subscriptions','tenant_entitlements','usage_events','usage_rollups','billing_provider_events',
        'outbox_events','async_jobs','tenant_integrations','integration_deliveries',
        'tenant_feature_flags','tenant_retention_policies','tenant_restore_manifests'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %1$I USING (tenant_id = current_setting(''app.current_tenant'', true)) WITH CHECK (tenant_id = current_setting(''app.current_tenant'', true))', table_name);
    END LOOP;
END $$;`

const sqliteServiceAccountCredentialMigration = `ALTER TABLE service_accounts ADD COLUMN token_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE service_accounts ADD COLUMN scopes_json TEXT NOT NULL DEFAULT '["learner:read"]' CHECK (json_valid(scopes_json));
ALTER TABLE service_accounts ADD COLUMN expires_at DATETIME;
ALTER TABLE service_accounts ADD COLUMN last_used_at DATETIME;
CREATE UNIQUE INDEX idx_service_accounts_token_hash ON service_accounts(token_hash) WHERE token_hash <> '';
CREATE TABLE service_account_routes (
    token_hash TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    service_account_id TEXT NOT NULL,
    expires_at DATETIME
);`

const postgresServiceAccountCredentialMigration = `ALTER TABLE service_accounts ADD COLUMN IF NOT EXISTS token_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE service_accounts ADD COLUMN IF NOT EXISTS scopes_json JSONB NOT NULL DEFAULT '["learner:read"]'::jsonb;
ALTER TABLE service_accounts ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE service_accounts ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_accounts_token_hash ON service_accounts(token_hash) WHERE token_hash <> '';
CREATE TABLE service_account_routes (
    token_hash TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    service_account_id TEXT NOT NULL,
    expires_at TIMESTAMPTZ
);`
