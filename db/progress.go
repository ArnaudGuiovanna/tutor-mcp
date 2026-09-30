// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func validProgressID(id string, optional bool) bool {
	return (optional || id != "") && len(id) <= 255 && strings.TrimSpace(id) == id && !strings.ContainsRune(id, 0)
}

func validProgressPage(after string, limit int) bool {
	return validProgressID(after, true) && limit >= 1 && limit <= 100
}

// A single visibility predicate is shared by discovery and all detail reads.
// Formation and cohort grants are deliberately distinct and combine across roles.
func (s *Store) progressAccess(ctx context.Context, p models.Principal) (string, []any, error) {
	if !p.CanReadInstitutionProgress() || !models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeProgressRead) {
		return "", nil, storeport.ErrInvalidPrincipal
	}
	if err := s.ValidatePrincipal(ctx, p); err != nil {
		return "", nil, err
	}
	global := formationGlobalAccess(p)
	manager, trainer := 0, 0
	if slices.Contains(p.Roles, models.RolePedagogyManager) {
		manager = 1
	}
	if slices.Contains(p.Roles, models.RoleTrainer) {
		trainer = 1
	}
	return ` FROM cohorts c JOIN formation_versions v ON v.tenant_id = c.tenant_id AND v.id = c.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 WHERE c.tenant_id = ? AND v.status = 'published' AND ` + institutionalFormationSQL + ` AND
 (? = 1 OR (? = 1 AND (f.owner_membership_id = ? OR EXISTS
 (SELECT 1 FROM formation_trainers ft WHERE ft.tenant_id = f.tenant_id AND ft.formation_id = f.id AND ft.membership_id = ?)))
 OR (? = 1 AND EXISTS (SELECT 1 FROM cohort_trainers ct WHERE ct.tenant_id = c.tenant_id AND ct.cohort_id = c.id AND ct.membership_id = ?)))`,
		[]any{p.TenantID, global, manager, p.MembershipID, p.MembershipID, trainer, p.MembershipID}, nil
}

const trainerCohortColumns = `c.id, c.name, c.status, f.id, f.name, f.status, v.id, v.version,
 (SELECT COUNT(*) FROM enrollments e WHERE e.tenant_id = c.tenant_id AND e.cohort_id = c.id),
 (SELECT COUNT(*) FROM enrollments e WHERE e.tenant_id = c.tenant_id AND e.cohort_id = c.id AND e.status = 'active'),
 (SELECT COUNT(*) FROM enrollments e WHERE e.tenant_id = c.tenant_id AND e.cohort_id = c.id AND e.status = 'completed')`

func scanTrainerCohort(row interface{ Scan(...any) error }) (models.TrainerCohort, error) {
	var c models.TrainerCohort
	err := row.Scan(&c.CohortID, &c.CohortName, &c.CohortStatus, &c.FormationID, &c.FormationName, &c.FormationStatus, &c.VersionID, &c.Version, &c.EnrollmentCount, &c.ActiveCount, &c.CompletedCount)
	if errors.Is(err, sql.ErrNoRows) {
		err = storeport.ErrNotFound
	}
	return c, err
}

func (s *Store) ListTrainerCohorts(ctx context.Context, p models.Principal, after string, limit int) (models.TrainerCohortPage, error) {
	page := models.TrainerCohortPage{Items: []models.TrainerCohort{}}
	if !p.CanReadInstitutionProgress() {
		return page, storeport.ErrInvalidPrincipal
	}
	if !validProgressPage(after, limit) {
		return page, storeport.ErrInvalidProgressRequest
	}
	err := s.WithTenantTx(ctx, p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		from, args, err := txs.progressAccess(ctx, p)
		if err != nil {
			return err
		}
		rows, err := txs.query(ctx, `SELECT `+trainerCohortColumns+from+` AND c.id > ? ORDER BY c.id LIMIT ?`, append(args, after, limit+1)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanTrainerCohort(rows)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, c)
		}
		return rows.Err()
	})
	if err != nil {
		return models.TrainerCohortPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = page.Items[limit-1].CohortID
	}
	return page, nil
}

