// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

// Latest anonymous snapshots written only by the worker. A NULL cell is below
// the minimum-contributor threshold or a small complementary group.
const statisticsMigration = `
CREATE TABLE cohort_statistics (
 tenant_id TEXT NOT NULL, cohort_id TEXT NOT NULL, formation_version_id TEXT NOT NULL,
 policy_version TEXT NOT NULL, computed_at TIMESTAMP NOT NULL,
 contributors INTEGER, participants INTEGER, completed_learners INTEGER,
 mean_mastery DOUBLE PRECISION, median_mastery DOUBLE PRECISION,
 PRIMARY KEY (tenant_id, cohort_id),
 FOREIGN KEY (tenant_id, cohort_id) REFERENCES cohorts(tenant_id, id)
);
CREATE TABLE cohort_concept_statistics (
 tenant_id TEXT NOT NULL, cohort_id TEXT NOT NULL, concept_id TEXT NOT NULL,
 observed_learners INTEGER, mean_mastery DOUBLE PRECISION, median_mastery DOUBLE PRECISION,
 PRIMARY KEY (tenant_id, cohort_id, concept_id),
 FOREIGN KEY (tenant_id, cohort_id) REFERENCES cohorts(tenant_id, id)
);
CREATE TABLE cohort_badge_statistics (
 tenant_id TEXT NOT NULL, cohort_id TEXT NOT NULL, kind TEXT NOT NULL
 CHECK (kind IN ('mastery','formation_completed','retention')),
 retention_days INTEGER NOT NULL CHECK (retention_days IN (0,7,30,90)),
 learners INTEGER,
 PRIMARY KEY (tenant_id, cohort_id, kind, retention_days),
 FOREIGN KEY (tenant_id, cohort_id) REFERENCES cohorts(tenant_id, id)
);
`

const postgresStatisticsMigration = statisticsMigration + `
ALTER TABLE cohort_statistics ENABLE ROW LEVEL SECURITY;
ALTER TABLE cohort_statistics FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cohort_statistics
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
ALTER TABLE cohort_concept_statistics ENABLE ROW LEVEL SECURITY;
ALTER TABLE cohort_concept_statistics FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cohort_concept_statistics
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
ALTER TABLE cohort_badge_statistics ENABLE ROW LEVEL SECURITY;
ALTER TABLE cohort_badge_statistics FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cohort_badge_statistics
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`
