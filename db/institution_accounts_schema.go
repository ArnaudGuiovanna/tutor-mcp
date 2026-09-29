// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

// Institution accounts add the browser console, a two-step TOTP ceremony with
// single-use recovery codes, and self-service institution signup.
//
// console_sessions, pending_signups and user_password_resets are global
// lookup tables, like
// credential_tenant_routes: a browser presents only an opaque token, so the
// row must be found before a tenant is known. They hold hashes and
// identifiers, never tenant business data. MFA material stays user-global
// like mfa_credentials.
//
// Trainers and pedagogy managers read learner progress and assessment
// material, so they now require MFA like owners and admins.
const sqliteInstitutionAccountsMigration = `
ALTER TABLE mfa_credentials ADD COLUMN confirmed_at DATETIME;
UPDATE mfa_credentials SET confirmed_at = created_at WHERE confirmed_at IS NULL;
CREATE TABLE mfa_recovery_codes (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL UNIQUE,
    created_at DATETIME NOT NULL,
    used_at    DATETIME
);
CREATE INDEX idx_mfa_recovery_codes_user ON mfa_recovery_codes(user_id, used_at);
CREATE TABLE console_sessions (
    token_hash         TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant_id          TEXT NOT NULL REFERENCES tenants(id),
    membership_id      TEXT NOT NULL,
    membership_version INTEGER NOT NULL CHECK (membership_version >= 1),
    mfa_verified_at    DATETIME,
    created_at         DATETIME NOT NULL,
    last_seen_at       DATETIME NOT NULL,
    expires_at         DATETIME NOT NULL
);
CREATE INDEX idx_console_sessions_expiry ON console_sessions(expires_at);
CREATE INDEX idx_console_sessions_membership ON console_sessions(tenant_id, membership_id);
CREATE TABLE pending_signups (
    token_hash       TEXT PRIMARY KEY,
    email            TEXT NOT NULL,
    normalized_email TEXT NOT NULL,
    tenant_name      TEXT NOT NULL,
    tenant_slug      TEXT NOT NULL,
    created_at       DATETIME NOT NULL,
    expires_at       DATETIME NOT NULL,
    consumed_at      DATETIME
);
CREATE INDEX idx_pending_signups_expiry ON pending_signups(expires_at);
CREATE TABLE user_password_resets (
    token_hash  TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  DATETIME NOT NULL,
    expires_at  DATETIME NOT NULL,
    consumed_at DATETIME
);
CREATE INDEX idx_user_password_resets_expiry ON user_password_resets(expires_at);
UPDATE tenant_memberships SET mfa_required = 1
WHERE EXISTS (SELECT 1 FROM json_each(roles_json) WHERE value IN ('pedagogy_manager','trainer'));
`

const postgresInstitutionAccountsMigration = `
ALTER TABLE mfa_credentials ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ;
UPDATE mfa_credentials SET confirmed_at = created_at WHERE confirmed_at IS NULL;
CREATE TABLE IF NOT EXISTS mfa_recovery_codes (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mfa_recovery_codes_user ON mfa_recovery_codes(user_id, used_at);
CREATE TABLE IF NOT EXISTS console_sessions (
    token_hash         TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant_id          TEXT NOT NULL REFERENCES tenants(id),
    membership_id      TEXT NOT NULL,
    membership_version BIGINT NOT NULL CHECK (membership_version >= 1),
    mfa_verified_at    TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL,
    last_seen_at       TIMESTAMPTZ NOT NULL,
    expires_at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_console_sessions_expiry ON console_sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_console_sessions_membership ON console_sessions(tenant_id, membership_id);
CREATE TABLE IF NOT EXISTS pending_signups (
    token_hash       TEXT PRIMARY KEY,
    email            TEXT NOT NULL,
    normalized_email TEXT NOT NULL,
    tenant_name      TEXT NOT NULL,
    tenant_slug      TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    consumed_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_pending_signups_expiry ON pending_signups(expires_at);
CREATE TABLE IF NOT EXISTS user_password_resets (
    token_hash  TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_user_password_resets_expiry ON user_password_resets(expires_at);
DO $$
DECLARE tenant_row RECORD;
BEGIN
    -- tenant_memberships forces RLS for its owner too: bind each tenant.
    FOR tenant_row IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.current_tenant', tenant_row.id, true);
        UPDATE tenant_memberships SET mfa_required = 1
        WHERE roles_json ?| ARRAY['pedagogy_manager','trainer'];
    END LOOP;
    PERFORM set_config('app.current_tenant', '', true);
END $$;
`
