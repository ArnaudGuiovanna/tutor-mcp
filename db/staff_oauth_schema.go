// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

// Staff authenticate as memberships, without an artificial learner profile.
// SQLite needs a table rebuild to relax the historical learner NOT NULL.
// These leaf tables have no inbound foreign keys. Preserve every live grant.
const sqliteStaffOAuthMigration = `
CREATE TABLE credential_tenant_routes_staff (
 kind TEXT NOT NULL CHECK (kind IN ('authorization_code','refresh_token','email_verification','password_reset','login_challenge')),
 credential_key TEXT NOT NULL, tenant_id TEXT NOT NULL REFERENCES tenants(id),
 user_id TEXT NOT NULL REFERENCES users(id), membership_id TEXT NOT NULL, learner_id TEXT,
 expires_at DATETIME NOT NULL, created_at DATETIME NOT NULL,
 PRIMARY KEY (kind, credential_key),
 FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id),
 FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id),
 CHECK (learner_id IS NOT NULL OR kind IN ('authorization_code','refresh_token'))
);
INSERT INTO credential_tenant_routes_staff SELECT * FROM credential_tenant_routes;
DROP TABLE credential_tenant_routes;
ALTER TABLE credential_tenant_routes_staff RENAME TO credential_tenant_routes;
CREATE INDEX idx_credential_tenant_routes_expiry ON credential_tenant_routes(expires_at, kind);

CREATE TABLE oauth_codes_staff (
 code TEXT PRIMARY KEY, learner_id TEXT REFERENCES learners(id),
 code_challenge TEXT NOT NULL, client_id TEXT NOT NULL DEFAULT '',
 expires_at DATETIME NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
 code_challenge_method TEXT NOT NULL DEFAULT '', redirect_uri TEXT NOT NULL DEFAULT '',
 resource TEXT NOT NULL DEFAULT '', scope TEXT NOT NULL DEFAULT 'learner',
 tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy', user_id TEXT NOT NULL DEFAULT '',
 membership_id TEXT NOT NULL DEFAULT '', membership_version INTEGER NOT NULL DEFAULT 1
);
INSERT INTO oauth_codes_staff SELECT code, learner_id, code_challenge, client_id,
 expires_at, created_at, code_challenge_method, redirect_uri, resource, scope,
 tenant_id, user_id, membership_id, membership_version FROM oauth_codes;
DROP TABLE oauth_codes;
ALTER TABLE oauth_codes_staff RENAME TO oauth_codes;
CREATE INDEX idx_oauth_codes_expires ON oauth_codes(expires_at);
CREATE INDEX idx_oauth_codes_tenant_learner ON oauth_codes(tenant_id, learner_id, expires_at);
CREATE INDEX idx_oauth_codes_tenant_membership ON oauth_codes(tenant_id, membership_id, expires_at);
CREATE TABLE refresh_tokens_staff (
 token TEXT PRIMARY KEY, learner_id TEXT REFERENCES learners(id),
 expires_at DATETIME NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
 client_id TEXT, family_id TEXT NOT NULL DEFAULT '', used_at DATETIME, revoked_at DATETIME,
 resource TEXT NOT NULL DEFAULT '', scope TEXT NOT NULL DEFAULT 'learner',
 tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy', user_id TEXT NOT NULL DEFAULT '',
 membership_id TEXT NOT NULL DEFAULT '', membership_version INTEGER NOT NULL DEFAULT 1
);
INSERT INTO refresh_tokens_staff SELECT token, learner_id, expires_at, created_at,
 client_id, family_id, used_at, revoked_at, resource, scope, tenant_id, user_id,
 membership_id, membership_version FROM refresh_tokens;
DROP TABLE refresh_tokens;
ALTER TABLE refresh_tokens_staff RENAME TO refresh_tokens;
CREATE INDEX idx_refresh_tokens_family ON refresh_tokens(family_id);
CREATE INDEX idx_refresh_tokens_client_resource ON refresh_tokens(client_id, resource);
CREATE INDEX idx_refresh_tokens_tenant_learner ON refresh_tokens(tenant_id, learner_id);
CREATE INDEX idx_refresh_tokens_tenant_membership ON refresh_tokens(tenant_id, membership_id, expires_at);
`

const postgresStaffOAuthMigration = `
ALTER TABLE credential_tenant_routes ALTER COLUMN learner_id DROP NOT NULL;
ALTER TABLE credential_tenant_routes ADD CONSTRAINT credential_route_staff_kind
 CHECK (learner_id IS NOT NULL OR kind IN ('authorization_code','refresh_token'));

ALTER TABLE oauth_codes ALTER COLUMN learner_id DROP NOT NULL;
ALTER TABLE refresh_tokens ALTER COLUMN learner_id DROP NOT NULL;
CREATE FUNCTION tutor_enforce_oauth_membership()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE resolved_tenant text;
BEGIN
 IF NEW.learner_id IS NOT NULL THEN
   SELECT tenant_id INTO resolved_tenant FROM learners WHERE id = NEW.learner_id;
   IF resolved_tenant IS NULL THEN
     RAISE EXCEPTION 'unknown learner for tenant-owned row' USING ERRCODE = '23503';
   END IF;
   IF NEW.tenant_id IS NULL THEN
     NEW.tenant_id := resolved_tenant;
   ELSIF NEW.tenant_id <> resolved_tenant THEN
     RAISE EXCEPTION 'cross-tenant learner relation' USING ERRCODE = '23514';
   END IF;
   RETURN NEW;
 END IF;
 IF NOT EXISTS (SELECT 1 FROM tenant_memberships tm
   WHERE tm.tenant_id = NEW.tenant_id AND tm.id = NEW.membership_id
     AND tm.user_id = NEW.user_id AND tm.learner_id IS NOT DISTINCT FROM NEW.learner_id) THEN
   RAISE EXCEPTION 'invalid OAuth membership relation' USING ERRCODE = '23503';
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER tenant_scope_guard ON oauth_codes;
CREATE TRIGGER tenant_scope_guard BEFORE INSERT OR UPDATE OF tenant_id, user_id, membership_id, learner_id
 ON oauth_codes FOR EACH ROW EXECUTE FUNCTION tutor_enforce_oauth_membership();
DROP TRIGGER tenant_scope_guard ON refresh_tokens;
CREATE TRIGGER tenant_scope_guard BEFORE INSERT OR UPDATE OF tenant_id, user_id, membership_id, learner_id
 ON refresh_tokens FOR EACH ROW EXECUTE FUNCTION tutor_enforce_oauth_membership();
`
