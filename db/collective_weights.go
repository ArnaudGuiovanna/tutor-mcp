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

// RecomputeCollectiveWeights publishes immutable versions from independent,
// current source estimates. It never writes learner states. All versions of a
// tenant are published in one transaction; it returns how many were inserted.
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
		rows, err := txs.query(ctx, `SELECT b.formation_version_id, b.formation_concept_id, b.learner_id,
 d.id, fc.stable_key, b.enrollment_id, b.curriculum_version,
 b.admissible_observations, b.last_interaction_id, b.evidence_hash,
 b.p_learn, b.p_forget, b.p_slip, b.p_guess
 FROM concept_bkt_estimates b
 JOIN enrollments e ON e.tenant_id = b.tenant_id AND e.id = b.enrollment_id AND e.learner_id = b.learner_id
 AND e.formation_version_id = b.formation_version_id
 JOIN domains d ON d.tenant_id = b.tenant_id AND d.learner_id = b.learner_id AND d.formation_enrollment_id = b.enrollment_id
 AND d.graph_version = b.curriculum_version AND d.deleted_at IS NULL
 JOIN formation_versions v ON v.tenant_id = e.tenant_id AND v.id = e.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
		JOIN formation_concepts fc ON fc.tenant_id = b.tenant_id AND fc.id = b.formation_concept_id AND fc.formation_version_id = e.formation_version_id
 WHERE b.tenant_id = ? AND b.admissible_observations >= ? AND e.status IN ('active','completed') AND v.status = 'published'
 AND b.reference_policy_version = ? AND b.policy_version = ? AND `+institutionalFormationSQL+`
 ORDER BY b.updated_at DESC, b.enrollment_id`,
			scope.TenantID, models.CollectiveWeightMinObservations, models.BKTReferencePolicyVersion, models.BKTIndividualEstimatePolicyVersion)
		if err != nil {
			return err
		}
		type candidate struct {
			state    models.ConceptState
			evidence models.BKTCalibrationEvidence
			params   models.BKTParams
		}
		var candidates []candidate
		for rows.Next() {
			c := candidate{}
			c.evidence.TenantID = scope.TenantID
			if err := rows.Scan(&c.evidence.FormationVersionID, &c.evidence.FormationConceptID, &c.state.LearnerID,
				&c.state.DomainID, &c.state.Concept, &c.evidence.EnrollmentID, &c.evidence.CurriculumVersion,
				&c.evidence.Observations, &c.evidence.LastInteractionID, &c.evidence.EvidenceHash,
				&c.params.PLearn, &c.params.PForget, &c.params.PSlip, &c.params.PGuess); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		perConcept := map[string]map[string]models.BKTParams{} // concept -> learner -> params
		versionOf := map[string]string{}
		for _, c := range candidates {
			learnerScope := scope
			learnerScope.LearnerID = c.state.LearnerID
			actual, err := txs.GetBKTCalibrationEvidence(ctx, learnerScope, &c.state, models.BKTCalibrationProfileWindow)
			if err != nil {
				return err
			}
			if !sameCalibrationSource(actual, &c.evidence) {
				continue
			}
			concept, learner := c.evidence.FormationConceptID, c.state.LearnerID
			if perConcept[concept] == nil {
				perConcept[concept] = map[string]models.BKTParams{}
			}
			if _, voted := perConcept[concept][learner]; !voted {
				perConcept[concept][learner] = c.params // newest valid estimate, one vote per learner
			}
			versionOf[concept] = c.evidence.FormationVersionID
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
			result, err := txs.exec(ctx, `INSERT INTO concept_collective_weights
 (tenant_id, formation_version_id, formation_concept_id, version, p_learn, p_forget, p_slip, p_guess, contributors, policy_version, computed_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (tenant_id, formation_concept_id, version) DO NOTHING`,
				scope.TenantID, versionOf[concept], concept, version+1, next.PLearn, next.PForget, next.PSlip, next.PGuess,
				len(votes), models.CollectiveWeightsPolicyVersion, now.UTC())
			if err != nil {
				return err
			}
			inserted, err := result.RowsAffected()
			if err != nil {
				return err
			}
			published += int(inserted)
		}
		return nil
	})
	if err != nil {
		return 0, err // the whole tenant transaction rolled back
	}
	return published, nil
}

// currentCollectiveWeight returns the latest compatible weight and the highest
// allocated version across every policy. Old policies stay in immutable history
// without becoming effective parameters or seeds for the new policy.
func (s *Store) currentCollectiveWeight(ctx context.Context, tenant, concept string) (*models.CollectiveWeight, int64, error) {
	var w models.CollectiveWeight
	var computed flexTime
	var latest int64
	if err := s.queryRow(ctx, `SELECT COALESCE(MAX(version),0) FROM concept_collective_weights WHERE tenant_id = ? AND formation_concept_id = ?`, tenant, concept).Scan(&latest); err != nil {
		return nil, 0, err
	}
	err := s.queryRow(ctx, `SELECT version, p_learn, p_forget, p_slip, p_guess, contributors, policy_version, computed_at
 FROM concept_collective_weights WHERE tenant_id = ? AND formation_concept_id = ? AND policy_version = ? ORDER BY version DESC LIMIT 1`, tenant, concept, models.CollectiveWeightsPolicyVersion).
		Scan(&w.Version, &w.PLearn, &w.PForget, &w.PSlip, &w.PGuess, &w.Contributors, &w.PolicyVersion, &computed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, latest, nil
	}
	if err != nil {
		return nil, 0, err
	}
	w.ComputedAt = computed.Time.UTC()
	return &w, latest, nil
}

// ApplyCollectiveWeights moves a learner's BKT parameters toward the current
// collective weight of the concept, at most once per weight version. It must
// run inside the interaction transaction, after the state is locked, and the
// caller persists the state. It returns nil when nothing applies: a domain that
// is not an active enrollment in a published institutional formation, no
// compatible weight, or a version already applied. Common domains without a
// compatible weight return to the fixed reference. Mastery and FSRS are untouched.
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
	weight, _, err := s.currentCollectiveWeight(ctx, tenant, concept)
	if err != nil {
		return nil, err
	}
	if weight == nil {
		setStateBKTParams(state, models.BKTReferenceParams())
		return nil, nil
	}
	version := weight.Version
	var applied int64 = -1
	var appliedPolicy string
	err = s.queryRow(ctx, `SELECT a.weight_version, COALESCE(w.policy_version,'') FROM collective_weight_applications a
 LEFT JOIN concept_collective_weights w ON w.tenant_id = a.tenant_id AND w.formation_concept_id = a.formation_concept_id AND w.version = a.weight_version
 WHERE a.tenant_id = ? AND a.enrollment_id = ? AND a.formation_concept_id = ?`, tenant, enrollment, concept).Scan(&applied, &appliedPolicy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if applied < 0 || appliedPolicy != models.CollectiveWeightsPolicyVersion {
		setStateBKTParams(state, models.BKTReferenceParams())
		applied = -1
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

func setStateBKTParams(state *models.ConceptState, p models.BKTParams) {
	state.PLearn, state.PForget, state.PSlip, state.PGuess = p.PLearn, p.PForget, p.PSlip, p.PGuess
}
