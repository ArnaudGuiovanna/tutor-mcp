// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

const assessmentReviewSchema = `
CREATE UNIQUE INDEX idx_assessment_attempt_tenant_id ON assessment_attempts(tenant_id, id);
CREATE TABLE assessment_reviews (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id),
 learner_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 reviewer_user_id TEXT NOT NULL REFERENCES users(id),
 reviewer_membership_id TEXT NOT NULL,
 reviewer_token_version BIGINT NOT NULL CHECK (reviewer_token_version >= 1),
 idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
 material_hash TEXT NOT NULL CHECK (length(material_hash) = 64),
 rubric_score_hash TEXT NOT NULL CHECK (length(rubric_score_hash) = 64),
 rubric_score_json TEXT,
 total DOUBLE PRECISION NOT NULL CHECK (total >= 0),
 passed INTEGER NOT NULL CHECK (passed IN (0, 1)),
 trusted_evaluation INTEGER NOT NULL DEFAULT 0 CHECK (trusted_evaluation = 0),
 created_at TIMESTAMP NOT NULL,
 FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id),
 FOREIGN KEY (tenant_id, attempt_id) REFERENCES assessment_attempts(tenant_id, id) ON DELETE CASCADE,
 FOREIGN KEY (tenant_id, reviewer_membership_id) REFERENCES tenant_memberships(tenant_id, id),
 UNIQUE (tenant_id, reviewer_user_id, idempotency_key),
 UNIQUE (tenant_id, attempt_id, reviewer_user_id)
);
CREATE INDEX idx_assessment_reviews_learner ON assessment_reviews(tenant_id, learner_id, created_at);
`

const assessmentReviewImmutable = `NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
 OR NEW.learner_id <> OLD.learner_id OR NEW.attempt_id <> OLD.attempt_id
 OR NEW.reviewer_user_id <> OLD.reviewer_user_id OR NEW.reviewer_membership_id <> OLD.reviewer_membership_id
 OR NEW.reviewer_token_version <> OLD.reviewer_token_version OR NEW.idempotency_key <> OLD.idempotency_key
 OR NEW.material_hash <> OLD.material_hash OR NEW.rubric_score_hash <> OLD.rubric_score_hash
 OR NEW.total <> OLD.total OR NEW.passed <> OLD.passed OR NEW.trusted_evaluation <> OLD.trusted_evaluation
 OR NEW.created_at <> OLD.created_at
 OR (NEW.rubric_score_json IS NOT NULL AND (OLD.rubric_score_json IS NULL OR NEW.rubric_score_json <> OLD.rubric_score_json))`

const assessmentReviewScope = `NOT EXISTS (
 SELECT 1 FROM assessment_attempts a JOIN learners l ON l.id = a.learner_id AND l.tenant_id = a.tenant_id
 JOIN tenant_memberships m ON m.id = NEW.reviewer_membership_id AND m.tenant_id = a.tenant_id
 WHERE a.id = NEW.attempt_id AND a.tenant_id = NEW.tenant_id AND a.learner_id = NEW.learner_id
 AND m.user_id = NEW.reviewer_user_id AND l.user_id <> NEW.reviewer_user_id)`

const sqliteAssessmentReviewMigration = assessmentReviewSchema + `
CREATE TRIGGER assessment_reviews_immutable BEFORE UPDATE ON assessment_reviews
 WHEN ` + assessmentReviewImmutable + `
 BEGIN SELECT RAISE(ABORT, 'assessment reviews are immutable except plaintext redaction'); END;
CREATE TRIGGER assessment_reviews_scope BEFORE INSERT ON assessment_reviews
 WHEN ` + assessmentReviewScope + `
 BEGIN SELECT RAISE(ABORT, 'assessment review scope mismatch'); END;
`

const postgresAssessmentReviewMigration = assessmentReviewSchema + `
ALTER TABLE assessment_reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE assessment_reviews FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON assessment_reviews
 USING (tenant_id = current_setting('app.current_tenant', true))
 WITH CHECK (tenant_id = current_setting('app.current_tenant', true));
CREATE FUNCTION tutor_immutable_assessment_review() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ` + assessmentReviewImmutable + ` THEN RAISE EXCEPTION 'assessment reviews are immutable except plaintext redaction'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER assessment_reviews_immutable BEFORE UPDATE ON assessment_reviews
 FOR EACH ROW EXECUTE FUNCTION tutor_immutable_assessment_review();
CREATE FUNCTION tutor_assessment_review_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ` + assessmentReviewScope + ` THEN RAISE EXCEPTION 'assessment review scope mismatch'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER assessment_reviews_scope BEFORE INSERT ON assessment_reviews
 FOR EACH ROW EXECUTE FUNCTION tutor_assessment_review_scope();
`