// New canonical states contain priors. Only reviewed concepts contribute to
// reported mastery; absence of evidence must not become a failing score.
const progressObserved = `(s.reps > 0 OR s.last_review IS NOT NULL)`

// Older migrations could leave compatibility rows bound to the source
// enrollment. Never expose estimates written after that enrollment migrated.
// Missing historical evidence is safer than attributing a later cohort's data.
const progressHistoricalState = `NOT EXISTS (SELECT 1 FROM enrollment_migrations em
 WHERE em.tenant_id = s.tenant_id AND em.source_enrollment_id = s.enrollment_id AND s.updated_at >= em.created_at)`

func (s *Store) progressLearners(ctx context.Context, tenant, cohort, enrollment, after string, limit int, now time.Time) ([]models.LearnerProgressSummary, string, error) {
	query := `SELECT e.id, u.email, e.status, e.created_at,
 (SELECT COUNT(*) FROM formation_concepts fc WHERE fc.tenant_id = e.tenant_id AND fc.formation_version_id = e.formation_version_id),
 COUNT(CASE WHEN ` + progressObserved + ` THEN 1 END),
 COUNT(CASE WHEN ` + progressObserved + ` AND s.p_mastery >= ? THEN 1 END),
 AVG(CASE WHEN ` + progressObserved + ` THEN s.p_mastery END), MAX(s.last_review),
 (SELECT COUNT(*) FROM learner_concept_states hs JOIN enrollment_migrations em ON em.tenant_id = hs.tenant_id AND em.source_enrollment_id = hs.enrollment_id
 WHERE hs.tenant_id = e.tenant_id AND hs.enrollment_id = e.id AND hs.updated_at >= em.created_at)
 FROM enrollments e JOIN users u ON u.id = e.user_id
 LEFT JOIN learner_concept_states s ON s.tenant_id = e.tenant_id AND s.enrollment_id = e.id
 AND s.formation_version_id = e.formation_version_id AND s.learner_id = e.learner_id AND ` + progressHistoricalState + `
 WHERE e.tenant_id = ? AND e.cohort_id = ? AND e.id > ?`
	args := []any{models.ProgressMasteryThreshold, tenant, cohort, after}
	if enrollment != "" {
		query += ` AND e.id = ?`
		args = append(args, enrollment)
	}
	query += ` GROUP BY e.id, u.email, e.status, e.created_at, e.tenant_id, e.formation_version_id ORDER BY e.id LIMIT ?`
	rows, err := s.query(ctx, query, append(args, limit+1)...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := []models.LearnerProgressSummary{}
	for rows.Next() {
		var item models.LearnerProgressSummary
		var mastery sql.NullFloat64
		var review flexTime
		var unavailable int
		if err := rows.Scan(&item.EnrollmentID, &item.Email, &item.Status, &item.JoinedAt, &item.ConceptCount, &item.ObservedConceptCount, &item.MasteredConceptCount, &mastery, &review, &unavailable); err != nil {
			return nil, "", err
		}
		if mastery.Valid {
			item.AverageMastery = &mastery.Float64
		}
		if review.Valid {
			at := review.Time.UTC()
			item.LastReviewAt = &at
		}
		item.AttentionSignals = []string{}
		item.HistoricalEstimatesUnavailable = unavailable > 0
		if item.HistoricalEstimatesUnavailable {
			item.AttentionSignals = append(item.AttentionSignals, "historical_estimates_unavailable")
		}
		if item.ObservedConceptCount == 0 && !item.HistoricalEstimatesUnavailable {
			item.AttentionSignals = append(item.AttentionSignals, "no_reviews")
		}
		if item.Status == "active" && item.LastReviewAt != nil && now.Sub(*item.LastReviewAt) >= 14*24*time.Hour {
			item.AttentionSignals = append(item.AttentionSignals, "no_review_in_14_days")
		}
		if item.ObservedConceptCount > item.MasteredConceptCount {
			item.AttentionSignals = append(item.AttentionSignals, "reviewed_concepts_below_mastery_threshold")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[limit-1].EnrollmentID
	}
	return items, next, nil
}

func (s *Store) GetCohortInsights(ctx context.Context, p models.Principal, cohort, after, conceptAfter string, limit int) (*models.CohortInsights, error) {
	if !p.CanReadInstitutionProgress() {
		return nil, storeport.ErrInvalidPrincipal
	}
	if !validProgressID(cohort, false) || !validProgressPage(after, limit) || !validProgressID(conceptAfter, true) {
		return nil, storeport.ErrInvalidProgressRequest
	}
	out := &models.CohortInsights{GeneratedAt: time.Now().UTC(), Concepts: []models.CohortConceptInsight{}, MinimumContributors: models.ProgressMinimumContributors, SynthesisGuidance: models.ProgressSynthesisGuidance}
	err := s.WithTenantTx(ctx, p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		from, args, err := txs.progressAccess(ctx, p)
		if err != nil {
			return err
		}
		out.Cohort, err = scanTrainerCohort(txs.queryRow(ctx, `SELECT `+trainerCohortColumns+from+` AND c.id = ?`, append(args, cohort)...))
		if err != nil {
			return err
		}
		out.Learners, out.NextAfter, err = txs.progressLearners(ctx, p.TenantID, cohort, "", after, limit, out.GeneratedAt)
		if err != nil {
			return err
		}
		if err := txs.progressAssignments(ctx, p, out); err != nil {
			return err
		}
		rows, err := txs.query(ctx, `SELECT fc.id, fc.stable_key, fc.label,
 COUNT(DISTINCT CASE WHEN `+progressObserved+` THEN e.learner_id END),
 AVG(CASE WHEN `+progressObserved+` THEN s.p_mastery END)
 FROM formation_concepts fc
 LEFT JOIN enrollments e ON e.tenant_id = fc.tenant_id AND e.formation_version_id = fc.formation_version_id AND e.cohort_id = ? AND e.status = 'active'
 LEFT JOIN learner_concept_states s ON s.tenant_id = e.tenant_id AND s.enrollment_id = e.id AND s.formation_concept_id = fc.id AND s.learner_id = e.learner_id AND `+progressHistoricalState+`
 WHERE fc.tenant_id = ? AND fc.formation_version_id = ? AND fc.id > ?
 GROUP BY fc.id, fc.stable_key, fc.label ORDER BY fc.id LIMIT ?`, cohort, p.TenantID, out.Cohort.VersionID, conceptAfter, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item models.CohortConceptInsight
			var average sql.NullFloat64
			if err := rows.Scan(&item.ConceptID, &item.StableKey, &item.Label, &item.ObservedLearners, &average); err != nil {
				return err
			}
			if item.ObservedLearners >= models.ProgressMinimumContributors && average.Valid {
				item.AverageMastery = &average.Float64
			} else {
				item.SuppressionReason = "insufficient_observed_learners"
			}
			out.Concepts = append(out.Concepts, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(out.Concepts) > limit {
		out.Concepts = out.Concepts[:limit]
		out.NextConceptAfter = out.Concepts[limit-1].ConceptID
	}
	return out, nil
}

func (s *Store) GetLearnerProgress(ctx context.Context, p models.Principal, enrollment, after string, limit int) (*models.LearnerProgress, error) {
	if !p.CanReadInstitutionProgress() {
		return nil, storeport.ErrInvalidPrincipal
	}
	if !validProgressID(enrollment, false) || !validProgressPage(after, limit) {
		return nil, storeport.ErrInvalidProgressRequest
	}
	out := &models.LearnerProgress{GeneratedAt: time.Now().UTC(), Concepts: []models.ProgressConcept{}, RecentSessions: []models.ProgressSession{}, RecentSessionLimit: 10, SynthesisGuidance: models.ProgressSynthesisGuidance}
	err := s.WithTenantTx(ctx, p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		from, args, err := txs.progressAccess(ctx, p)
		if err != nil {
			return err
		}
		out.Cohort, err = scanTrainerCohort(txs.queryRow(ctx, `SELECT `+trainerCohortColumns+from+` AND EXISTS
 (SELECT 1 FROM enrollments e WHERE e.tenant_id = c.tenant_id AND e.cohort_id = c.id AND e.id = ?)`, append(args, enrollment)...))
		if err != nil {
			return err
		}
		items, _, err := txs.progressLearners(ctx, p.TenantID, out.Cohort.CohortID, enrollment, "", 1, out.GeneratedAt)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return storeport.ErrNotFound
		}
		out.Learner = items[0]
		rows, err := txs.query(ctx, `SELECT fc.id, fc.stable_key, fc.label, fm.title,
 CASE WHEN `+progressObserved+` THEN s.p_mastery END, COALESCE(s.reps, 0), s.last_review, s.next_review
 FROM formation_concepts fc JOIN formation_modules fm ON fm.tenant_id = fc.tenant_id AND fm.id = fc.module_id
 JOIN enrollments e ON e.tenant_id = fc.tenant_id AND e.formation_version_id = fc.formation_version_id AND e.id = ?
 LEFT JOIN learner_concept_states s ON s.tenant_id = fc.tenant_id AND s.formation_concept_id = fc.id AND s.enrollment_id = e.id AND s.learner_id = e.learner_id AND `+progressHistoricalState+`
 WHERE fc.tenant_id = ? AND fc.formation_version_id = ? AND fc.id > ? ORDER BY fc.id LIMIT ?`, enrollment, p.TenantID, out.Cohort.VersionID, after, limit+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item models.ProgressConcept
			var mastery sql.NullFloat64
			var last, next flexTime
			if err := rows.Scan(&item.ConceptID, &item.StableKey, &item.Label, &item.ModuleTitle, &mastery, &item.ReviewCount, &last, &next); err != nil {
				rows.Close()
				return err
			}
			if mastery.Valid {
				item.Mastery = &mastery.Float64
			}
			if last.Valid {
				at := last.Time.UTC()
				item.LastReviewAt = &at
			}
			if next.Valid && mastery.Valid {
				at := next.Time.UTC()
				item.NextReviewAt = &at
			}
			out.Concepts = append(out.Concepts, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = txs.query(ctx, `SELECT id, status, started_at, last_active_at FROM learning_sessions
 WHERE tenant_id = ? AND enrollment_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`, p.TenantID, enrollment, out.RecentSessionLimit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item models.ProgressSession
			if err := rows.Scan(&item.SessionID, &item.Status, &item.StartedAt, &item.LastActiveAt); err != nil {
				rows.Close()
				return err
			}
			out.RecentSessions = append(out.RecentSessions, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if err := txs.queryRow(ctx, `SELECT COUNT(*) FROM interactions WHERE tenant_id = ? AND enrollment_id = ?`, p.TenantID, enrollment).Scan(&out.InteractionCount); err != nil {
			return err
		}
		return txs.queryRow(ctx, `SELECT COUNT(*), COUNT(CASE WHEN trusted_evaluation = 1 THEN 1 END) FROM assessment_attempts
 WHERE tenant_id = ? AND enrollment_id = ? AND status = 'evaluated' AND curriculum_invalidated_version = 0`, p.TenantID, enrollment).Scan(&out.EvaluatedAttemptCount, &out.TrustedEvaluationCount)
	})
	if err != nil {
		return nil, err
	}
	if len(out.Concepts) > limit {
		out.Concepts = out.Concepts[:limit]
		out.NextAfter = out.Concepts[limit-1].ConceptID
	}
	return out, nil
}

var _ storeport.ProgressStore = (*Store)(nil)
