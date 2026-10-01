// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func clampParam(v float64) float64 {
	return math.Min(models.CollectiveParamMax, math.Max(models.CollectiveParamMin, v))
}

func stepToward(from, to, maxStep float64) float64 {
	return from + math.Max(-maxStep, math.Min(maxStep, to-from))
}

// nextCollectiveParams returns the parameters of the next weight version, or
// false when the median stays within the hysteresis band of the current one.
// The first version is the clamped median; later ones move by a capped step.
func nextCollectiveParams(median models.BKTParams, current *models.BKTParams) (models.BKTParams, bool) {
	target := models.BKTParams{PLearn: clampParam(median.PLearn), PForget: clampParam(median.PForget), PSlip: clampParam(median.PSlip), PGuess: clampParam(median.PGuess)}
	if current == nil {
		return target, true
	}
	moved := math.Abs(target.PLearn-current.PLearn) >= models.CollectiveWeightMinChange ||
		math.Abs(target.PForget-current.PForget) >= models.CollectiveWeightMinChange ||
		math.Abs(target.PSlip-current.PSlip) >= models.CollectiveWeightMinChange ||
		math.Abs(target.PGuess-current.PGuess) >= models.CollectiveWeightMinChange
	if !moved {
		return *current, false
	}
	step := models.CollectiveWeightMaxStep
	return models.BKTParams{
		PLearn: clampParam(stepToward(current.PLearn, target.PLearn, step)), PForget: clampParam(stepToward(current.PForget, target.PForget, step)),
		PSlip: clampParam(stepToward(current.PSlip, target.PSlip, step)), PGuess: clampParam(stepToward(current.PGuess, target.PGuess, step)),
	}, true
}

// applyCollectiveWeight is pure. A state with no review takes the weight as its
// prior; a reviewed state moves a bounded fraction toward it.
func applyCollectiveWeight(state models.BKTParams, weight models.BKTParams, prior bool) models.BKTParams {
	if prior {
		return models.BKTParams{PLearn: clampParam(weight.PLearn), PForget: clampParam(weight.PForget), PSlip: clampParam(weight.PSlip), PGuess: clampParam(weight.PGuess)}
	}
	blend := func(from, to float64) float64 {
		return clampParam(stepToward(from, from+models.CollectiveBlendFactor*(to-from), models.CollectiveBlendMaxStep))
	}
	return models.BKTParams{PLearn: blend(state.PLearn, weight.PLearn), PForget: blend(state.PForget, weight.PForget), PSlip: blend(state.PSlip, weight.PSlip), PGuess: blend(state.PGuess, weight.PGuess)}
}

type weightContribution struct {
	version string
	params  models.BKTParams
}

// RecomputeCollectiveWeights publishes new immutable weight versions. It reads
// learner states but never writes them. All versions of a tenant are published
// in one transaction; it returns how many.
func (s *Store) RecomputeCollectiveWeights(ctx context.Context, scope models.TenantScope, now time.Time) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, fmt.Errorf("recompute collective weights: invalid scope")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	published := 0
	err := s.WithTenantTx(ctx, scope, func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		rows, err := txs.query(ctx, `SELECT e.formation_version_id, cs.formation_concept_id, cs.learner_id,
 cs.p_learn, cs.p_forget, cs.p_slip, cs.p_guess
 FROM concept_states cs
 JOIN enrollments e ON e.tenant_id = cs.tenant_id AND e.id = cs.enrollment_id AND e.learner_id = cs.learner_id
 JOIN formation_versions v ON v.tenant_id = e.tenant_id AND v.id = e.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 JOIN formation_concepts fc ON fc.tenant_id = cs.tenant_id AND fc.id = cs.formation_concept_id AND fc.formation_version_id = e.formation_version_id
 WHERE cs.tenant_id = ? AND cs.reps >= ? AND e.status IN ('active','completed') AND v.status = 'published'
 AND `+institutionalFormationSQL+` AND NOT EXISTS (SELECT 1 FROM enrollment_migrations em
 WHERE em.tenant_id = cs.tenant_id AND em.source_enrollment_id = cs.enrollment_id AND cs.updated_at >= em.created_at)`,
			scope.TenantID, models.CollectiveWeightMinReps)
		if err != nil {
			return err
		}
		perConcept := map[string]map[string]models.BKTParams{} // concept -> learner -> params
		versionOf := map[string]string{}
		for rows.Next() {
			var version, concept, learner string
			var p models.BKTParams
			if err := rows.Scan(&version, &concept, &learner, &p.PLearn, &p.PForget, &p.PSlip, &p.PGuess); err != nil {
				rows.Close()
				return err
			}
			if perConcept[concept] == nil {
				perConcept[concept] = map[string]models.BKTParams{}
			}
			perConcept[concept][learner] = p // one vote per distinct learner
			versionOf[concept] = version
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for concept, votes := range perConcept {
			if len(votes) < models.CollectiveWeightMinContributors {
				continue
			}
			var learn, forget, slip, guess []float64
			for _, p := range votes {
				learn, forget, slip, guess = append(learn, p.PLearn), append(forget, p.PForget), append(slip, p.PSlip), append(guess, p.PGuess)
			}
			median := models.BKTParams{PLearn: medianOf(learn), PForget: medianOf(forget), PSlip: medianOf(slip), PGuess: medianOf(guess)}
			current, version, err := txs.currentCollectiveWeight(ctx, scope.TenantID, concept)
			if err != nil {
				return err
			}
			var base *models.BKTParams
			if current != nil {
				base = &current.BKTParams
			}
			next, changed := nextCollectiveParams(median, base)
			if !changed {
				continue
			}
			if _, err := txs.exec(ctx, `INSERT INTO concept_collective_weights
 (tenant_id, formation_version_id, formation_concept_id, version, p_learn, p_forget, p_slip, p_guess, contributors, policy_version, computed_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (tenant_id, formation_concept_id, version) DO NOTHING`,
				scope.TenantID, versionOf[concept], concept, version+1, next.PLearn, next.PForget, next.PSlip, next.PGuess,
				len(votes), models.CollectiveWeightsPolicyVersion, now.UTC()); err != nil {
				return err
			}
			published++
		}
		return nil
	})
	if err != nil {
		return 0, err // the whole tenant transaction rolled back
	}
	return published, nil
}

