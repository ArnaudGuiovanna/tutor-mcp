// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const learnerFormationsMigration = `
CREATE TABLE formation_admissions (
 tenant_id TEXT NOT NULL, cohort_id TEXT NOT NULL, membership_id TEXT NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('pending','invited','approved','rejected','revoked','cancelled')),
 version BIGINT NOT NULL DEFAULT 1, updated_at TIMESTAMP NOT NULL,
 PRIMARY KEY (tenant_id, cohort_id, membership_id),
 FOREIGN KEY (tenant_id, cohort_id) REFERENCES cohorts(tenant_id, id),
 FOREIGN KEY (tenant_id, membership_id) REFERENCES tenant_memberships(tenant_id, id)
);
`

const postgresLearnerFormationsMigration = learnerFormationsMigration + `
ALTER TABLE formation_admissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE formation_admissions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON formation_admissions
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`
