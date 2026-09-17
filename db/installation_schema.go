package db

// Email columns in the historical schema are NOT NULL and learners.email is
// unique. Non-email identities use an opaque identity:<id> storage key there,
// never an address and never a verified mailbox. Public models clear it.
// Identity mode is explicit, so authentication cannot mistake it for email.
const installationIdentityMigration = `
ALTER TABLE users ADD COLUMN identity_mode TEXT NOT NULL DEFAULT 'email'
    CHECK (identity_mode IN ('email','username','local') AND (identity_mode = 'email' OR email_verified_at IS NULL));
ALTER TABLE users ADD COLUMN login_name TEXT;
CREATE UNIQUE INDEX idx_users_login_name ON users(login_name) WHERE login_name IS NOT NULL;
ALTER TABLE learners ADD COLUMN identity_mode TEXT NOT NULL DEFAULT 'email'
    CHECK (identity_mode IN ('email','username','local') AND (identity_mode = 'email' OR email_verified_at IS NULL));
CREATE TABLE installation (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    profile TEXT NOT NULL CHECK (profile IN ('legacy','local','hobby','institution')),
    local_learner_id TEXT REFERENCES learners(id) ON DELETE SET NULL
);
CREATE TABLE hobby_account_links (
    token_hash TEXT PRIMARY KEY,
    purpose TEXT NOT NULL CHECK (purpose IN ('invite','reset')),
    learner_id TEXT REFERENCES learners(id) ON DELETE CASCADE,
    expires_at TIMESTAMP NOT NULL,
    consumed_at TIMESTAMP,
    CHECK ((purpose = 'invite' AND learner_id IS NULL) OR (purpose = 'reset' AND learner_id IS NOT NULL))
);
`
