// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const learningEventSchema = `
CREATE TABLE learning_events (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id),
 learner_id TEXT NOT NULL,
 domain_id TEXT NOT NULL REFERENCES domains(id),
 enrollment_id TEXT NOT NULL,
 concept_id TEXT NOT NULL,
 attempt_id TEXT,
 event_key TEXT NOT NULL,
 kind TEXT NOT NULL CHECK (kind IN ('response','feedback','instruction')),
 source TEXT NOT NULL CHECK (source IN ('committed_response','host_reported')),
 curriculum_version INTEGER NOT NULL CHECK (curriculum_version > 0),
 curriculum_invalidated_version INTEGER NOT NULL DEFAULT 0,
 occurred_at TIMESTAMP NOT NULL,
 FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id),
	FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id),
 FOREIGN KEY (tenant_id, attempt_id) REFERENCES assessment_attempts(tenant_id, id) ON DELETE CASCADE,
 FOREIGN KEY (domain_id, curriculum_version) REFERENCES curriculum_versions(domain_id, version),
 UNIQUE (tenant_id, learner_id, event_key),
 CHECK ((kind = 'response' AND source = 'committed_response' AND attempt_id IS NOT NULL)
  OR (kind <> 'response' AND source = 'host_reported'))
);
CREATE INDEX idx_learning_event_exposure ON learning_events(tenant_id, learner_id, domain_id, concept_id, curriculum_invalidated_version, occurred_at);
ALTER TABLE assessment_attempts ADD COLUMN event_protocol TEXT NOT NULL DEFAULT '';
ALTER TABLE assessment_attempts ADD COLUMN prior_exposure_at TIMESTAMP;
`

const learningEventScope = `NOT EXISTS (SELECT 1 FROM domains d JOIN enrollments e ON e.id = NEW.enrollment_id
 WHERE d.id = NEW.domain_id AND d.tenant_id = NEW.tenant_id AND d.learner_id = NEW.learner_id
 AND e.tenant_id = NEW.tenant_id AND e.learner_id = NEW.learner_id)
 OR (NEW.attempt_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM assessment_attempts a
 WHERE a.id = NEW.attempt_id AND a.tenant_id = NEW.tenant_id AND a.learner_id = NEW.learner_id
 AND a.domain_id = NEW.domain_id AND a.concept_id = NEW.concept_id AND a.enrollment_id = NEW.enrollment_id))`

const learningEventImmutable = `NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id OR NEW.learner_id <> OLD.learner_id
 OR NEW.domain_id <> OLD.domain_id OR NEW.enrollment_id <> OLD.enrollment_id OR NEW.concept_id <> OLD.concept_id
 OR COALESCE(NEW.attempt_id,'') <> COALESCE(OLD.attempt_id,'') OR NEW.event_key <> OLD.event_key
 OR NEW.kind <> OLD.kind OR NEW.source <> OLD.source OR NEW.curriculum_version <> OLD.curriculum_version
 OR NEW.occurred_at <> OLD.occurred_at
 OR (NEW.curriculum_invalidated_version <> OLD.curriculum_invalidated_version
  AND (OLD.curriculum_invalidated_version <> 0 OR NEW.curriculum_invalidated_version <= 0))`

const sqliteLearningEventMigration = learningEventSchema + `
CREATE TRIGGER learning_events_scope BEFORE INSERT ON learning_events WHEN ` + learningEventScope + `
 BEGIN SELECT RAISE(ABORT, 'learning event scope mismatch'); END;
CREATE TRIGGER learning_events_immutable BEFORE UPDATE ON learning_events WHEN ` + learningEventImmutable + `
 BEGIN SELECT RAISE(ABORT, 'learning events are immutable except first curriculum invalidation'); END;
CREATE TRIGGER assessment_event_protocol_immutable BEFORE UPDATE ON assessment_attempts
 WHEN NEW.event_protocol <> OLD.event_protocol OR (NEW.prior_exposure_at IS NOT OLD.prior_exposure_at
 AND NOT (OLD.status = 'prepared' AND NEW.status = 'submitted' AND OLD.prior_exposure_at IS NULL))
 BEGIN SELECT RAISE(ABORT, 'assessment exposure contract is immutable'); END;
`

const postgresLearningEventMigration = learningEventSchema + `
ALTER TABLE learning_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE learning_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON learning_events
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
CREATE FUNCTION tutor_learning_event_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ` + learningEventScope + ` THEN RAISE EXCEPTION 'learning event scope mismatch'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER learning_events_scope BEFORE INSERT ON learning_events FOR EACH ROW EXECUTE FUNCTION tutor_learning_event_scope();
CREATE FUNCTION tutor_learning_event_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ` + learningEventImmutable + ` THEN RAISE EXCEPTION 'learning events are immutable except first curriculum invalidation'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER learning_events_immutable BEFORE UPDATE ON learning_events FOR EACH ROW EXECUTE FUNCTION tutor_learning_event_immutable();
CREATE FUNCTION tutor_assessment_event_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.event_protocol <> OLD.event_protocol OR (NEW.prior_exposure_at IS DISTINCT FROM OLD.prior_exposure_at
 AND NOT (OLD.status = 'prepared' AND NEW.status = 'submitted' AND OLD.prior_exposure_at IS NULL))
 THEN RAISE EXCEPTION 'assessment exposure contract is immutable'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER assessment_event_protocol_immutable BEFORE UPDATE ON assessment_attempts FOR EACH ROW EXECUTE FUNCTION tutor_assessment_event_immutable();
`
