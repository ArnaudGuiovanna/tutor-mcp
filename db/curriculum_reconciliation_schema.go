// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const curriculumReconciliationSchema = `
ALTER TABLE interactions ADD COLUMN curriculum_invalidated_version INTEGER NOT NULL DEFAULT 0 CHECK (curriculum_invalidated_version >= 0);
ALTER TABLE assessment_attempts ADD COLUMN curriculum_invalidated_version INTEGER NOT NULL DEFAULT 0 CHECK (curriculum_invalidated_version >= 0);
ALTER TABLE transfer_records ADD COLUMN curriculum_invalidated_version INTEGER NOT NULL DEFAULT 0 CHECK (curriculum_invalidated_version >= 0);
`

// Invalidation is a one-way applicability annotation, not a rewrite of the
// original outcome, response, trust decision or frozen assessment contract.
// SQLite cannot extend a CHECK allow-list in place. MigrateContext uses the
// documented foreign-key-safe rebuild procedure when upgrading existing data.
const sqliteCurriculumReconciliationMigration = curriculumReconciliationSchema + `
CREATE TABLE curriculum_versions_reconciled (
    domain_id TEXT NOT NULL,
    learner_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version >= 1),
    parent_version INTEGER,
    snapshot_json TEXT NOT NULL CHECK (json_valid(snapshot_json)),
    operation_type TEXT NOT NULL CHECK (operation_type IN ('create','baseline_import','add','rename','update_metadata','split','merge','remove','legacy_graph_update','repair_prerequisites')),
    operation_json TEXT NOT NULL CHECK (json_valid(operation_json)),
    provenance_json TEXT NOT NULL CHECK (json_valid(provenance_json)),
    review_json TEXT NOT NULL CHECK (json_valid(review_json)),
    created_by TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    tenant_id TEXT NOT NULL DEFAULT 'tenant_legacy',
    PRIMARY KEY (domain_id, version),
    FOREIGN KEY (domain_id, parent_version) REFERENCES curriculum_versions(domain_id, version)
);
INSERT INTO curriculum_versions_reconciled
 (domain_id, learner_id, version, parent_version, snapshot_json, operation_type, operation_json, provenance_json, review_json, created_by, created_at, tenant_id)
 SELECT domain_id, learner_id, version, parent_version, snapshot_json, operation_type, operation_json, provenance_json, review_json, created_by, created_at, tenant_id FROM curriculum_versions;
DROP TABLE curriculum_versions;
ALTER TABLE curriculum_versions_reconciled RENAME TO curriculum_versions;
CREATE INDEX idx_curriculum_versions_learner_domain ON curriculum_versions(learner_id, domain_id, version DESC);
CREATE TRIGGER curriculum_versions_no_update BEFORE UPDATE ON curriculum_versions
BEGIN SELECT RAISE(ABORT, 'curriculum versions are immutable'); END;
CREATE TRIGGER curriculum_versions_no_delete BEFORE DELETE ON curriculum_versions
BEGIN SELECT RAISE(ABORT, 'curriculum versions are immutable'); END;
CREATE TRIGGER interaction_curriculum_invalidation_immutable BEFORE UPDATE OF curriculum_invalidated_version ON interactions
WHEN OLD.curriculum_invalidated_version > 0 AND NEW.curriculum_invalidated_version <> OLD.curriculum_invalidated_version
BEGIN SELECT RAISE(ABORT, 'curriculum invalidation is immutable'); END;
CREATE TRIGGER assessment_curriculum_invalidation_immutable BEFORE UPDATE OF curriculum_invalidated_version ON assessment_attempts
WHEN OLD.curriculum_invalidated_version > 0 AND NEW.curriculum_invalidated_version <> OLD.curriculum_invalidated_version
BEGIN SELECT RAISE(ABORT, 'curriculum invalidation is immutable'); END;
CREATE TRIGGER transfer_curriculum_invalidation_immutable BEFORE UPDATE OF curriculum_invalidated_version ON transfer_records
WHEN OLD.curriculum_invalidated_version > 0 AND NEW.curriculum_invalidated_version <> OLD.curriculum_invalidated_version
BEGIN SELECT RAISE(ABORT, 'curriculum invalidation is immutable'); END;
`

const postgresCurriculumReconciliationMigration = curriculumReconciliationSchema + `
ALTER TABLE curriculum_versions DROP CONSTRAINT curriculum_versions_operation_type_check;
ALTER TABLE curriculum_versions ADD CONSTRAINT curriculum_versions_operation_type_check
 CHECK (operation_type IN ('create','baseline_import','add','rename','update_metadata','split','merge','remove','legacy_graph_update','repair_prerequisites'));
CREATE FUNCTION tutor_immutable_curriculum_invalidation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.curriculum_invalidated_version > 0 AND NEW.curriculum_invalidated_version <> OLD.curriculum_invalidated_version
 THEN RAISE EXCEPTION 'curriculum invalidation is immutable'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER interaction_curriculum_invalidation_immutable BEFORE UPDATE OF curriculum_invalidated_version ON interactions
 FOR EACH ROW EXECUTE FUNCTION tutor_immutable_curriculum_invalidation();
CREATE TRIGGER assessment_curriculum_invalidation_immutable BEFORE UPDATE OF curriculum_invalidated_version ON assessment_attempts
 FOR EACH ROW EXECUTE FUNCTION tutor_immutable_curriculum_invalidation();
CREATE TRIGGER transfer_curriculum_invalidation_immutable BEFORE UPDATE OF curriculum_invalidated_version ON transfer_records
 FOR EACH ROW EXECUTE FUNCTION tutor_immutable_curriculum_invalidation();
`
