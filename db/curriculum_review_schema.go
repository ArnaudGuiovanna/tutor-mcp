// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const curriculumReviewSchema = `
CREATE TABLE curriculum_review_opinions (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id),
 learner_id TEXT NOT NULL,
 domain_id TEXT NOT NULL REFERENCES domains(id),
 version INTEGER NOT NULL,
 reviewer_user_id TEXT NOT NULL REFERENCES users(id),
 reviewer_membership_id TEXT NOT NULL,
 idempotency_key TEXT NOT NULL,
 material_hash TEXT NOT NULL,
 findings_hash TEXT NOT NULL,
 findings_json TEXT,
 disposition TEXT NOT NULL CHECK (disposition IN ('partial_opinion','complete_opinion','changes_requested')),
 reviewed_sections INTEGER NOT NULL CHECK (reviewed_sections >= 0),
 total_sections INTEGER NOT NULL CHECK (total_sections >= reviewed_sections),
 created_at TIMESTAMP NOT NULL,
 FOREIGN KEY (tenant_id,learner_id) REFERENCES learners(tenant_id,id),
 FOREIGN KEY (tenant_id,reviewer_membership_id) REFERENCES tenant_memberships(tenant_id,id),
 FOREIGN KEY (domain_id,version) REFERENCES curriculum_versions(domain_id,version),
 UNIQUE (tenant_id,reviewer_user_id,idempotency_key),
 UNIQUE (tenant_id,domain_id,version,reviewer_user_id)
);
CREATE INDEX idx_curriculum_review_learner ON curriculum_review_opinions(tenant_id,learner_id,created_at);
`

const curriculumReviewScope = `NOT EXISTS (SELECT 1 FROM domains d JOIN learners l ON l.id = d.learner_id AND l.tenant_id = d.tenant_id
 JOIN tenant_memberships m ON m.id = NEW.reviewer_membership_id AND m.tenant_id = d.tenant_id
 WHERE d.id = NEW.domain_id AND d.tenant_id = NEW.tenant_id AND d.learner_id = NEW.learner_id
 AND m.user_id = NEW.reviewer_user_id AND l.user_id <> NEW.reviewer_user_id)`

const curriculumReviewImmutable = `NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id OR NEW.learner_id <> OLD.learner_id
 OR NEW.domain_id <> OLD.domain_id OR NEW.version <> OLD.version OR NEW.reviewer_user_id <> OLD.reviewer_user_id
 OR NEW.reviewer_membership_id <> OLD.reviewer_membership_id OR NEW.idempotency_key <> OLD.idempotency_key
 OR NEW.material_hash <> OLD.material_hash OR NEW.findings_hash <> OLD.findings_hash OR NEW.disposition <> OLD.disposition
 OR NEW.reviewed_sections <> OLD.reviewed_sections OR NEW.total_sections <> OLD.total_sections OR NEW.created_at <> OLD.created_at
 OR (NEW.findings_json IS NOT NULL AND (OLD.findings_json IS NULL OR NEW.findings_json <> OLD.findings_json))`

const sqliteCurriculumReviewMigration = curriculumReviewSchema + `
CREATE TRIGGER curriculum_review_opinions_scope BEFORE INSERT ON curriculum_review_opinions WHEN ` + curriculumReviewScope + `
 BEGIN SELECT RAISE(ABORT,'curriculum review scope mismatch'); END;
CREATE TRIGGER curriculum_review_opinions_immutable BEFORE UPDATE ON curriculum_review_opinions WHEN ` + curriculumReviewImmutable + `
 BEGIN SELECT RAISE(ABORT,'curriculum opinions are immutable except plaintext redaction'); END;
`

const postgresCurriculumReviewMigration = curriculumReviewSchema + `
ALTER TABLE curriculum_review_opinions ENABLE ROW LEVEL SECURITY;
ALTER TABLE curriculum_review_opinions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON curriculum_review_opinions
 USING (tenant_id = current_setting('app.current_tenant',true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant',true));
CREATE FUNCTION tutor_curriculum_review_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ` + curriculumReviewScope + ` THEN RAISE EXCEPTION 'curriculum review scope mismatch'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER curriculum_review_opinions_scope BEFORE INSERT ON curriculum_review_opinions FOR EACH ROW EXECUTE FUNCTION tutor_curriculum_review_scope();
CREATE FUNCTION tutor_curriculum_review_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ` + curriculumReviewImmutable + ` THEN RAISE EXCEPTION 'curriculum opinions are immutable except plaintext redaction'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER curriculum_review_opinions_immutable BEFORE UPDATE ON curriculum_review_opinions FOR EACH ROW EXECUTE FUNCTION tutor_curriculum_review_immutable();
`
