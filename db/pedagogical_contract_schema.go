// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const pedagogicalContractSchema = `
CREATE TABLE pedagogical_decisions (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    learner_id TEXT NOT NULL REFERENCES learners(id),
    domain_id TEXT NOT NULL REFERENCES domains(id),
    session_id TEXT NOT NULL REFERENCES learning_sessions(id),
    curriculum_version INTEGER NOT NULL CHECK (curriculum_version >= 1),
    snapshot_json TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id),
    FOREIGN KEY (domain_id, curriculum_version) REFERENCES curriculum_versions(domain_id, version)
);
CREATE INDEX idx_pedagogical_decisions_scope ON pedagogical_decisions(tenant_id, learner_id, domain_id, created_at);
ALTER TABLE assessment_attempts ADD COLUMN decision_id TEXT REFERENCES pedagogical_decisions(id);
ALTER TABLE assessment_attempts ADD COLUMN curriculum_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assessment_attempts ADD COLUMN curriculum_concept_json TEXT NOT NULL DEFAULT '';
ALTER TABLE assessment_attempts ADD COLUMN outcome_ids_json TEXT NOT NULL DEFAULT '[]';
CREATE UNIQUE INDEX idx_assessment_decision_once ON assessment_attempts(decision_id) WHERE decision_id IS NOT NULL;
`

const sqlitePedagogicalContractMigration = pedagogicalContractSchema + `
CREATE TRIGGER pedagogical_decisions_no_update BEFORE UPDATE ON pedagogical_decisions
BEGIN SELECT RAISE(ABORT, 'pedagogical decisions are immutable'); END;
CREATE TRIGGER pedagogical_decisions_scope BEFORE INSERT ON pedagogical_decisions
WHEN NOT EXISTS (SELECT 1 FROM domains d JOIN learning_sessions s ON s.id = NEW.session_id
 WHERE d.id = NEW.domain_id AND d.learner_id = NEW.learner_id AND d.tenant_id = NEW.tenant_id
 AND s.learner_id = NEW.learner_id AND s.tenant_id = NEW.tenant_id AND s.domain_id = NEW.domain_id)
BEGIN SELECT RAISE(ABORT, 'pedagogical decision scope mismatch'); END;
CREATE TRIGGER assessment_decision_immutable BEFORE UPDATE OF decision_id, curriculum_version, curriculum_concept_json, outcome_ids_json ON assessment_attempts
WHEN NEW.decision_id IS NOT OLD.decision_id OR NEW.curriculum_version <> OLD.curriculum_version
 OR NEW.curriculum_concept_json <> OLD.curriculum_concept_json OR NEW.outcome_ids_json <> OLD.outcome_ids_json
BEGIN SELECT RAISE(ABORT, 'assessment curriculum binding is immutable'); END;
CREATE TRIGGER assessment_decision_identity_immutable BEFORE UPDATE OF tenant_id, learner_id, domain_id, session_id, concept_id, activity_type ON assessment_attempts
WHEN OLD.decision_id IS NOT NULL AND (
 NEW.tenant_id IS NOT OLD.tenant_id OR NEW.learner_id IS NOT OLD.learner_id
 OR NEW.domain_id IS NOT OLD.domain_id OR NEW.session_id IS NOT OLD.session_id
 OR NEW.concept_id IS NOT OLD.concept_id OR NEW.activity_type IS NOT OLD.activity_type)
BEGIN SELECT RAISE(ABORT, 'assessment decision identity is immutable'); END;
CREATE TRIGGER assessment_decision_scope BEFORE INSERT ON assessment_attempts
WHEN NEW.decision_id IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM pedagogical_decisions p JOIN domains d ON d.id = p.domain_id
 WHERE p.id = NEW.decision_id AND p.tenant_id = NEW.tenant_id AND p.learner_id = NEW.learner_id
 AND p.domain_id = NEW.domain_id AND p.session_id = NEW.session_id
 AND p.curriculum_version = NEW.curriculum_version AND d.graph_version = p.curriculum_version
 AND json_extract(p.snapshot_json, '$.contract.target_concept') = NEW.concept_id
 AND json_extract(p.snapshot_json, '$.contract.recommended_activity_type') = NEW.activity_type)
