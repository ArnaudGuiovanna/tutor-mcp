// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// smallStatisticsGroup is a non-empty group below the disclosure threshold.
func smallStatisticsGroup(n int) bool {
	return n > 0 && n < models.ProgressMinimumContributors
}

// holdersVisible applies complementary suppression: a holder count is reported
// only when neither the holders nor the non-holders form a small group, so a
// suppressed small group cannot be recovered by subtraction.
func holdersVisible(holders, total int) bool {
	return !smallStatisticsGroup(holders) && !smallStatisticsGroup(total-holders)
}

func meanOf(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func medianOf(values []float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

type statisticsCell struct {
	count        *int
	mean, median *float64
}

// masteryCell returns an all-NULL cell below the minimum contributors.
func masteryCell(values []float64) statisticsCell {
	if len(values) < models.ProgressMinimumContributors {
		return statisticsCell{}
	}
	n, mean, median := len(values), meanOf(values), medianOf(values)
	return statisticsCell{count: &n, mean: &mean, median: &median}
}

func (s *Store) statisticsCohorts(ctx context.Context, tenant, after string, limit int) ([][2]string, error) {
	rows, err := s.query(ctx, `SELECT c.id, c.formation_version_id FROM cohorts c
 JOIN formation_versions v ON v.tenant_id = c.tenant_id AND v.id = c.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 WHERE c.tenant_id = ? AND v.status = 'published' AND `+institutionalFormationSQL+` AND c.id > ?
 ORDER BY c.id LIMIT ?`, tenant, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}

// RecomputeInstitutionStatistics refreshes the snapshot of every published
// institutional cohort and returns how many were refreshed. Each cohort is one
// transaction of upserts keyed by tenant and cohort: a retry or a concurrent
// worker converges on the same rows. It continues past a failing cohort so one
// bad cohort cannot starve the others, and reports the first error.
func (s *Store) RecomputeInstitutionStatistics(ctx context.Context, scope models.TenantScope, now time.Time) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, fmt.Errorf("recompute institution statistics: invalid scope")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	done, after := 0, ""
	var first error
	for {
		var batch [][2]string
		err := s.WithTenantTx(ctx, scope, func(ctx context.Context, scoped storeport.Store) error {
			var err error
			batch, err = scoped.(*Store).statisticsCohorts(ctx, scope.TenantID, after, 100)
			return err
		})
		if err != nil {
			return done, err
		}
		for _, cohort := range batch {
			after = cohort[0]
			err := s.WithTenantTx(ctx, scope, func(ctx context.Context, scoped storeport.Store) error {
				return scoped.(*Store).recomputeCohortStatistics(ctx, scope.TenantID, cohort[0], cohort[1], now.UTC())
			})
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			done++
		}
		if len(batch) < 100 {
			return done, first
		}
	}
}

func (s *Store) recomputeCohortStatistics(ctx context.Context, tenant, cohort, version string, now time.Time) error {
	// Same evidence rules as the staff progress read: reviewed concepts only,
	// and no estimate written after an enrollment migrated away.
	rows, err := s.query(ctx, `SELECT fc.id, e.learner_id, s.p_mastery
 FROM enrollments e
 JOIN formation_concepts fc ON fc.tenant_id = e.tenant_id AND fc.formation_version_id = e.formation_version_id
 JOIN learner_concept_states s ON s.tenant_id = e.tenant_id AND s.enrollment_id = e.id
 AND s.formation_concept_id = fc.id AND s.learner_id = e.learner_id AND `+progressHistoricalState+`
 WHERE e.tenant_id = ? AND e.cohort_id = ? AND e.formation_version_id = ?
 AND e.status IN ('active','completed') AND e.learner_id IS NOT NULL AND `+progressObserved,
		tenant, cohort, version)
	if err != nil {
		return err
	}
	perConcept := map[string][]float64{}
	perLearner := map[string][]float64{}
	for rows.Next() {
		var concept, learner string
		var mastery float64
		if err := rows.Scan(&concept, &learner, &mastery); err != nil {
			rows.Close()
			return err
		}
		perConcept[concept] = append(perConcept[concept], mastery)
		perLearner[learner] = append(perLearner[learner], mastery)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var participants, completed int
	if err := s.queryRow(ctx, `SELECT COUNT(DISTINCT learner_id),
 COUNT(DISTINCT CASE WHEN status = 'completed' THEN learner_id END)
 FROM enrollments WHERE tenant_id = ? AND cohort_id = ? AND formation_version_id = ?
 AND status IN ('active','completed') AND learner_id IS NOT NULL`, tenant, cohort, version).Scan(&participants, &completed); err != nil {
		return err
	}
	learnerMeans := make([]float64, 0, len(perLearner))
	for _, values := range perLearner {
		learnerMeans = append(learnerMeans, meanOf(values))
	}
	overall := masteryCell(learnerMeans)
	open := overall.count != nil // the whole cohort is below threshold otherwise
	var participantCount, completedCount *int
	if open {
		participantCount = &participants
		if holdersVisible(completed, participants) {
			completedCount = &completed
		}
	}
	if _, err := s.exec(ctx, `INSERT INTO cohort_statistics
 (tenant_id, cohort_id, formation_version_id, policy_version, computed_at, contributors, participants, completed_learners, mean_mastery, median_mastery)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (tenant_id, cohort_id) DO UPDATE SET formation_version_id = excluded.formation_version_id,
 policy_version = excluded.policy_version, computed_at = excluded.computed_at, contributors = excluded.contributors,
 participants = excluded.participants, completed_learners = excluded.completed_learners,
 mean_mastery = excluded.mean_mastery, median_mastery = excluded.median_mastery`,
		tenant, cohort, version, models.StatisticsPolicyVersion, now, overall.count, participantCount, completedCount, overall.mean, overall.median); err != nil {
		return err
	}

	concepts, err := s.query(ctx, `SELECT id FROM formation_concepts WHERE tenant_id = ? AND formation_version_id = ? ORDER BY id`, tenant, version)
	if err != nil {
		return err
	}
	var conceptIDs []string
	for concepts.Next() {
		var id string
		if err := concepts.Scan(&id); err != nil {
			concepts.Close()
			return err
		}
		conceptIDs = append(conceptIDs, id)
	}
	err = concepts.Err()
	concepts.Close()
	if err != nil {
		return err
	}
	for _, id := range conceptIDs {
		cell := statisticsCell{}
		if open {
			cell = masteryCell(perConcept[id])
		}
		if _, err := s.exec(ctx, `INSERT INTO cohort_concept_statistics
 (tenant_id, cohort_id, concept_id, observed_learners, mean_mastery, median_mastery) VALUES (?, ?, ?, ?, ?, ?)
 ON CONFLICT (tenant_id, cohort_id, concept_id) DO UPDATE SET observed_learners = excluded.observed_learners,
 mean_mastery = excluded.mean_mastery, median_mastery = excluded.median_mastery`,
			tenant, cohort, id, cell.count, cell.mean, cell.median); err != nil {
			return err
		}
	}

	holders := map[[2]any]int{}
	badgeRows, err := s.query(ctx, `SELECT b.kind, b.retention_days, COUNT(DISTINCT b.learner_id)
 FROM learning_badges b JOIN enrollments e ON e.tenant_id = b.tenant_id AND e.id = b.enrollment_id
 AND e.formation_version_id = b.formation_version_id
 WHERE e.tenant_id = ? AND e.cohort_id = ? AND e.formation_version_id = ? AND e.status IN ('active','completed')
 GROUP BY b.kind, b.retention_days`, tenant, cohort, version)
	if err != nil {
		return err
	}
	for badgeRows.Next() {
		var kind string
		var days, n int
		if err := badgeRows.Scan(&kind, &days, &n); err != nil {
			badgeRows.Close()
			return err
		}
		holders[[2]any{kind, days}] = n
	}
	err = badgeRows.Err()
	badgeRows.Close()
	if err != nil {
		return err
	}
	for _, kind := range []struct {
		name string
		days int
	}{{models.BadgeMastery, 0}, {models.BadgeFormation, 0}, {models.BadgeRetention, 7}, {models.BadgeRetention, 30}, {models.BadgeRetention, 90}} {
		var learners *int
		if n := holders[[2]any{kind.name, kind.days}]; open && holdersVisible(n, participants) {
			learners = &n
		}
		if _, err := s.exec(ctx, `INSERT INTO cohort_badge_statistics (tenant_id, cohort_id, kind, retention_days, learners)
 VALUES (?, ?, ?, ?, ?)
 ON CONFLICT (tenant_id, cohort_id, kind, retention_days) DO UPDATE SET learners = excluded.learners`,
			tenant, cohort, kind.name, kind.days, learners); err != nil {
			return err
		}
	}
	return nil
}

func statInt(v sql.NullInt64) models.StatInt {
	if !v.Valid {
		return models.StatInt{Status: models.StatisticsInsufficientData}
	}
	n := int(v.Int64)
	return models.StatInt{Value: &n, Status: models.StatisticsAvailable}
}

func statFloat(v sql.NullFloat64) models.StatFloat {
	if !v.Valid {
		return models.StatFloat{Status: models.StatisticsInsufficientData}
	}
	return models.StatFloat{Value: &v.Float64, Status: models.StatisticsAvailable}
}

// GetCohortStatistics reads the latest worker snapshot under the same live
// staff authorization as the cohort progress read. It never computes anything.
func (s *Store) GetCohortStatistics(ctx context.Context, p models.Principal, cohort, conceptAfter string, limit int) (*models.CohortStatistics, error) {
	if !p.CanReadInstitutionProgress() {
		return nil, storeport.ErrInvalidPrincipal
	}
	if !validProgressID(cohort, false) || !validProgressPage(conceptAfter, limit) {
		return nil, storeport.ErrInvalidProgressRequest
	}
	out := &models.CohortStatistics{
		Status: models.StatisticsNotYetComputed, PolicyVersion: models.StatisticsPolicyVersion,
		MinimumContributors: models.ProgressMinimumContributors, SynthesisGuidance: models.StatisticsSynthesisGuidance,
		Badges: []models.BadgeStatistics{}, Concepts: []models.ConceptStatistics{},
		Contributors: models.StatInt{Status: models.StatisticsInsufficientData}, Participants: models.StatInt{Status: models.StatisticsInsufficientData},
		CompletedLearners: models.StatInt{Status: models.StatisticsInsufficientData},
		MeanMastery:       models.StatFloat{Status: models.StatisticsInsufficientData}, MedianMastery: models.StatFloat{Status: models.StatisticsInsufficientData},
	}
	now := time.Now().UTC()
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
		var computed flexTime
		var contributors, participants, completed sql.NullInt64
		var mean, median sql.NullFloat64
		var policy string
		err = txs.queryRow(ctx, `SELECT computed_at, policy_version, contributors, participants, completed_learners, mean_mastery, median_mastery
 FROM cohort_statistics WHERE tenant_id = ? AND cohort_id = ? AND formation_version_id = ?`,
			p.TenantID, cohort, out.Cohort.VersionID).Scan(&computed, &policy, &contributors, &participants, &completed, &mean, &median)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		at := computed.Time.UTC()
		out.ComputedAt, out.PolicyVersion = &at, policy
		out.Stale = now.Sub(at) > models.StatisticsStaleAfter
		out.Contributors, out.Participants, out.CompletedLearners = statInt(contributors), statInt(participants), statInt(completed)
		out.MeanMastery, out.MedianMastery = statFloat(mean), statFloat(median)
		out.Status = models.StatisticsAvailable
		if !contributors.Valid {
			out.Status = models.StatisticsInsufficientData
			return nil
		}
		badges, err := txs.query(ctx, `SELECT kind, retention_days, learners FROM cohort_badge_statistics
 WHERE tenant_id = ? AND cohort_id = ? ORDER BY retention_days, kind`, p.TenantID, cohort)
		if err != nil {
			return err
		}
		for badges.Next() {
			var item models.BadgeStatistics
			var learners sql.NullInt64
			if err := badges.Scan(&item.Kind, &item.RetentionDays, &learners); err != nil {
				badges.Close()
				return err
			}
			item.Learners = statInt(learners)
			out.Badges = append(out.Badges, item)
		}
		err = badges.Err()
		badges.Close()
		if err != nil {
			return err
		}
		rows, err := txs.query(ctx, `SELECT fc.id, fc.stable_key, fc.label, cs.observed_learners, cs.mean_mastery, cs.median_mastery
 FROM formation_concepts fc JOIN cohort_concept_statistics cs ON cs.tenant_id = fc.tenant_id AND cs.concept_id = fc.id AND cs.cohort_id = ?
 WHERE fc.tenant_id = ? AND fc.formation_version_id = ? AND fc.id > ? ORDER BY fc.id LIMIT ?`,
			cohort, p.TenantID, out.Cohort.VersionID, conceptAfter, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item models.ConceptStatistics
			var observed sql.NullInt64
			var cMean, cMedian sql.NullFloat64
			if err := rows.Scan(&item.ConceptID, &item.StableKey, &item.Label, &observed, &cMean, &cMedian); err != nil {
				return err
			}
			item.ObservedLearners, item.MeanMastery, item.MedianMastery = statInt(observed), statFloat(cMean), statFloat(cMedian)
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
