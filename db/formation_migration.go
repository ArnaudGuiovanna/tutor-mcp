// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (s *Store) MigrateFormationEnrollment(ctx context.Context, actor models.Principal, sourceID, targetCohortID string) (*models.FormationMigration, error) {
	var out *models.FormationMigration
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		targetFormation, err := txs.formationAccess(txCtx, actor, "cohort", targetCohortID, models.PermissionCohortManage, true)
		if err != nil {
			return err
		}
		if targetFormation.Status != "active" {
			return storeport.ErrFormationVersionImmutable
		}
		var source models.Enrollment
		source.ID, source.TenantID = sourceID, actor.TenantID
		if err := txs.queryRow(txCtx, `SELECT cohort_id, formation_version_id, user_id, membership_id, COALESCE(learner_id, ''), status, objectives_json, seat_reserved
		 FROM enrollments WHERE tenant_id = ? AND id = ?`, actor.TenantID, sourceID).Scan(&source.CohortID, &source.FormationVersionID, &source.UserID, &source.MembershipID, &source.LearnerID, &source.Status, &source.ObjectivesJSON, &source.SeatReserved); err != nil {
			return err
		}
		sourceFormation, err := txs.formationAccess(txCtx, actor, "cohort", source.CohortID, models.PermissionCohortManage, false)
		if err != nil {
			return err
		}
		if sourceFormation.ID != targetFormation.ID {
			return fmt.Errorf("migration requires two versions of the same formation")
		}
		var targetID, domainID string
		var responseJSON string
		err = txs.queryRow(txCtx, `SELECT target_enrollment_id, domain_id, response_json FROM enrollment_migrations WHERE tenant_id = ? AND source_enrollment_id = ?`, actor.TenantID, sourceID).Scan(&targetID, &domainID, &responseJSON)
		if err == nil {
			var cohortID string
			if err := txs.queryRow(txCtx, `SELECT cohort_id FROM enrollments WHERE tenant_id = ? AND id = ?`, actor.TenantID, targetID).Scan(&cohortID); err != nil {
				return err
			}
			if cohortID != targetCohortID {
				return fmt.Errorf("enrollment already migrated to another cohort")
			}
			return json.Unmarshal([]byte(responseJSON), &out)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if source.Status != "active" || !source.SeatReserved || source.LearnerID == "" {
			return fmt.Errorf("only an active enrollment can migrate")
		}
		var targetVersionID string
		if err := txs.queryRow(txCtx, `SELECT formation_version_id FROM cohorts WHERE tenant_id = ? AND id = ?`, actor.TenantID, targetCohortID).Scan(&targetVersionID); err != nil {
			return err
		}
		if targetVersionID == source.FormationVersionID {
			return fmt.Errorf("choose a cohort on a different published version")
		}
		detail, err := txs.loadFormationVersion(txCtx, actor.TenantID, targetVersionID)
		if err != nil {
			return err
		}
		if detail.Version.Status != "published" {
			return storeport.ErrFormationVersionImmutable
		}
		q := `SELECT id FROM domains WHERE tenant_id = ? AND learner_id = ? AND formation_enrollment_id = ? AND deleted_at IS NULL`
		if txs.dialect == DialectPostgres {
			q += " FOR UPDATE"
		}
		if err := txs.queryRow(txCtx, q, actor.TenantID, source.LearnerID, sourceID).Scan(&domainID); err != nil {
			return err
		}
		previous, err := txs.getCurriculumSnapshot(txCtx, source.LearnerID, domainID, 0)
		if err != nil {
			return err
		}
		// Reconciliation changes the current domain's estimates. Preserve the
		// source enrollment's pre-migration state for authorized historical reads.
		previousStates, err := txs.formationEnrollmentStates(txCtx, source, domainID)
		if err != nil {
			return err
		}
		for _, state := range previousStates {
			current := *state // Upsert updates UpdatedAt; retain the snapshot's date.
			if err := txs.UpsertConceptState(txCtx, &current); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		next, err := formationCurriculum(detail, domainID, actor.UserID, now)
		if err != nil {
			return err
		}
		next.Operation = models.CurriculumOperation{Type: models.CurriculumOperationLegacyUpdate, Rationale: "explicit formation version migration by authorized staff"}
		active := map[string]models.CurriculumConcept{}
		for _, c := range next.Concepts {
			active[c.Key] = c
		}
		invalidated := []string{}
		for _, old := range previous.Concepts {
			updated, exists := active[old.Key]
			if !exists {
				old.Status = models.CurriculumConceptRetired
				next.Concepts = append(next.Concepts, old)
			}
			if !exists || updated.Status != old.Status || !models.SameCurriculumDefinition(old, updated) {
				invalidated = append(invalidated, old.Key)
			}
		}
		if err := s.compareAndSwapCurriculum(txCtx, source.LearnerID, domainID, previous.Version, next); err != nil {
			return err
		}
		for _, state := range previousStates {
			if _, err := txs.exec(txCtx, `UPDATE learner_concept_states SET
 stability = ?, difficulty = ?, elapsed_days = ?, scheduled_days = ?, reps = ?, lapses = ?, card_state = ?,
 last_review = ?, next_review = ?, p_mastery = ?, p_learn = ?, p_forget = ?, p_slip = ?, p_guess = ?, theta = ?, updated_at = ?
 WHERE tenant_id = ? AND enrollment_id = ? AND formation_concept_id = ?`,
				state.Stability, state.Difficulty, state.ElapsedDays, state.ScheduledDays, state.Reps, state.Lapses, state.CardState,
				state.LastReview, state.NextReview, state.PMastery, state.PLearn, state.PForget, state.PSlip, state.PGuess, state.Theta, state.UpdatedAt,
				actor.TenantID, sourceID, state.FormationConceptID); err != nil {
				return err
			}
		}
		res, err := txs.exec(txCtx, `UPDATE cohorts SET reserved_seats = reserved_seats + 1, version = version + 1, updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND status = 'open' AND reserved_seats < capacity`, now, actor.TenantID, targetCohortID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return storeport.ErrCohortCapacityReached
		}
		targetID, err = generateID()
		if err != nil {
			return err
		}
		target := source
		target.ID, target.CohortID, target.FormationVersionID, target.DomainID = targetID, targetCohortID, targetVersionID, domainID
		target.CreatedAt, target.UpdatedAt = now, now
		if _, err := txs.exec(txCtx, `INSERT INTO enrollments (id, tenant_id, cohort_id, formation_version_id, user_id, membership_id, learner_id, status, objectives_json, seat_reserved, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, 1, ?, ?)`, targetID, actor.TenantID, targetCohortID, targetVersionID, source.UserID, source.MembershipID, source.LearnerID, source.ObjectivesJSON, now, now); err != nil {
			return err
		}
		if err := txs.mapFormationDomain(txCtx, &target, domainID, detail); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE domains SET formation_enrollment_id = ?, formation_version_id = ? WHERE tenant_id = ? AND id = ?`, targetID, targetVersionID, actor.TenantID, domainID); err != nil {
			return err
		}
		// Keep historical observations attached to their original enrollment.
		// Only current estimates project onto the new version's shared concepts.
		for _, concept := range detail.Concepts {
			state, err := txs.GetConceptStateInDomain(txCtx, source.LearnerID, domainID, concept.StableKey)
			if errors.Is(err, sql.ErrNoRows) {
				state = models.NewConceptStateInDomain(source.LearnerID, domainID, concept.StableKey)
			} else if err != nil {
				return err
			}
			if err := txs.UpsertConceptState(txCtx, state); err != nil {
				return err
			}
		}
		if _, err := txs.exec(txCtx, `UPDATE learning_sessions SET status = 'closed', closed_at = ? WHERE tenant_id = ? AND learner_id = ? AND domain_id = ? AND status = 'open'`, now, actor.TenantID, source.LearnerID, domainID); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE enrollments SET status = 'cancelled', seat_reserved = 0, updated_at = ? WHERE tenant_id = ? AND id = ?`, now, actor.TenantID, sourceID); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE cohorts SET reserved_seats = reserved_seats - 1, version = version + 1, updated_at = ? WHERE tenant_id = ? AND id = ? AND reserved_seats > 0`, now, actor.TenantID, source.CohortID); err != nil {
			return err
		}
		out = &models.FormationMigration{SourceEnrollmentID: sourceID, Enrollment: target, InvalidatedConceptKeys: invalidated}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `INSERT INTO enrollment_migrations (tenant_id, source_enrollment_id, target_enrollment_id, domain_id, actor_membership_id, created_at, response_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`, actor.TenantID, sourceID, targetID, domainID, actor.MembershipID, now, string(raw)); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "enrollment.migrate", TargetType: "enrollment", TargetID: sourceID, DetailsJSON: `{"target_enrollment_id":` + strconvQuote(targetID) + `,"target_version_id":` + strconvQuote(targetVersionID) + `}`})
	})
	return out, err
}

// Read the canonical enrollment snapshot before changing compatibility-domain
// estimates. The published concept supplies the stable key, never a label from
// another enrollment or a mutable domain mapping.
func (s *Store) formationEnrollmentStates(ctx context.Context, enrollment models.Enrollment, domainID string) ([]*models.ConceptState, error) {
	rows, err := s.query(ctx, `SELECT fc.id, fc.stable_key, s.stability, s.difficulty, s.elapsed_days,
 s.scheduled_days, s.reps, s.lapses, s.card_state, s.last_review, s.next_review,
 s.p_mastery, s.p_learn, s.p_forget, s.p_slip, s.p_guess, s.theta, s.updated_at
 FROM learner_concept_states s JOIN formation_concepts fc ON fc.tenant_id = s.tenant_id
 AND fc.id = s.formation_concept_id AND fc.formation_version_id = s.formation_version_id
 WHERE s.tenant_id = ? AND s.enrollment_id = ? AND s.learner_id = ?`, enrollment.TenantID, enrollment.ID, enrollment.LearnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []*models.ConceptState{}
	for rows.Next() {
		state := &models.ConceptState{LearnerID: enrollment.LearnerID, DomainID: domainID}
		var last, next sql.NullTime
		if err := rows.Scan(&state.FormationConceptID, &state.Concept, &state.Stability, &state.Difficulty,
			&state.ElapsedDays, &state.ScheduledDays, &state.Reps, &state.Lapses, &state.CardState, &last, &next,
			&state.PMastery, &state.PLearn, &state.PForget, &state.PSlip, &state.PGuess, &state.Theta, &state.UpdatedAt); err != nil {
			return nil, err
		}
		if last.Valid {
			state.LastReview = &last.Time
		}
		if next.Valid {
			state.NextReview = &next.Time
		}
		states = append(states, state)
	}
	return states, rows.Err()
}
