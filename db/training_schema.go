// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const trainingMigration = `
CREATE TABLE training_receipts (
 tenant_id TEXT NOT NULL, membership_id TEXT NOT NULL, learner_id TEXT,
 idempotency_key TEXT NOT NULL, operation TEXT NOT NULL, request_hash TEXT NOT NULL,
 response_json TEXT NOT NULL, actor_user_id TEXT NOT NULL, created_at TIMESTAMP NOT NULL,
 PRIMARY KEY(tenant_id,idempotency_key),
 FOREIGN KEY(tenant_id,membership_id) REFERENCES tenant_memberships(tenant_id,id)
);
CREATE TABLE conversation_contexts (
 tenant_id TEXT NOT NULL, membership_id TEXT NOT NULL, learner_id TEXT,
 conversation_id TEXT NOT NULL, training_id TEXT NOT NULL, domain_id TEXT NOT NULL DEFAULT '',
 revision TEXT NOT NULL, selection_version INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY(tenant_id,membership_id,conversation_id),
 FOREIGN KEY(tenant_id,membership_id) REFERENCES tenant_memberships(tenant_id,id)
);
CREATE TABLE training_workflows (
 tenant_id TEXT NOT NULL, formation_id TEXT NOT NULL, author_id TEXT NOT NULL,
 source_domain_id TEXT NOT NULL DEFAULT '', free_publication INTEGER NOT NULL DEFAULT 0,
 kind TEXT NOT NULL CHECK(kind IN ('institutional','shared')),
 PRIMARY KEY(tenant_id,formation_id),
 FOREIGN KEY(tenant_id,formation_id) REFERENCES formations(tenant_id,id)
);
CREATE TABLE training_submissions (
 tenant_id TEXT NOT NULL, id TEXT NOT NULL, formation_id TEXT NOT NULL,
 version_id TEXT NOT NULL, revision TEXT NOT NULL, author_id TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('pending','changes_requested','published','superseded')),
 reason TEXT NOT NULL DEFAULT '', document_json TEXT NOT NULL, created_at TIMESTAMP NOT NULL,
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,formation_id) REFERENCES formations(tenant_id,id),
 FOREIGN KEY(tenant_id,version_id) REFERENCES formation_versions(tenant_id,id)
);
CREATE INDEX idx_training_submissions ON training_submissions(tenant_id,formation_id,created_at);
CREATE TABLE training_proposals (
 tenant_id TEXT NOT NULL, membership_id TEXT NOT NULL, learner_id TEXT,
 training_id TEXT NOT NULL, id TEXT NOT NULL, expected_revision TEXT NOT NULL,
 document_json TEXT NOT NULL, created_at TIMESTAMP NOT NULL,
 PRIMARY KEY(tenant_id,membership_id,training_id),
 FOREIGN KEY(tenant_id,membership_id) REFERENCES tenant_memberships(tenant_id,id)
);
`

const postgresTrainingMigration = trainingMigration + `
ALTER TABLE training_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE training_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON training_receipts USING (tenant_id=current_setting('app.current_tenant',true)) WITH CHECK (tenant_id=current_setting('app.current_tenant',true));
ALTER TABLE training_receipts ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE conversation_contexts ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation_contexts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON conversation_contexts USING (tenant_id=current_setting('app.current_tenant',true)) WITH CHECK (tenant_id=current_setting('app.current_tenant',true));
ALTER TABLE training_workflows ENABLE ROW LEVEL SECURITY;
ALTER TABLE training_workflows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON training_workflows USING (tenant_id=current_setting('app.current_tenant',true)) WITH CHECK (tenant_id=current_setting('app.current_tenant',true));
ALTER TABLE training_submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE training_submissions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON training_submissions USING (tenant_id=current_setting('app.current_tenant',true)) WITH CHECK (tenant_id=current_setting('app.current_tenant',true));
ALTER TABLE training_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE training_proposals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON training_proposals USING (tenant_id=current_setting('app.current_tenant',true)) WITH CHECK (tenant_id=current_setting('app.current_tenant',true));
ALTER TABLE conversation_contexts ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
ALTER TABLE training_proposals ADD COLUMN erasure_row_id BIGINT GENERATED ALWAYS AS IDENTITY;
`
