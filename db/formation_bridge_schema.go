// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const formationBridgeMigration = `
ALTER TABLE formations ADD COLUMN owner_membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE formations ADD COLUMN enrollment_policy TEXT NOT NULL DEFAULT 'invitation'
 CHECK (enrollment_policy IN ('open','invitation','approval'));
UPDATE formations SET owner_membership_id = COALESCE((SELECT MIN(tm.id) FROM tenant_memberships tm
 WHERE tm.tenant_id = formations.tenant_id AND tm.user_id = formations.created_by), '');
CREATE TABLE formation_trainers (
 tenant_id TEXT NOT NULL, formation_id TEXT NOT NULL, membership_id TEXT NOT NULL,
 assigned_by TEXT NOT NULL, assigned_at TIMESTAMP NOT NULL,
 PRIMARY KEY (tenant_id, formation_id, membership_id),
 FOREIGN KEY (tenant_id, formation_id) REFERENCES formations(tenant_id, id),
 FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id)
);
ALTER TABLE domains ADD COLUMN formation_enrollment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE domains ADD COLUMN formation_version_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_domains_formation_enrollment ON domains(tenant_id, formation_enrollment_id)
 WHERE formation_enrollment_id <> '';
CREATE TABLE enrollment_migrations (
 tenant_id TEXT NOT NULL, source_enrollment_id TEXT NOT NULL, target_enrollment_id TEXT NOT NULL,
 domain_id TEXT NOT NULL, actor_membership_id TEXT NOT NULL, created_at TIMESTAMP NOT NULL,
 response_json TEXT NOT NULL,
 PRIMARY KEY (tenant_id, source_enrollment_id),
 FOREIGN KEY (tenant_id, source_enrollment_id) REFERENCES enrollments(tenant_id, id),
 FOREIGN KEY (tenant_id, target_enrollment_id) REFERENCES enrollments(tenant_id, id)
);
`

const postgresFormationBridgeMigration = formationBridgeMigration + `
ALTER TABLE formation_trainers ENABLE ROW LEVEL SECURITY;
ALTER TABLE formation_trainers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON formation_trainers
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
ALTER TABLE enrollment_migrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE enrollment_migrations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON enrollment_migrations
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`
