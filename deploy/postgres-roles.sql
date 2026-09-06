\set ON_ERROR_STOP on

-- Run as the database owner after creating login roles in the secret manager.
-- Login roles should be granted exactly one of these group roles.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tutor_api') THEN
        CREATE ROLE tutor_api NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tutor_worker') THEN
        CREATE ROLE tutor_worker NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tutor_restore') THEN
        CREATE ROLE tutor_restore NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
    END IF;
END $$;

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM tutor_api, tutor_worker, tutor_restore;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM tutor_api, tutor_worker, tutor_restore;
GRANT USAGE ON SCHEMA public TO tutor_api, tutor_worker, tutor_restore;

GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO tutor_api;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO tutor_api;

-- Workers use the same RLS policies and SET LOCAL wrapper as API requests.
-- They receive no BYPASSRLS and enumerate only the global tenants root before
-- opening one scoped transaction per tenant.
GRANT SELECT ON tenants, plans, schema_migrations TO tutor_worker;

-- Read models used by the pedagogical scheduler. learners contains the
-- encrypted webhook credential required at the final dispatch boundary; the
-- worker cannot read global users, password/MFA/service/support credentials.
GRANT SELECT ON
    learners, availability, domains, concept_states, interactions,
    affect_states, calibration_records, transfer_records, assessment_attempts,
    pedagogical_decisions,
    narrative_objects, audit_events, retention_legal_holds,
    tenant_integrations, tenant_integration_secret_versions,
    tenant_entitlements, entitlement_reservations, usage_events,
    usage_corrections, usage_rollups, tenant_dsar_requests, tenant_dsar_phases
TO tutor_worker;

-- Durable queues and operational state owned by the worker.
GRANT SELECT, INSERT, UPDATE, DELETE ON
    outbox_events, async_jobs, integration_deliveries,
    webhook_message_queue, webhook_delivery_transitions, webhook_push_log,
    scheduled_alerts, pending_consolidations, scheduled_job_runs,
    retention_jobs, retention_job_phases, worker_tenant_runs
TO tutor_worker;
GRANT SELECT, UPDATE ON tenant_entitlements, entitlement_reservations TO tutor_worker;
GRANT SELECT, UPDATE ON tenant_dsar_requests, tenant_dsar_phases TO tutor_worker;
-- Resume pre-upgrade erasure requests by appending newly introduced checkpoints.
GRANT INSERT ON tenant_dsar_phases TO tutor_worker;
GRANT UPDATE ON learners TO tutor_worker;
GRANT DELETE ON
    webhook_delivery_transitions, webhook_push_log, webhook_message_queue,
    narrative_mutations, narrative_objects, pedagogical_snapshots,
    transfer_records, interactions, assessment_attempts, pedagogical_decisions, affect_states,
    implementation_intentions, learning_sessions, concept_states,
    scheduled_alerts, availability
TO tutor_worker;

-- The maintenance job only deletes expired rows from these stores. It cannot
-- create or rotate credentials and does not need to select their payloads.
GRANT DELETE ON
    account_tokens, oauth_codes, oauth_clients, refresh_tokens,
    rate_limit_buckets, login_failure_windows, login_failures
TO tutor_worker;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO tutor_worker;

-- The restore role is activated only for an approved isolated logical restore.
-- RLS remains active and the command sets exactly one app.current_tenant. The
-- role can insert/verify archived rows but cannot mutate or delete existing
-- rows, change schema, own tables or bypass RLS. Trigger suppression is needed
-- only to load the FK graph without relying on table order; the application
-- restores origin mode and validates every tenant FK before commit.
GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA public TO tutor_restore;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO tutor_restore;
GRANT SET ON PARAMETER session_replication_role TO tutor_restore;

ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO tutor_api;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT ON TABLES TO tutor_restore;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO tutor_api, tutor_worker;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO tutor_restore;

-- Deliberately absent: CREATE on schema, ALTER/DROP, role management,
-- ownership, SUPERUSER and BYPASSRLS. The migrator uses a separate owner DSN.
