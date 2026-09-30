// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type badgeEnrollment struct{ tenant, learner, enrollment, version, name string }

// Called inside the observation/adjudication transaction, after the domain lock.
// It never awards from estimates or from another enrollment's carried state.
func (s *Store) awardInstitutionBadges(ctx context.Context, learner, domain, concept string, now time.Time) error {
	var e badgeEnrollment
	err := s.queryRow(ctx, `SELECT e.tenant_id, e.learner_id, e.id, e.formation_version_id, f.name
 FROM domains d JOIN enrollments e ON e.tenant_id = d.tenant_id AND e.id = d.formation_enrollment_id AND e.learner_id = d.learner_id
 JOIN formation_versions v ON v.tenant_id = e.tenant_id AND v.id = e.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 WHERE d.id = ? AND d.learner_id = ? AND d.deleted_at IS NULL AND e.status = 'active'
 AND v.status = 'published' AND `+institutionalFormationSQL, domain, learner).Scan(&e.tenant, &e.learner, &e.enrollment, &e.version, &e.name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	// Earliest qualifying challenge for each published concept, under the same
	// effective trust policy as the mastery read model (including adjudication).
	rows, err := s.query(ctx, `SELECT a.id, a.formation_concept_id, fc.label, a.submitted_at,
 COALESCE(j.created_at, a.evaluated_at), CASE WHEN j.verdict = 'accept' THEN 'external_service' ELSE a.evaluation_method END
 FROM `+assessmentEvidenceFrom+`
 JOIN formation_concepts fc ON fc.tenant_id = a.tenant_id AND fc.id = a.formation_concept_id
 WHERE a.tenant_id = ? AND a.enrollment_id = ? AND a.learner_id = ? AND fc.formation_version_id = ?
 AND a.activity_type = ? AND a.status = 'evaluated' AND a.curriculum_invalidated_version = 0
 AND a.decision_id IS NOT NULL AND a.decision_id <> ''
 AND a.created_at <= a.submitted_at AND a.submitted_at <= a.evaluated_at
 AND COALESCE(j.created_at, a.evaluated_at) <= ? AND `+assessmentEffectivePassed+` = 1
 AND (`+s.assessmentEffectiveTrust()+`) = 1
 ORDER BY a.formation_concept_id, COALESCE(j.created_at, a.evaluated_at), a.id`,
		e.tenant, e.enrollment, learner, e.version, string(models.ActivityMasteryChallenge), now)
	if err != nil {
		return err
	}
	type candidate struct {
		label string
		proof models.BadgeEvidence
	}
	var mastery []candidate
	seen := map[string]bool{}
	for rows.Next() {
		var c candidate
		var responded, evaluated flexTime
		if err := rows.Scan(&c.proof.AttemptID, &c.proof.ConceptID, &c.label, &responded, &evaluated, &c.proof.EvaluationMethod); err != nil {
			rows.Close()
			return err
		}
		if !responded.Valid || !evaluated.Valid || seen[c.proof.ConceptID] {
			continue
		}
		seen[c.proof.ConceptID] = true
		c.proof.RespondedAt, c.proof.EvaluatedAt = responded.Time, evaluated.Time
		mastery = append(mastery, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	proofs := []models.BadgeEvidence{}
	var completed time.Time
	for _, c := range mastery {
		if err := s.insertBadge(ctx, e, models.BadgeMastery, c.proof.ConceptID, c.label, 0, c.proof.EvaluatedAt, now, []models.BadgeEvidence{c.proof}); err != nil {
			return err
		}
		proofs = append(proofs, c.proof)
		if c.proof.EvaluatedAt.After(completed) {
			completed = c.proof.EvaluatedAt
		}
	}
	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM formation_concepts WHERE tenant_id = ? AND formation_version_id = ?`, e.tenant, e.version).Scan(&total); err != nil {
		return err
	}
	if total > 0 && total == len(proofs) {
		if err := s.insertBadge(ctx, e, models.BadgeFormation, "", e.name, 0, completed, now, proofs); err != nil {
			return err
		}
	}
	return s.awardRetentionBadges(ctx, e, domain, concept, now)
}

func (s *Store) insertBadge(ctx context.Context, e badgeEnrollment, kind, concept, label string, days int, achieved, now time.Time, evidence []models.BadgeEvidence) error {
	id := stableLearningID("badge_", e.tenant, e.enrollment, concept, kind, fmt.Sprint(days), models.BadgePolicyVersion)
	result, err := s.exec(ctx, `INSERT INTO learning_badges
 (id, tenant_id, learner_id, enrollment_id, formation_version_id, concept_id, label, kind, retention_days, policy_version, achieved_at, recorded_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, id, e.tenant, e.learner, e.enrollment, e.version, concept, label, kind, days, models.BadgePolicyVersion, achieved.UTC(), now.UTC())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	for _, proof := range evidence {
		if _, err := s.exec(ctx, `INSERT INTO learning_badge_evidence
 (tenant_id, badge_id, learner_id, attempt_id, concept_id, snapshot_id, responded_at, evaluated_at, evaluation_method)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.tenant, id, e.learner, proof.AttemptID, proof.ConceptID, proof.SnapshotID, proof.RespondedAt.UTC(), proof.EvaluatedAt.UTC(), string(proof.EvaluationMethod)); err != nil {
			return err
		}
	}
	return nil
}

type retentionBadgeObservation struct {
	proof                                                      models.BadgeEvidence
	activity, beforeJSON, observationJSON, afterJSON, protocol string
	priorExposure                                              *time.Time
	passed                                                     bool
	hints                                                      int
	valid                                                      bool
}

// Use a bounded suffix of observations. Truncation can only delay an award;
// it cannot invent an earlier start or hide a failure inside the retained run.
func (s *Store) awardRetentionBadges(ctx context.Context, e badgeEnrollment, domain, concept string, now time.Time) error {
	// The inherited FSRS card can predate a version migration. Its old review
	// must not lengthen a new enrollment's achievement period.
	var earliest flexTime
	if err := s.queryRow(ctx, `SELECT MIN(submitted_at) FROM assessment_attempts
 WHERE tenant_id = ? AND enrollment_id = ? AND learner_id = ? AND domain_id = ? AND concept_id = ?
 AND status = 'evaluated' AND curriculum_invalidated_version = 0 AND evaluated_at <= ?`,
		e.tenant, e.enrollment, e.learner, domain, concept, now).Scan(&earliest); err != nil {
		return err
	}
	if !earliest.Valid {
		return nil
	}
	rows, err := s.query(ctx, `SELECT COALESCE(ps.id, 0), COALESCE(ps.before_json, '{}'), COALESCE(ps.observation_json, '{}'), COALESCE(ps.after_json, '{}'),
 i.activity_type, CASE WHEN a.id IS NULL THEN i.success WHEN j.verdict = 'reject' THEN 0 ELSE `+assessmentEffectivePassed+` END,
 i.hints_requested, COALESCE(a.id, ''), i.formation_concept_id,
 COALESCE(a.submitted_at, i.created_at), COALESCE(j.created_at, a.evaluated_at), CASE WHEN j.verdict = 'accept' THEN 'external_service' ELSE COALESCE(a.evaluation_method, '') END,
 COALESCE(a.event_protocol, ''), a.prior_exposure_at,
 CASE WHEN a.status = 'evaluated' AND a.curriculum_invalidated_version = 0 AND i.curriculum_invalidated_version = 0
 AND a.created_at <= a.submitted_at AND a.submitted_at <= a.evaluated_at
 AND COALESCE(j.created_at, a.evaluated_at) <= ? AND `+assessmentEffectivePassed+` = 1
 AND (j.id IS NULL OR j.verdict = 'accept') THEN 1 ELSE 0 END,
 COALESCE(fc.label, '')
 FROM interactions i LEFT JOIN pedagogical_snapshots ps ON ps.interaction_id = i.id AND ps.learner_id = i.learner_id
 JOIN formation_concepts fc ON fc.tenant_id = i.tenant_id AND fc.id = i.formation_concept_id
 LEFT JOIN assessment_attempts a ON a.id = i.assessment_attempt_id AND a.tenant_id = i.tenant_id
 AND a.enrollment_id = i.enrollment_id AND a.learner_id = i.learner_id AND a.concept_id = i.concept AND a.activity_type = i.activity_type
 LEFT JOIN assessment_adjudications j ON j.tenant_id = a.tenant_id AND j.attempt_id = a.id
 AND j.revision = (SELECT MAX(j2.revision) FROM assessment_adjudications j2 WHERE j2.tenant_id = a.tenant_id AND j2.attempt_id = a.id)
 LEFT JOIN assessment_reviews r ON r.tenant_id = j.tenant_id AND r.id = j.review_id
 WHERE i.tenant_id = ? AND i.enrollment_id = ? AND i.learner_id = ? AND i.domain_id = ? AND i.concept = ?
 AND i.created_at <= ? ORDER BY i.created_at DESC, i.id DESC, ps.id DESC LIMIT 500`, now, e.tenant, e.enrollment, e.learner, domain, concept, now)
	if err != nil {
		return err
	}
	var observations []retentionBadgeObservation
	var label string
	for rows.Next() {
		var o retentionBadgeObservation
		var passed, valid int
		var responded, evaluated, prior flexTime
		if err := rows.Scan(&o.proof.SnapshotID, &o.beforeJSON, &o.observationJSON, &o.afterJSON, &o.activity, &passed, &o.hints,
			&o.proof.AttemptID, &o.proof.ConceptID, &responded, &evaluated, &o.proof.EvaluationMethod, &o.protocol, &prior, &valid, &label); err != nil {
			rows.Close()
			return err
		}
		o.passed, o.valid = passed == 1, valid == 1 && responded.Valid && evaluated.Valid
		if prior.Valid {
			o.priorExposure = &prior.Time
		}
		o.proof.RespondedAt, o.proof.EvaluatedAt = responded.Time, evaluated.Time
		observations = append(observations, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	slices.Reverse(observations)
	proofs, start := retentionBadgeRun(observations, earliest.Time)
	if len(proofs) < 3 {
		return nil
	}
	last := proofs[len(proofs)-1]
	for _, days := range []int{7, 30, 90} {
		if last.RespondedAt.Sub(start) >= time.Duration(days)*24*time.Hour {
			if err := s.insertBadge(ctx, e, models.BadgeRetention, last.ConceptID, label, days, last.RespondedAt, now, proofs); err != nil {
				return err
			}
		}
	}
	return nil
}

func retentionBadgeRun(observations []retentionBadgeObservation, earliest time.Time) ([]models.BadgeEvidence, time.Time) {
	var proofs []models.BadgeEvidence
	var start, previous time.Time
	seen := map[string]bool{}
	for _, o := range observations {
		if !o.passed || (o.activity == string(models.ActivityRecall) && (!o.valid || o.hints != 0)) {
			proofs, start, previous = nil, time.Time{}, time.Time{}
			if o.proof.RespondedAt.After(earliest) {
				earliest = o.proof.RespondedAt
			}
		}
		// Explicit JSON tags match the immutable pedagogical snapshot format.
		var b struct {
			Last time.Time `json:"last_review"`
			Next time.Time `json:"next_review"`
		}
		var obs struct {
			Applied bool      `json:"fsrs_update_applied"`
			At      time.Time `json:"fsrs_observation_at"`
		}
		var after struct {
			Stability float64   `json:"stability"`
			Last      time.Time `json:"last_review"`
		}
		if json.Unmarshal([]byte(o.beforeJSON), &b) != nil || json.Unmarshal([]byte(o.observationJSON), &obs) != nil || json.Unmarshal([]byte(o.afterJSON), &after) != nil {
			continue
		}
		at := o.proof.RespondedAt
		if o.activity != string(models.ActivityRecall) || !o.valid || o.hints != 0 || o.protocol != models.LearningEventProtocol ||
			o.priorExposure == nil || at.Sub(*o.priorExposure) < 24*time.Hour ||
			b.Last.IsZero() || b.Next.IsZero() || at.Before(b.Next) || at.Sub(b.Last) < 24*time.Hour ||
			!obs.Applied || !obs.At.Equal(at) || after.Stability <= 0 || !after.Last.Equal(at) ||
			!at.After(previous) || seen[o.proof.AttemptID] {
			continue
		}
		if start.IsZero() {
			start = b.Last
			if earliest.After(start) {
				start = earliest
			}
		}
		previous = at
		seen[o.proof.AttemptID] = true
		proofs = append(proofs, o.proof)
	}
	return proofs, start
}

func (s *Store) GetEnrollmentBadges(ctx context.Context, p models.Principal, enrollment, after string, limit int, staff bool) (models.BadgePage, error) {
	page := models.BadgePage{Items: []models.Badge{}}
	if !validProgressID(enrollment, false) || !validProgressPage(after, limit) {
		return page, storeport.ErrInvalidProgressRequest
	}
	err := s.WithTenantTx(ctx, p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var visible string
		if staff {
			from, args, err := txs.progressAccess(ctx, p)
			if err != nil {
				return err
			}
			err = txs.queryRow(ctx, `SELECT c.id`+from+` AND EXISTS (SELECT 1 FROM enrollments e WHERE e.tenant_id = c.tenant_id AND e.cohort_id = c.id AND e.id = ?)`, append(args, enrollment)...).Scan(&visible)
			if errors.Is(err, sql.ErrNoRows) {
				return storeport.ErrNotFound
			}
			if err != nil {
				return err
			}
		} else {
			if err := txs.learnerFormationActor(ctx, p, models.OAuthScopeLearnerRead); err != nil {
				return err
			}
			err := txs.queryRow(ctx, `SELECT e.id FROM enrollments e JOIN formation_versions v ON v.tenant_id = e.tenant_id AND v.id = e.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id WHERE e.tenant_id = ? AND e.id = ?
 AND e.learner_id = ? AND e.user_id = ? AND e.membership_id = ? AND v.status = 'published' AND `+institutionalFormationSQL,
				p.TenantID, enrollment, p.LearnerID, p.UserID, p.MembershipID).Scan(&visible)
			if errors.Is(err, sql.ErrNoRows) {
				return storeport.ErrNotFound
			}
			if err != nil {
				return err
			}
		}
		rows, err := txs.query(ctx, `SELECT id, enrollment_id, formation_version_id, concept_id, label, kind, retention_days, policy_version, achieved_at, recorded_at
 FROM learning_badges WHERE tenant_id = ? AND enrollment_id = ? AND id > ? ORDER BY id LIMIT ?`, p.TenantID, enrollment, after, limit+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var b models.Badge
			if err := rows.Scan(&b.ID, &b.EnrollmentID, &b.VersionID, &b.ConceptID, &b.Label, &b.Kind, &b.RetentionDays, &b.PolicyVersion, &b.AchievedAt, &b.RecordedAt); err != nil {
				rows.Close()
				return err
			}
			page.Items = append(page.Items, b)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			page.NextAfter = page.Items[limit-1].ID
		}
		for i := range page.Items {
			if err := txs.readBadgeEvidence(ctx, p.TenantID, &page.Items[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return models.BadgePage{}, err
	}
	return page, nil
}

func (s *Store) readBadgeEvidence(ctx context.Context, tenant string, b *models.Badge) error {
	rows, err := s.query(ctx, `SELECT be.attempt_id, be.concept_id, be.snapshot_id, be.responded_at, be.evaluated_at, be.evaluation_method,
 CASE WHEN a.id IS NULL THEN 0 WHEN a.status = 'evaluated' AND a.curriculum_invalidated_version = 0
 AND `+assessmentEffectivePassed+` = 1 AND (j.id IS NULL OR j.verdict = 'accept')
 AND (? = 'retention' OR (`+s.assessmentEffectiveTrust()+`) = 1) THEN 1 ELSE 2 END
 FROM learning_badge_evidence be
 LEFT JOIN assessment_attempts a ON a.tenant_id = be.tenant_id AND a.id = be.attempt_id AND a.learner_id = be.learner_id
 LEFT JOIN domains d ON d.id = a.domain_id AND d.learner_id = a.learner_id
 LEFT JOIN assessment_adjudications j ON j.tenant_id = a.tenant_id AND j.attempt_id = a.id
 AND j.revision = (SELECT MAX(j2.revision) FROM assessment_adjudications j2 WHERE j2.tenant_id = a.tenant_id AND j2.attempt_id = a.id)
 LEFT JOIN assessment_reviews r ON r.tenant_id = j.tenant_id AND r.id = j.review_id
 WHERE be.tenant_id = ? AND be.badge_id = ? ORDER BY be.responded_at, be.attempt_id`, b.Kind, tenant, b.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	b.Evidence, b.EvidenceStatus = []models.BadgeEvidence{}, "valid"
	for rows.Next() {
		var proof models.BadgeEvidence
		var status int
		if err := rows.Scan(&proof.AttemptID, &proof.ConceptID, &proof.SnapshotID, &proof.RespondedAt, &proof.EvaluatedAt, &proof.EvaluationMethod, &status); err != nil {
			return err
		}
		if status != 1 {
			b.EvidenceStatus = "changed_or_unavailable"
		}
		b.Evidence = append(b.Evidence, proof)
	}
	if len(b.Evidence) == 0 {
		b.EvidenceStatus = "changed_or_unavailable"
	}
	return rows.Err()
}
