// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const adjudicationSchema = `
CREATE UNIQUE INDEX idx_assessment_review_tenant_id ON assessment_reviews(tenant_id, id);
CREATE TABLE assessment_adjudications (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id),
 learner_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 review_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision > 0),
 verdict TEXT NOT NULL CHECK (verdict IN ('accept', 'reject')),
 authority_id TEXT NOT NULL,
 key_id TEXT NOT NULL,
 certificate_id TEXT NOT NULL,
 certificate_hash TEXT NOT NULL CHECK (length(certificate_hash) = 64),
 certificate_json TEXT NOT NULL,
 authority_public_key TEXT NOT NULL,
 actor_user_id TEXT NOT NULL REFERENCES users(id),
 material_hash TEXT NOT NULL CHECK (length(material_hash) = 64),
 score_hash TEXT NOT NULL CHECK (length(score_hash) = 64),
 created_at TIMESTAMP NOT NULL,
 FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id),
 FOREIGN KEY (tenant_id, attempt_id) REFERENCES assessment_attempts(tenant_id, id) ON DELETE CASCADE,
 FOREIGN KEY (tenant_id, review_id) REFERENCES assessment_reviews(tenant_id, id) ON DELETE CASCADE,
 UNIQUE (tenant_id, attempt_id, revision),
 UNIQUE (tenant_id, authority_id, certificate_id)
);
CREATE INDEX idx_adjudication_learner ON assessment_adjudications(tenant_id, learner_id, created_at);
`

const adjudicationScope = `NOT EXISTS (SELECT 1 FROM assessment_reviews r
 WHERE r.id = NEW.review_id AND r.tenant_id = NEW.tenant_id AND r.learner_id = NEW.learner_id
 AND r.attempt_id = NEW.attempt_id AND r.material_hash = NEW.material_hash AND r.rubric_score_hash = NEW.score_hash)`

const sqliteAdjudicationMigration = adjudicationSchema + `
CREATE TRIGGER assessment_adjudications_immutable BEFORE UPDATE ON assessment_adjudications
 BEGIN SELECT RAISE(ABORT, 'assessment adjudications are immutable'); END;
CREATE TRIGGER assessment_adjudications_scope BEFORE INSERT ON assessment_adjudications
 WHEN ` + adjudicationScope + `
 BEGIN SELECT RAISE(ABORT, 'assessment adjudication scope mismatch'); END;
`

const postgresAdjudicationMigration = adjudicationSchema + `
ALTER TABLE assessment_adjudications ENABLE ROW LEVEL SECURITY;
ALTER TABLE assessment_adjudications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON assessment_adjudications
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
CREATE FUNCTION tutor_immutable_adjudication() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'assessment adjudications are immutable'; END; $$;
CREATE TRIGGER assessment_adjudications_immutable BEFORE UPDATE ON assessment_adjudications
 FOR EACH ROW EXECUTE FUNCTION tutor_immutable_adjudication();
CREATE FUNCTION tutor_adjudication_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ` + adjudicationScope + ` THEN RAISE EXCEPTION 'assessment adjudication scope mismatch'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER assessment_adjudications_scope BEFORE INSERT ON assessment_adjudications
 FOR EACH ROW EXECUTE FUNCTION tutor_adjudication_scope();
`
