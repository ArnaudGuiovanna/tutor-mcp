// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

// A tenant erasure destroys only the tenant's learner credential copy. The
// compatibility password trigger must keep the global identity usable in
// other institutions. Ordinary password changes retain their existing global
// synchronization and session-revocation behavior.
const sqliteErasureIdentityMigration = `
DROP TRIGGER learners_identity_after_password_change;
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
UPDATE cohorts SET reserved_seats = (
 SELECT COALESCE(SUM(e.seat_reserved), 0) FROM enrollments e
 WHERE e.tenant_id = cohorts.tenant_id AND e.cohort_id = cohorts.id
), version = version + 1, updated_at = CURRENT_TIMESTAMP
WHERE reserved_seats <> (
 SELECT COALESCE(SUM(e.seat_reserved), 0) FROM enrollments e
 WHERE e.tenant_id = cohorts.tenant_id AND e.cohort_id = cohorts.id
);
`

const postgresErasureIdentityMigration = `
CREATE OR REPLACE FUNCTION tutor_learner_identity_after_update()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.email_verified_at IS NULL AND NEW.email_verified_at IS NOT NULL THEN
  UPDATE users SET status = 'active', email_verified_at = NEW.email_verified_at,
   updated_at = CURRENT_TIMESTAMP WHERE id = NEW.user_id AND status = 'pending';
  UPDATE tenant_memberships SET status = 'active', version = version + 1,
   updated_at = CURRENT_TIMESTAMP
   WHERE tenant_id = NEW.tenant_id AND id = NEW.membership_id AND status = 'invited';
 END IF;
 IF OLD.password_hash IS DISTINCT FROM NEW.password_hash AND NOT (
  NEW.password_hash = 'erased' AND EXISTS (
   SELECT 1 FROM tenant_dsar_requests r JOIN tenant_memberships tm
    ON tm.tenant_id = r.tenant_id AND tm.learner_id = r.learner_id
   WHERE r.tenant_id = NEW.tenant_id AND r.learner_id = NEW.id
    AND r.kind = 'erase' AND r.status = 'processing' AND tm.status = 'revoked'
  )) THEN
  UPDATE users SET password_hash = NEW.password_hash, token_version = token_version + 1,
   updated_at = CURRENT_TIMESTAMP WHERE id = NEW.user_id;
  UPDATE tenant_memberships SET version = version + 1, updated_at = CURRENT_TIMESTAMP
   WHERE user_id = NEW.user_id AND status IN ('invited','active');
 END IF;
 RETURN NEW;
END $$;

-- Non-secret identifiers permit bounded DELETEs under the worker's column
-- grants. ctid cannot be read without widening SELECT to token payloads.
ALTER TABLE refresh_tokens ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE oauth_codes ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE login_challenges ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE account_tokens ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE learner_approved_clients ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE tool_call_idempotency ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
CREATE UNIQUE INDEX idx_refresh_tokens_erasure_row ON refresh_tokens(tenant_id, erasure_row_id);
CREATE UNIQUE INDEX idx_oauth_codes_erasure_row ON oauth_codes(tenant_id, erasure_row_id);
CREATE UNIQUE INDEX idx_login_challenges_erasure_row ON login_challenges(tenant_id, erasure_row_id);
CREATE UNIQUE INDEX idx_account_tokens_erasure_row ON account_tokens(tenant_id, erasure_row_id);
CREATE UNIQUE INDEX idx_learner_approved_clients_erasure_row ON learner_approved_clients(tenant_id, erasure_row_id);
CREATE UNIQUE INDEX idx_tool_call_idempotency_erasure_row ON tool_call_idempotency(tenant_id, erasure_row_id);

-- Repair seats left behind by an erasure performed by an older binary. The
-- migrator remains under forced tenant RLS for every repair.
DO $$
DECLARE tenant_key text;
 previous_tenant text := current_setting('app.current_tenant', true);
BEGIN
 FOR tenant_key IN SELECT id FROM tenants LOOP
  PERFORM set_config('app.current_tenant', tenant_key, true);
  UPDATE cohorts c SET reserved_seats = (
   SELECT COALESCE(SUM(e.seat_reserved), 0) FROM enrollments e
   WHERE e.tenant_id = c.tenant_id AND e.cohort_id = c.id
  ), version = version + 1, updated_at = CURRENT_TIMESTAMP
  WHERE c.tenant_id = tenant_key AND c.reserved_seats <> (
   SELECT COALESCE(SUM(e.seat_reserved), 0) FROM enrollments e
   WHERE e.tenant_id = c.tenant_id AND e.cohort_id = c.id
  );
 END LOOP;
 PERFORM set_config('app.current_tenant', COALESCE(previous_tenant, ''), true);
END $$;
`