// currentCollectiveWeight returns the highest version of a concept's weight, or
// nil with version 0. It runs inside the caller's tenant transaction.
func (s *Store) currentCollectiveWeight(ctx context.Context, tenant, concept string) (*models.CollectiveWeight, int64, error) {
	var w models.CollectiveWeight
	var computed flexTime
	err := s.queryRow(ctx, `SELECT version, p_learn, p_forget, p_slip, p_guess, contributors, policy_version, computed_at
 FROM concept_collective_weights WHERE tenant_id = ? AND formation_concept_id = ? ORDER BY version DESC LIMIT 1`, tenant, concept).
		Scan(&w.Version, &w.PLearn, &w.PForget, &w.PSlip, &w.PGuess, &w.Contributors, &w.PolicyVersion, &computed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	w.ComputedAt = computed.Time.UTC()
	return &w, w.Version, nil
}

// ApplyCollectiveWeights moves a learner's BKT parameters toward the current
// collective weight of the concept, at most once per weight version. It must
// run inside the interaction transaction, after the state is locked, and the
// caller persists the state. It returns nil when nothing applies: a domain that
// is not an active enrollment in a published institutional formation, no
// weight, or a version already applied. Mastery and FSRS are never touched.
func (s *Store) ApplyCollectiveWeights(ctx context.Context, scope models.TenantScope, state *models.ConceptState, now time.Time) (*models.CollectiveWeightApplication, error) {
	if err := scope.Validate(); err != nil || state == nil || state.LearnerID == "" || (scope.LearnerID != "" && scope.LearnerID != state.LearnerID) {
		return nil, fmt.Errorf("apply collective weights: invalid scope or state")
	}
	var tenant, enrollment, concept string
	err := s.queryRow(ctx, `SELECT cs.tenant_id, cs.enrollment_id, cs.formation_concept_id
 FROM concept_states cs JOIN enrollments e ON e.tenant_id = cs.tenant_id AND e.id = cs.enrollment_id AND e.learner_id = cs.learner_id
 JOIN formation_versions v ON v.tenant_id = e.tenant_id AND v.id = e.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 WHERE cs.tenant_id = ? AND cs.learner_id = ? AND cs.domain_id = ? AND cs.concept = ? AND e.status = 'active'
 AND v.status = 'published' AND `+institutionalFormationSQL, scope.TenantID, state.LearnerID, state.DomainID, state.Concept).Scan(&tenant, &enrollment, &concept)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	weight, version, err := s.currentCollectiveWeight(ctx, tenant, concept)
	if err != nil || weight == nil {
		return nil, err
	}
	var applied int64 = -1
	err = s.queryRow(ctx, `SELECT weight_version FROM collective_weight_applications
 WHERE tenant_id = ? AND enrollment_id = ? AND formation_concept_id = ?`, tenant, enrollment, concept).Scan(&applied)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if applied >= version {
		return nil, nil
	}
	before := models.BKTParams{PLearn: state.PLearn, PForget: state.PForget, PSlip: state.PSlip, PGuess: state.PGuess}
	prior := applied < 0 && state.Reps == 0
	after := applyCollectiveWeight(before, weight.BKTParams, prior)
	mode := models.CollectiveApplyBlend
	if prior {
		mode = models.CollectiveApplyPrior
	}
	if _, err := s.exec(ctx, `INSERT INTO collective_weight_applications
 (tenant_id, enrollment_id, formation_concept_id, learner_id, weight_version, mode, applied_at)
 VALUES (?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (tenant_id, enrollment_id, formation_concept_id) DO UPDATE SET
 weight_version = excluded.weight_version, mode = excluded.mode, applied_at = excluded.applied_at`,
		tenant, enrollment, concept, state.LearnerID, version, mode, now.UTC()); err != nil {
		return nil, err
	}
	state.PLearn, state.PForget, state.PSlip, state.PGuess = after.PLearn, after.PForget, after.PSlip, after.PGuess
	return &models.CollectiveWeightApplication{Version: version, Mode: mode, Before: before, After: after}, nil
}