BEGIN SELECT RAISE(ABORT, 'assessment decision scope mismatch'); END;
`

const postgresPedagogicalContractMigration = pedagogicalContractSchema + `
ALTER TABLE pedagogical_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE pedagogical_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pedagogical_decisions
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
CREATE FUNCTION tutor_immutable_pedagogical_decision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'pedagogical decisions are immutable'; END; $$;
CREATE TRIGGER pedagogical_decisions_no_update BEFORE UPDATE ON pedagogical_decisions
 FOR EACH ROW EXECUTE FUNCTION tutor_immutable_pedagogical_decision();
CREATE FUNCTION tutor_immutable_assessment_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.decision_id IS DISTINCT FROM OLD.decision_id OR NEW.curriculum_version <> OLD.curriculum_version
 OR NEW.curriculum_concept_json <> OLD.curriculum_concept_json OR NEW.outcome_ids_json <> OLD.outcome_ids_json
 THEN RAISE EXCEPTION 'assessment curriculum binding is immutable'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER assessment_decision_immutable BEFORE UPDATE OF decision_id, curriculum_version, curriculum_concept_json, outcome_ids_json ON assessment_attempts
 FOR EACH ROW EXECUTE FUNCTION tutor_immutable_assessment_binding();
CREATE FUNCTION tutor_immutable_assessment_decision_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.decision_id IS NOT NULL AND (
 NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.learner_id IS DISTINCT FROM OLD.learner_id
 OR NEW.domain_id IS DISTINCT FROM OLD.domain_id OR NEW.session_id IS DISTINCT FROM OLD.session_id
 OR NEW.concept_id IS DISTINCT FROM OLD.concept_id OR NEW.activity_type IS DISTINCT FROM OLD.activity_type)
 THEN RAISE EXCEPTION 'assessment decision identity is immutable'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER assessment_decision_identity_immutable BEFORE UPDATE OF tenant_id, learner_id, domain_id, session_id, concept_id, activity_type ON assessment_attempts
 FOR EACH ROW EXECUTE FUNCTION tutor_immutable_assessment_decision_identity();
CREATE FUNCTION tutor_pedagogical_decision_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM domains d JOIN learning_sessions s ON s.id = NEW.session_id
 WHERE d.id = NEW.domain_id AND d.learner_id = NEW.learner_id AND d.tenant_id = NEW.tenant_id
 AND s.learner_id = NEW.learner_id AND s.tenant_id = NEW.tenant_id AND s.domain_id = NEW.domain_id)
 THEN RAISE EXCEPTION 'pedagogical decision scope mismatch'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER pedagogical_decisions_scope BEFORE INSERT ON pedagogical_decisions
 FOR EACH ROW EXECUTE FUNCTION tutor_pedagogical_decision_scope();
CREATE FUNCTION tutor_assessment_decision_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.decision_id IS NOT NULL THEN
  PERFORM 1 FROM domains WHERE id = NEW.domain_id FOR SHARE;
  IF NOT EXISTS (SELECT 1 FROM pedagogical_decisions p JOIN domains d ON d.id = p.domain_id
   WHERE p.id = NEW.decision_id AND p.tenant_id = NEW.tenant_id AND p.learner_id = NEW.learner_id
   AND p.domain_id = NEW.domain_id AND p.session_id = NEW.session_id
   AND p.curriculum_version = NEW.curriculum_version AND d.graph_version = p.curriculum_version
   AND p.snapshot_json::jsonb #>> '{contract,target_concept}' = NEW.concept_id
   AND p.snapshot_json::jsonb #>> '{contract,recommended_activity_type}' = NEW.activity_type)
  THEN RAISE EXCEPTION 'assessment decision scope mismatch'; END IF;
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER assessment_decision_scope BEFORE INSERT ON assessment_attempts
 FOR EACH ROW EXECUTE FUNCTION tutor_assessment_decision_scope();
`
