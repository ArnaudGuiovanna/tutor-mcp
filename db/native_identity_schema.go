// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

// Native identities have no email, login name or password. The rebuild retains
// historical account values, indexes and legacy synchronization triggers.
const sqliteNativeIdentityMigration = `
CREATE TABLE users_native (
 id TEXT PRIMARY KEY,
 email TEXT,
 normalized_email TEXT,
 password_hash TEXT,
 status TEXT NOT NULL CHECK (status IN ('pending','active','suspended','revoked')),
 email_verified_at DATETIME,
 token_version INTEGER NOT NULL DEFAULT 1 CHECK (token_version >= 1),
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL,
 identity_mode TEXT NOT NULL DEFAULT 'email' CHECK (identity_mode IN ('email','username','local','pubkey') AND (identity_mode = 'email' OR email_verified_at IS NULL)),
 login_name TEXT,
 CHECK ((identity_mode = 'pubkey' AND email IS NULL AND normalized_email IS NULL AND password_hash IS NULL AND login_name IS NULL) OR (identity_mode <> 'pubkey' AND email IS NOT NULL AND normalized_email IS NOT NULL AND password_hash IS NOT NULL))
);
INSERT INTO users_native SELECT id,email,normalized_email,password_hash,status,email_verified_at,token_version,created_at,updated_at,identity_mode,login_name FROM users;
DROP TABLE users;
ALTER TABLE users_native RENAME TO users;
CREATE INDEX idx_users_normalized_email ON users(normalized_email);
CREATE UNIQUE INDEX idx_users_login_name ON users(login_name) WHERE login_name IS NOT NULL;
CREATE TABLE learners_native (
 id TEXT PRIMARY KEY,
 email TEXT UNIQUE,
 password_hash TEXT,
 objective TEXT NOT NULL,
 webhook_url TEXT DEFAULT '',
 profile_json TEXT DEFAULT '{}',
 created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
 last_active DATETIME,
 email_verified_at DATETIME,
 tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy',
 user_id TEXT NOT NULL DEFAULT '',
 membership_id TEXT NOT NULL DEFAULT '',
 identity_mode TEXT NOT NULL DEFAULT 'email' CHECK (identity_mode IN ('email','username','local','pubkey') AND (identity_mode = 'email' OR email_verified_at IS NULL)),
 CHECK ((identity_mode = 'pubkey' AND email IS NULL AND password_hash IS NULL) OR (identity_mode <> 'pubkey' AND email IS NOT NULL AND password_hash IS NOT NULL))
);
INSERT INTO learners_native SELECT id,email,password_hash,objective,webhook_url,profile_json,created_at,last_active,email_verified_at,tenant_id,user_id,membership_id,identity_mode FROM learners;
DROP TABLE learners;
ALTER TABLE learners_native RENAME TO learners;
CREATE UNIQUE INDEX idx_learners_email_lower ON learners(lower(email));
CREATE UNIQUE INDEX idx_learners_tenant_id_id ON learners(tenant_id,id);
CREATE UNIQUE INDEX idx_learners_tenant_membership ON learners(tenant_id,membership_id);
CREATE TRIGGER learners_identity_after_insert
AFTER INSERT ON learners
WHEN NEW.identity_mode <> 'pubkey'
BEGIN
    UPDATE learners
    SET tenant_id = CASE WHEN NEW.tenant_id = '' THEN 'tenant_legacy' ELSE NEW.tenant_id END,
        user_id = CASE WHEN NEW.user_id = '' THEN NEW.id ELSE NEW.user_id END,
        membership_id = CASE WHEN NEW.membership_id = '' THEN 'membership_legacy_' || NEW.id ELSE NEW.membership_id END
    WHERE id = NEW.id;
    INSERT OR IGNORE INTO users
        (id, email, normalized_email, password_hash, status, email_verified_at, token_version, created_at, updated_at)
    VALUES (
        CASE WHEN NEW.user_id = '' THEN NEW.id ELSE NEW.user_id END,
        NEW.email, lower(NEW.email), NEW.password_hash,
        CASE WHEN NEW.email_verified_at IS NULL THEN 'pending' ELSE 'active' END,
        NEW.email_verified_at, 1, NEW.created_at, COALESCE(NEW.last_active, NEW.created_at)
    );
    INSERT OR IGNORE INTO tenant_memberships
        (id, tenant_id, user_id, learner_id, roles_json, status, version, created_at, updated_at)
    VALUES (
        CASE WHEN NEW.membership_id = '' THEN 'membership_legacy_' || NEW.id ELSE NEW.membership_id END,
        CASE WHEN NEW.tenant_id = '' THEN 'tenant_legacy' ELSE NEW.tenant_id END,
        CASE WHEN NEW.user_id = '' THEN NEW.id ELSE NEW.user_id END,
        NEW.id, '["learner"]',
        CASE WHEN NEW.email_verified_at IS NULL THEN 'invited' ELSE 'active' END,
        1, NEW.created_at, COALESCE(NEW.last_active, NEW.created_at)
    );
END;
CREATE TRIGGER learners_identity_after_verification
AFTER UPDATE OF email_verified_at ON learners
WHEN OLD.email_verified_at IS NULL AND NEW.email_verified_at IS NOT NULL
BEGIN
    UPDATE users
    SET status = 'active', email_verified_at = NEW.email_verified_at,
        updated_at = CURRENT_TIMESTAMP
    WHERE id = NEW.user_id AND status = 'pending';
    UPDATE tenant_memberships
    SET status = 'active', version = version + 1, updated_at = CURRENT_TIMESTAMP
    WHERE tenant_id = NEW.tenant_id AND id = NEW.membership_id AND status = 'invited';
END;
CREATE TRIGGER learners_identity_after_password_change
AFTER UPDATE OF password_hash ON learners
WHEN OLD.password_hash <> NEW.password_hash AND NOT (
 NEW.password_hash = 'erased' AND EXISTS (
  SELECT 1 FROM tenant_dsar_requests r JOIN tenant_memberships tm
   ON tm.tenant_id = r.tenant_id AND tm.learner_id = r.learner_id
  WHERE r.tenant_id = NEW.tenant_id AND r.learner_id = NEW.id
   AND r.kind = 'erase' AND r.status = 'processing' AND tm.status = 'revoked'
 ))
BEGIN
 UPDATE users SET password_hash = NEW.password_hash, token_version = token_version + 1,
  updated_at = CURRENT_TIMESTAMP WHERE id = NEW.user_id;
 UPDATE tenant_memberships SET version = version + 1, updated_at = CURRENT_TIMESTAMP
  WHERE user_id = NEW.user_id AND status IN ('invited','active');
END;
` + nativeIdentitySchema

