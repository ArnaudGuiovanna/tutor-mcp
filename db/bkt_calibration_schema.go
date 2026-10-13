// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

// No backfill: historical applied parameters do not establish independent
// estimates. Only the new committed-observation pipeline creates these rows.
const bktCalibrationMigration = `
CREATE UNIQUE INDEX idx_enrollments_bkt_learner_scope ON enrollments(tenant_id,id,learner_id);
CREATE TABLE concept_bkt_estimates (
 tenant_id TEXT NOT NULL, enrollment_id TEXT NOT NULL,
 formation_version_id TEXT NOT NULL, formation_concept_id TEXT NOT NULL,
 learner_id TEXT NOT NULL, curriculum_version INTEGER NOT NULL CHECK (curriculum_version > 0),
 reference_policy_version TEXT NOT NULL, policy_version TEXT NOT NULL,
 p_learn DOUBLE PRECISION NOT NULL CHECK (p_learn >= 0 AND p_learn <= 1),
 p_forget DOUBLE PRECISION NOT NULL CHECK (p_forget >= 0 AND p_forget <= 1),
 p_slip DOUBLE PRECISION NOT NULL CHECK (p_slip >= 0 AND p_slip <= 1),
 p_guess DOUBLE PRECISION NOT NULL CHECK (p_guess >= 0 AND p_guess <= 1),
 admissible_observations INTEGER NOT NULL CHECK (admissible_observations > 0 AND admissible_observations <= 20),
 last_interaction_id BIGINT NOT NULL CHECK (last_interaction_id > 0),
 evidence_hash TEXT NOT NULL CHECK (length(evidence_hash) = 64), updated_at TIMESTAMP NOT NULL,
 PRIMARY KEY (tenant_id, enrollment_id, formation_concept_id, curriculum_version, policy_version),
 FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id),
 FOREIGN KEY (tenant_id, enrollment_id, formation_version_id)
  REFERENCES enrollments(tenant_id, id, formation_version_id),
 FOREIGN KEY (tenant_id, enrollment_id, learner_id)
  REFERENCES enrollments(tenant_id, id, learner_id),
 FOREIGN KEY (tenant_id, formation_version_id, formation_concept_id)
  REFERENCES formation_concepts(tenant_id, formation_version_id, id)
);
CREATE INDEX idx_concept_bkt_estimates_learner ON concept_bkt_estimates(tenant_id, learner_id);
CREATE INDEX idx_interactions_bkt_candidates
 ON interactions(tenant_id,enrollment_id,formation_concept_id,created_at DESC,id DESC)
 WHERE assessment_attempt_id IS NOT NULL;
`

const postgresBKTCalibrationMigration = bktCalibrationMigration + `
ALTER TABLE concept_bkt_estimates ENABLE ROW LEVEL SECURITY;
ALTER TABLE concept_bkt_estimates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON concept_bkt_estimates
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`
