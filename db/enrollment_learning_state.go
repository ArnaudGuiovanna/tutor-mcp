// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const sqliteEnrollmentLearningStateMigration = `CREATE UNIQUE INDEX idx_enrollments_tenant_id_version
ON enrollments(tenant_id, id, formation_version_id);
CREATE UNIQUE INDEX idx_formation_concepts_tenant_version_id
ON formation_concepts(tenant_id, formation_version_id, id);
CREATE TABLE learner_concept_states (
    tenant_id            TEXT NOT NULL,
    enrollment_id        TEXT NOT NULL,
    formation_version_id TEXT NOT NULL,
    formation_concept_id TEXT NOT NULL,
    learner_id           TEXT NOT NULL,
    legacy_domain_id     TEXT NOT NULL DEFAULT '',
    legacy_concept       TEXT NOT NULL DEFAULT '',
    stability            REAL NOT NULL DEFAULT 1.0,
    difficulty           REAL NOT NULL DEFAULT 0.3,
    elapsed_days         INTEGER NOT NULL DEFAULT 0,
    scheduled_days       INTEGER NOT NULL DEFAULT 0,
    reps                 INTEGER NOT NULL DEFAULT 0,
    lapses               INTEGER NOT NULL DEFAULT 0,
    card_state           TEXT NOT NULL DEFAULT 'new',
    last_review          DATETIME,
    next_review          DATETIME,
    p_mastery            REAL NOT NULL DEFAULT 0.1,
    p_learn              REAL NOT NULL DEFAULT 0.15,
    p_forget             REAL NOT NULL DEFAULT 0.05,
    p_slip               REAL NOT NULL DEFAULT 0.1,
    p_guess              REAL NOT NULL DEFAULT 0.2,
    theta                REAL NOT NULL DEFAULT 0.0,
    updated_at           DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, enrollment_id, formation_concept_id),
    FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id),
    FOREIGN KEY (tenant_id, enrollment_id, formation_version_id)
        REFERENCES enrollments(tenant_id, id, formation_version_id),
    FOREIGN KEY (tenant_id, formation_version_id, formation_concept_id)
        REFERENCES formation_concepts(tenant_id, formation_version_id, id)
);
CREATE INDEX idx_learner_concept_states_learner
ON learner_concept_states(tenant_id, learner_id, enrollment_id);
CREATE INDEX idx_learner_concept_states_review
ON learner_concept_states(tenant_id, enrollment_id, next_review);
INSERT INTO learner_concept_states (
    tenant_id, enrollment_id, formation_version_id, formation_concept_id,
    learner_id, legacy_domain_id, legacy_concept, stability, difficulty,
    elapsed_days, scheduled_days, reps, lapses, card_state, last_review,
    next_review, p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
SELECT cs.tenant_id, cs.enrollment_id, e.formation_version_id, cs.formation_concept_id,
       cs.learner_id, cs.domain_id, cs.concept, cs.stability, cs.difficulty,
       cs.elapsed_days, cs.scheduled_days, cs.reps, cs.lapses, cs.card_state,
       cs.last_review, cs.next_review, cs.p_mastery, cs.p_learn, cs.p_forget,
       cs.p_slip, cs.p_guess, cs.theta, cs.updated_at
FROM concept_states cs
JOIN enrollments e ON e.tenant_id = cs.tenant_id AND e.id = cs.enrollment_id
JOIN formation_concepts fc ON fc.tenant_id = cs.tenant_id
 AND fc.formation_version_id = e.formation_version_id AND fc.id = cs.formation_concept_id
WHERE cs.enrollment_id <> '' AND cs.formation_concept_id <> '';
CREATE TRIGGER concept_states_sync_enrollment_after_insert
AFTER INSERT ON concept_states WHEN NEW.enrollment_id <> '' AND NEW.formation_concept_id <> ''
BEGIN
    INSERT INTO learner_concept_states (
        tenant_id, enrollment_id, formation_version_id, formation_concept_id,
        learner_id, legacy_domain_id, legacy_concept, stability, difficulty,
        elapsed_days, scheduled_days, reps, lapses, card_state, last_review,
        next_review, p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
    SELECT NEW.tenant_id, NEW.enrollment_id, e.formation_version_id, NEW.formation_concept_id,
        NEW.learner_id, NEW.domain_id, NEW.concept, NEW.stability, NEW.difficulty,
        NEW.elapsed_days, NEW.scheduled_days, NEW.reps, NEW.lapses, NEW.card_state,
        NEW.last_review, NEW.next_review, NEW.p_mastery, NEW.p_learn, NEW.p_forget,
        NEW.p_slip, NEW.p_guess, NEW.theta, NEW.updated_at
    FROM enrollments e JOIN formation_concepts fc
      ON fc.tenant_id = e.tenant_id AND fc.formation_version_id = e.formation_version_id
     AND fc.id = NEW.formation_concept_id
    WHERE e.tenant_id = NEW.tenant_id AND e.id = NEW.enrollment_id
    ON CONFLICT (tenant_id, enrollment_id, formation_concept_id) DO UPDATE SET
        learner_id=excluded.learner_id, legacy_domain_id=excluded.legacy_domain_id,
        legacy_concept=excluded.legacy_concept, stability=excluded.stability,
        difficulty=excluded.difficulty, elapsed_days=excluded.elapsed_days,
        scheduled_days=excluded.scheduled_days, reps=excluded.reps, lapses=excluded.lapses,
        card_state=excluded.card_state, last_review=excluded.last_review,
        next_review=excluded.next_review, p_mastery=excluded.p_mastery,
        p_learn=excluded.p_learn, p_forget=excluded.p_forget, p_slip=excluded.p_slip,
        p_guess=excluded.p_guess, theta=excluded.theta, updated_at=excluded.updated_at;
END;
CREATE TRIGGER concept_states_sync_enrollment_after_update
AFTER UPDATE ON concept_states WHEN NEW.enrollment_id <> '' AND NEW.formation_concept_id <> ''
BEGIN
    INSERT INTO learner_concept_states (
        tenant_id, enrollment_id, formation_version_id, formation_concept_id,
        learner_id, legacy_domain_id, legacy_concept, stability, difficulty,
        elapsed_days, scheduled_days, reps, lapses, card_state, last_review,
        next_review, p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
    SELECT NEW.tenant_id, NEW.enrollment_id, e.formation_version_id, NEW.formation_concept_id,
        NEW.learner_id, NEW.domain_id, NEW.concept, NEW.stability, NEW.difficulty,
        NEW.elapsed_days, NEW.scheduled_days, NEW.reps, NEW.lapses, NEW.card_state,
        NEW.last_review, NEW.next_review, NEW.p_mastery, NEW.p_learn, NEW.p_forget,
        NEW.p_slip, NEW.p_guess, NEW.theta, NEW.updated_at
    FROM enrollments e JOIN formation_concepts fc
      ON fc.tenant_id = e.tenant_id AND fc.formation_version_id = e.formation_version_id
     AND fc.id = NEW.formation_concept_id
    WHERE e.tenant_id = NEW.tenant_id AND e.id = NEW.enrollment_id
    ON CONFLICT (tenant_id, enrollment_id, formation_concept_id) DO UPDATE SET
        learner_id=excluded.learner_id, legacy_domain_id=excluded.legacy_domain_id,
        legacy_concept=excluded.legacy_concept, stability=excluded.stability,
        difficulty=excluded.difficulty, elapsed_days=excluded.elapsed_days,
        scheduled_days=excluded.scheduled_days, reps=excluded.reps, lapses=excluded.lapses,
        card_state=excluded.card_state, last_review=excluded.last_review,
        next_review=excluded.next_review, p_mastery=excluded.p_mastery,
        p_learn=excluded.p_learn, p_forget=excluded.p_forget, p_slip=excluded.p_slip,
        p_guess=excluded.p_guess, theta=excluded.theta, updated_at=excluded.updated_at;
END;`

const postgresEnrollmentLearningStateMigration = `CREATE UNIQUE INDEX IF NOT EXISTS idx_enrollments_tenant_id_version
ON enrollments(tenant_id, id, formation_version_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_formation_concepts_tenant_version_id
ON formation_concepts(tenant_id, formation_version_id, id);
CREATE TABLE learner_concept_states (
    tenant_id TEXT NOT NULL, enrollment_id TEXT NOT NULL, formation_version_id TEXT NOT NULL,
    formation_concept_id TEXT NOT NULL, learner_id TEXT NOT NULL,
    legacy_domain_id TEXT NOT NULL DEFAULT '', legacy_concept TEXT NOT NULL DEFAULT '',
    stability DOUBLE PRECISION NOT NULL DEFAULT 1.0, difficulty DOUBLE PRECISION NOT NULL DEFAULT 0.3,
    elapsed_days INTEGER NOT NULL DEFAULT 0, scheduled_days INTEGER NOT NULL DEFAULT 0,
    reps INTEGER NOT NULL DEFAULT 0, lapses INTEGER NOT NULL DEFAULT 0,
    card_state TEXT NOT NULL DEFAULT 'new', last_review TIMESTAMPTZ, next_review TIMESTAMPTZ,
    p_mastery DOUBLE PRECISION NOT NULL DEFAULT 0.1, p_learn DOUBLE PRECISION NOT NULL DEFAULT 0.15,
    p_forget DOUBLE PRECISION NOT NULL DEFAULT 0.05, p_slip DOUBLE PRECISION NOT NULL DEFAULT 0.1,
    p_guess DOUBLE PRECISION NOT NULL DEFAULT 0.2, theta DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, enrollment_id, formation_concept_id),
    FOREIGN KEY (tenant_id, learner_id) REFERENCES learners(tenant_id, id),
    FOREIGN KEY (tenant_id, enrollment_id, formation_version_id)
        REFERENCES enrollments(tenant_id, id, formation_version_id),
    FOREIGN KEY (tenant_id, formation_version_id, formation_concept_id)
        REFERENCES formation_concepts(tenant_id, formation_version_id, id)
);
CREATE INDEX idx_learner_concept_states_learner
ON learner_concept_states(tenant_id, learner_id, enrollment_id);
CREATE INDEX idx_learner_concept_states_review
ON learner_concept_states(tenant_id, enrollment_id, next_review);
INSERT INTO learner_concept_states (
    tenant_id, enrollment_id, formation_version_id, formation_concept_id,
    learner_id, legacy_domain_id, legacy_concept, stability, difficulty,
    elapsed_days, scheduled_days, reps, lapses, card_state, last_review,
    next_review, p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
SELECT cs.tenant_id, cs.enrollment_id, e.formation_version_id, cs.formation_concept_id,
       cs.learner_id, COALESCE(cs.domain_id, ''), cs.concept, cs.stability, cs.difficulty,
       cs.elapsed_days, cs.scheduled_days, cs.reps, cs.lapses, cs.card_state,
       cs.last_review, cs.next_review, cs.p_mastery, cs.p_learn, cs.p_forget,
       cs.p_slip, cs.p_guess, cs.theta, cs.updated_at
FROM concept_states cs
JOIN enrollments e ON e.tenant_id = cs.tenant_id AND e.id = cs.enrollment_id
JOIN formation_concepts fc ON fc.tenant_id = cs.tenant_id
 AND fc.formation_version_id = e.formation_version_id AND fc.id = cs.formation_concept_id
ON CONFLICT (tenant_id, enrollment_id, formation_concept_id) DO NOTHING;
CREATE OR REPLACE FUNCTION tutor_sync_enrollment_concept_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO learner_concept_states (
        tenant_id, enrollment_id, formation_version_id, formation_concept_id,
        learner_id, legacy_domain_id, legacy_concept, stability, difficulty,
        elapsed_days, scheduled_days, reps, lapses, card_state, last_review,
        next_review, p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
    SELECT NEW.tenant_id, NEW.enrollment_id, e.formation_version_id, NEW.formation_concept_id,
        NEW.learner_id, COALESCE(NEW.domain_id, ''), NEW.concept, NEW.stability, NEW.difficulty,
        NEW.elapsed_days, NEW.scheduled_days, NEW.reps, NEW.lapses, NEW.card_state,
        NEW.last_review, NEW.next_review, NEW.p_mastery, NEW.p_learn, NEW.p_forget,
        NEW.p_slip, NEW.p_guess, NEW.theta, NEW.updated_at
    FROM enrollments e JOIN formation_concepts fc
      ON fc.tenant_id = e.tenant_id AND fc.formation_version_id = e.formation_version_id
     AND fc.id = NEW.formation_concept_id
    WHERE e.tenant_id = NEW.tenant_id AND e.id = NEW.enrollment_id
    ON CONFLICT (tenant_id, enrollment_id, formation_concept_id) DO UPDATE SET
        learner_id=excluded.learner_id, legacy_domain_id=excluded.legacy_domain_id,
        legacy_concept=excluded.legacy_concept, stability=excluded.stability,
        difficulty=excluded.difficulty, elapsed_days=excluded.elapsed_days,
        scheduled_days=excluded.scheduled_days, reps=excluded.reps, lapses=excluded.lapses,
        card_state=excluded.card_state, last_review=excluded.last_review,
        next_review=excluded.next_review, p_mastery=excluded.p_mastery,
        p_learn=excluded.p_learn, p_forget=excluded.p_forget, p_slip=excluded.p_slip,
        p_guess=excluded.p_guess, theta=excluded.theta, updated_at=excluded.updated_at;
    RETURN NEW;
END $$;
CREATE TRIGGER concept_states_sync_enrollment AFTER INSERT OR UPDATE ON concept_states
FOR EACH ROW EXECUTE FUNCTION tutor_sync_enrollment_concept_state();
ALTER TABLE learner_concept_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE learner_concept_states FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON learner_concept_states
USING (tenant_id = current_setting('app.current_tenant', true))
WITH CHECK (tenant_id = current_setting('app.current_tenant', true));`

func validateEnrollmentLearningScope(scope models.TenantScope, conceptID string) error {
	if err := scope.Validate(); err != nil || scope.EnrollmentID == "" || scope.LearnerID == "" || conceptID == "" {
		return fmt.Errorf("enrollment learning scope is incomplete")
	}
	return nil
}

// UpsertEnrollmentConceptState is the canonical M3 write boundary. The two
// composite foreign keys make an enrollment/concept pair from different
// formation versions impossible even through direct SQL.
func (s *Store) UpsertEnrollmentConceptState(ctx context.Context, scope models.TenantScope, conceptID string, state *models.ConceptState) error {
	if err := validateEnrollmentLearningScope(scope, conceptID); err != nil || state == nil {
		return fmt.Errorf("upsert enrollment concept state: invalid input")
	}
	if state.LearnerID != "" && state.LearnerID != scope.LearnerID {
		return fmt.Errorf("upsert enrollment concept state: learner mismatch")
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now().UTC()
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var versionID string
		if err := txs.queryRow(txCtx, `SELECT e.formation_version_id
			FROM enrollments e JOIN formation_concepts c
			  ON c.tenant_id = e.tenant_id AND c.formation_version_id = e.formation_version_id
			WHERE e.tenant_id = ? AND e.id = ? AND e.user_id = ? AND e.membership_id = ?
			  AND e.learner_id = ? AND e.status IN ('active','completed') AND c.id = ?`,
			scope.TenantID, scope.EnrollmentID, scope.UserID, scope.MembershipID,
			scope.LearnerID, conceptID).Scan(&versionID); err != nil {
			return fmt.Errorf("resolve enrollment concept: %w", err)
		}
		_, err := txs.exec(txCtx, `INSERT INTO learner_concept_states
			(tenant_id, enrollment_id, formation_version_id, formation_concept_id,
			 learner_id, legacy_domain_id, legacy_concept, stability, difficulty,
			 elapsed_days, scheduled_days, reps, lapses, card_state, last_review,
			 next_review, p_mastery, p_learn, p_forget, p_slip, p_guess, theta, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, enrollment_id, formation_concept_id) DO UPDATE SET
			 stability=excluded.stability, difficulty=excluded.difficulty,
			 elapsed_days=excluded.elapsed_days, scheduled_days=excluded.scheduled_days,
			 reps=excluded.reps, lapses=excluded.lapses, card_state=excluded.card_state,
			 last_review=excluded.last_review, next_review=excluded.next_review,
			 p_mastery=excluded.p_mastery, p_learn=excluded.p_learn,
			 p_forget=excluded.p_forget, p_slip=excluded.p_slip,
			 p_guess=excluded.p_guess, theta=excluded.theta, updated_at=excluded.updated_at`,
			scope.TenantID, scope.EnrollmentID, versionID, conceptID, scope.LearnerID,
			state.DomainID, state.Concept, state.Stability, state.Difficulty,
			state.ElapsedDays, state.ScheduledDays, state.Reps, state.Lapses,
			state.CardState, state.LastReview, state.NextReview, state.PMastery,
			state.PLearn, state.PForget, state.PSlip, state.PGuess, state.Theta,
			state.UpdatedAt.UTC())
		return err
	})
}

func (s *Store) GetEnrollmentConceptState(ctx context.Context, scope models.TenantScope, conceptID string) (*models.ConceptState, error) {
	if err := validateEnrollmentLearningScope(scope, conceptID); err != nil {
		return nil, fmt.Errorf("get enrollment concept state: %w", err)
	}
	var state *models.ConceptState
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		candidate := &models.ConceptState{
			TenantID: scope.TenantID, EnrollmentID: scope.EnrollmentID,
			FormationConceptID: conceptID, LearnerID: scope.LearnerID,
		}
		var lastReview, nextReview sql.NullTime
		err := txs.queryRow(txCtx, `SELECT state.legacy_domain_id, state.legacy_concept,
			state.stability, state.difficulty, state.elapsed_days, state.scheduled_days,
			state.reps, state.lapses, state.card_state, state.last_review, state.next_review,
			state.p_mastery, state.p_learn, state.p_forget, state.p_slip, state.p_guess,
			state.theta, state.updated_at
			FROM learner_concept_states state JOIN enrollments e
			  ON e.tenant_id = state.tenant_id AND e.id = state.enrollment_id
			WHERE state.tenant_id = ? AND state.enrollment_id = ?
			  AND state.formation_concept_id = ? AND state.learner_id = ?
			  AND e.user_id = ? AND e.membership_id = ?`, scope.TenantID,
			scope.EnrollmentID, conceptID, scope.LearnerID, scope.UserID, scope.MembershipID).
			Scan(&candidate.DomainID, &candidate.Concept, &candidate.Stability,
				&candidate.Difficulty, &candidate.ElapsedDays, &candidate.ScheduledDays,
				&candidate.Reps, &candidate.Lapses, &candidate.CardState, &lastReview,
				&nextReview, &candidate.PMastery, &candidate.PLearn, &candidate.PForget,
				&candidate.PSlip, &candidate.PGuess, &candidate.Theta, &candidate.UpdatedAt)
		if err != nil {
			return err
		}
		if lastReview.Valid {
			candidate.LastReview = &lastReview.Time
		}
		if nextReview.Valid {
			candidate.NextReview = &nextReview.Time
		}
		state = candidate
		return nil
	})
	return state, err
}
