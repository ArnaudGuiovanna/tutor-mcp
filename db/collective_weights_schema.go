// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

// concept_collective_weights is append-only and holds no learner identifier.
// collective_weight_applications records, per enrollment and concept, the last
// weight version applied to that learner; it carries the learner for erasure.
const collectiveWeightsMigration = `
CREATE TABLE concept_collective_weights (
 tenant_id TEXT NOT NULL, formation_version_id TEXT NOT NULL, formation_concept_id TEXT NOT NULL,
 version INTEGER NOT NULL CHECK (version >= 1),
 p_learn DOUBLE PRECISION NOT NULL, p_forget DOUBLE PRECISION NOT NULL,
 p_slip DOUBLE PRECISION NOT NULL, p_guess DOUBLE PRECISION NOT NULL,
 contributors INTEGER NOT NULL CHECK (contributors >= 30),
 policy_version TEXT NOT NULL, computed_at TIMESTAMP NOT NULL,
 PRIMARY KEY (tenant_id, formation_concept_id, version),
 FOREIGN KEY (tenant_id, formation_concept_id) REFERENCES formation_concepts(tenant_id, id)
);
CREATE TABLE collective_weight_applications (
 tenant_id TEXT NOT NULL, enrollment_id TEXT NOT NULL, formation_concept_id TEXT NOT NULL,
 learner_id TEXT NOT NULL, weight_version INTEGER NOT NULL,
 mode TEXT NOT NULL CHECK (mode IN ('prior','blend')), applied_at TIMESTAMP NOT NULL,
 PRIMARY KEY (tenant_id, enrollment_id, formation_concept_id),
 FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id)
);
CREATE INDEX idx_collective_weight_applications_learner ON collective_weight_applications(tenant_id, learner_id);
`

const postgresCollectiveWeightsMigration = collectiveWeightsMigration + `
ALTER TABLE concept_collective_weights ENABLE ROW LEVEL SECURITY;
ALTER TABLE concept_collective_weights FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON concept_collective_weights
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
ALTER TABLE collective_weight_applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE collective_weight_applications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON collective_weight_applications
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`