// Keys are global authentication identities. Challenges and grants remain
// tenant-owned. The key registry reserves retired keys too, preventing either
// purpose from being reused as another account's identity or recovery key.
const nativeIdentitySchema = `
CREATE TABLE native_identities (
 id TEXT PRIMARY KEY,
 user_id TEXT NOT NULL UNIQUE REFERENCES users(id),
 public_key TEXT NOT NULL UNIQUE CHECK (length(public_key) = 64),
 recovery_public_key TEXT NOT NULL UNIQUE CHECK (length(recovery_public_key) = 64 AND recovery_public_key <> public_key),
 display_name TEXT NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('active','revoked')),
 version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
 created_at TIMESTAMP NOT NULL,
 updated_at TIMESTAMP NOT NULL
);
CREATE TABLE native_identity_keys (
 public_key TEXT PRIMARY KEY CHECK (length(public_key) = 64),
 identity_id TEXT NOT NULL REFERENCES native_identities(id),
 purpose TEXT NOT NULL CHECK (purpose IN ('identity','recovery')),
 created_at TIMESTAMP NOT NULL,
 retired_at TIMESTAMP
);
CREATE INDEX idx_native_identity_keys_identity ON native_identity_keys(identity_id);
CREATE TABLE native_challenges (
 token_hash TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id),
 identity_id TEXT NOT NULL REFERENCES native_identities(id),
 user_id TEXT NOT NULL REFERENCES users(id),
 learner_id TEXT,
 public_key TEXT NOT NULL,
 action TEXT NOT NULL CHECK (action IN ('session','rotate','recover','logout')),
 scopes_json TEXT NOT NULL,
 key_version INTEGER NOT NULL CHECK (key_version >= 1),
 url TEXT NOT NULL,
 method TEXT NOT NULL CHECK (method = 'POST'),
 payload_hash TEXT NOT NULL,
 created_at TIMESTAMP NOT NULL,
 expires_at TIMESTAMP NOT NULL CHECK (expires_at > created_at),
 consumed_at TIMESTAMP,
 FOREIGN KEY (tenant_id,learner_id) REFERENCES learners(tenant_id,id)
);
CREATE INDEX idx_native_challenges_identity_expiry ON native_challenges(tenant_id,identity_id,expires_at);
CREATE TABLE native_sessions (
 id TEXT PRIMARY KEY,
 identity_id TEXT NOT NULL REFERENCES native_identities(id),
 tenant_id TEXT NOT NULL REFERENCES tenants(id),
 user_id TEXT NOT NULL REFERENCES users(id),
 membership_id TEXT NOT NULL,
 learner_id TEXT,
 key_version INTEGER NOT NULL CHECK (key_version >= 1),
 scopes_json TEXT NOT NULL,
 created_at TIMESTAMP NOT NULL,
 expires_at TIMESTAMP NOT NULL CHECK (expires_at > created_at),
 revoked_at TIMESTAMP,
 FOREIGN KEY (tenant_id,membership_id) REFERENCES tenant_memberships(tenant_id,id),
 FOREIGN KEY (tenant_id,learner_id) REFERENCES learners(tenant_id,id)
);
CREATE INDEX idx_native_sessions_identity ON native_sessions(identity_id,expires_at);
CREATE INDEX idx_native_sessions_membership ON native_sessions(tenant_id,membership_id,expires_at);
`

