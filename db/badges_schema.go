// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const badgesMigration = `
CREATE TABLE learning_badges (
 id TEXT NOT NULL, tenant_id TEXT NOT NULL, learner_id TEXT NOT NULL,
 enrollment_id TEXT NOT NULL, formation_version_id TEXT NOT NULL,
 concept_id TEXT NOT NULL DEFAULT '', label TEXT NOT NULL,
 kind TEXT NOT NULL CHECK (kind IN ('mastery','formation_completed','retention')),
 retention_days INTEGER NOT NULL CHECK (retention_days IN (0,7,30,90)),
 policy_version TEXT NOT NULL, achieved_at TIMESTAMP NOT NULL, recorded_at TIMESTAMP NOT NULL,
 PRIMARY KEY (tenant_id, id),
 UNIQUE (tenant_id, enrollment_id, concept_id, kind, retention_days, policy_version),
 FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id),
 FOREIGN KEY (tenant_id, formation_version_id) REFERENCES formation_versions(tenant_id, id),
 CHECK ((kind = 'retention' AND retention_days > 0 AND concept_id <> '')
 OR (kind = 'mastery' AND retention_days = 0 AND concept_id <> '')
 OR (kind = 'formation_completed' AND retention_days = 0 AND concept_id = ''))
);
CREATE INDEX idx_learning_badges_learner ON learning_badges(tenant_id, learner_id, enrollment_id, id);
CREATE TABLE learning_badge_evidence (
 tenant_id TEXT NOT NULL, badge_id TEXT NOT NULL, learner_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL, concept_id TEXT NOT NULL, snapshot_id BIGINT NOT NULL,
 responded_at TIMESTAMP NOT NULL, evaluated_at TIMESTAMP NOT NULL, evaluation_method TEXT NOT NULL,
 PRIMARY KEY (tenant_id, badge_id, attempt_id),
 FOREIGN KEY (tenant_id, badge_id) REFERENCES learning_badges(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX idx_learning_badge_evidence_learner ON learning_badge_evidence(tenant_id, learner_id);
`

const postgresBadgesMigration = badgesMigration + `
ALTER TABLE learning_badges ENABLE ROW LEVEL SECURITY;
ALTER TABLE learning_badges FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON learning_badges
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
ALTER TABLE learning_badge_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE learning_badge_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON learning_badge_evidence
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
`