const postgresNativeIdentityMigration = `
ALTER TABLE users ALTER COLUMN email DROP NOT NULL;
ALTER TABLE users ALTER COLUMN normalized_email DROP NOT NULL;
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE users DROP CONSTRAINT users_check;
ALTER TABLE users ADD CONSTRAINT users_identity_mode_check CHECK (identity_mode IN ('email','username','local','pubkey') AND (identity_mode = 'email' OR email_verified_at IS NULL));
ALTER TABLE users ADD CONSTRAINT users_native_credentials_check CHECK ((identity_mode = 'pubkey' AND email IS NULL AND normalized_email IS NULL AND password_hash IS NULL AND login_name IS NULL) OR (identity_mode <> 'pubkey' AND email IS NOT NULL AND normalized_email IS NOT NULL AND password_hash IS NOT NULL));
ALTER TABLE learners ALTER COLUMN email DROP NOT NULL;
ALTER TABLE learners ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE learners DROP CONSTRAINT learners_check;
ALTER TABLE learners ADD CONSTRAINT learners_identity_mode_check CHECK (identity_mode IN ('email','username','local','pubkey') AND (identity_mode = 'email' OR email_verified_at IS NULL));
ALTER TABLE learners ADD CONSTRAINT learners_native_credentials_check CHECK ((identity_mode = 'pubkey' AND email IS NULL AND password_hash IS NULL) OR (identity_mode <> 'pubkey' AND email IS NOT NULL AND password_hash IS NOT NULL));
CREATE OR REPLACE FUNCTION tutor_legacy_learner_identity_before_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.identity_mode = 'pubkey' THEN RETURN NEW; END IF;
 IF NEW.tenant_id IS NULL OR NEW.tenant_id = '' THEN NEW.tenant_id := 'tenant_legacy'; END IF;
 IF NEW.user_id = '' THEN NEW.user_id := NEW.id; END IF;
 IF NEW.membership_id = '' THEN NEW.membership_id := 'membership_legacy_' || NEW.id; END IF;
 INSERT INTO users (id,email,normalized_email,password_hash,status,email_verified_at,token_version,created_at,updated_at)
 VALUES (NEW.user_id,NEW.email,lower(NEW.email),NEW.password_hash,
  CASE WHEN NEW.email_verified_at IS NULL THEN 'pending' ELSE 'active' END,
  NEW.email_verified_at,1,NEW.created_at,COALESCE(NEW.last_active,NEW.created_at))
 ON CONFLICT (id) DO NOTHING;
 RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION tutor_legacy_learner_membership_after_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.identity_mode = 'pubkey' THEN RETURN NEW; END IF;
 INSERT INTO tenant_memberships (id,tenant_id,user_id,learner_id,roles_json,status,version,created_at,updated_at)
 VALUES (NEW.membership_id,NEW.tenant_id,NEW.user_id,NEW.id,'["learner"]'::jsonb,
  CASE WHEN NEW.email_verified_at IS NULL THEN 'invited' ELSE 'active' END,
  1,NEW.created_at,COALESCE(NEW.last_active,NEW.created_at))
 ON CONFLICT (id) DO NOTHING;
 RETURN NEW;
END $$;
` + nativeIdentitySchema + `
ALTER TABLE native_challenges ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE native_sessions ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE native_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_challenges FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON native_challenges USING (
 tenant_id = current_setting('app.current_tenant',true)
 OR token_hash = current_setting('app.native_challenge',true)
 OR user_id = current_setting('app.native_user',true)
) WITH CHECK (
 tenant_id = current_setting('app.current_tenant',true)
 OR token_hash = current_setting('app.native_challenge',true)
 OR user_id = current_setting('app.native_user',true)
);
ALTER TABLE native_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON native_sessions USING (
 tenant_id = current_setting('app.current_tenant',true)
 OR user_id = current_setting('app.native_user',true)
) WITH CHECK (
 tenant_id = current_setting('app.current_tenant',true)
 OR user_id = current_setting('app.native_user',true)
);
-- Only the cryptographically authenticated rotation transaction binds this
-- user. Existing tenant policies and legacy MFA requirements remain intact.
CREATE POLICY native_identity_rotation ON tenant_memberships FOR UPDATE
 USING (user_id = current_setting('app.native_user',true))
 WITH CHECK (user_id = current_setting('app.native_user',true));
`
