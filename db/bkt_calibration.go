// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"tutor-mcp/models"
)

// GetBKTCalibrationEvidence returns nil for a personal/free domain. Common
// domains return an envelope even before their first admissible response.
// Caller holds the domain/concept lock in the interaction transaction.
func (s *Store) GetBKTCalibrationEvidence(ctx context.Context, scope models.TenantScope, state *models.ConceptState, limit int) (*models.BKTCalibrationEvidence, error) {
	if err := scope.Validate(); err != nil || state == nil || state.LearnerID == "" ||
		(scope.LearnerID != "" && scope.LearnerID != state.LearnerID) ||
		(state.TenantID != "" && state.TenantID != scope.TenantID) || limit < 0 || limit > models.BKTCalibrationProfileWindow {
		return nil, fmt.Errorf("get BKT calibration evidence: invalid scope or state")
	}
	evidence := &models.BKTCalibrationEvidence{TenantID: scope.TenantID}
	err := s.queryRow(ctx, `SELECT e.id, e.formation_version_id, fc.id, d.graph_version
 FROM domains d JOIN enrollments e ON e.tenant_id = d.tenant_id AND e.id = d.formation_enrollment_id AND e.learner_id = d.learner_id
 JOIN formation_versions v ON v.tenant_id = e.tenant_id AND v.id = e.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 JOIN formation_concepts fc ON fc.tenant_id = e.tenant_id AND fc.formation_version_id = e.formation_version_id AND fc.stable_key = ?
 WHERE d.tenant_id = ? AND d.learner_id = ? AND d.id = ? AND d.deleted_at IS NULL
 AND e.status IN ('active','completed') AND v.status = 'published' AND `+institutionalFormationSQL,
		state.Concept, scope.TenantID, state.LearnerID, state.DomainID).
		Scan(&evidence.EnrollmentID, &evidence.FormationVersionID, &evidence.FormationConceptID, &evidence.CurriculumVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		return evidence, nil
	}
	rows, err := s.query(ctx, `SELECT i.id, i.assessment_attempt_id, i.session_id, i.activity_type, i.success,
 i.response_time, i.confidence, COALESCE(i.error_type,''), i.hints_requested, i.created_at,
 COALESCE(a.evaluation_provenance_json,'{}'), pd.id, pd.snapshot_json
 FROM (SELECT * FROM interactions WHERE tenant_id = ? AND learner_id = ? AND domain_id = ?
  AND enrollment_id = ? AND formation_concept_id = ? AND assessment_attempt_id IS NOT NULL
  ORDER BY created_at DESC,id DESC LIMIT ?) i
 JOIN assessment_attempts a ON a.tenant_id = i.tenant_id AND a.id = i.assessment_attempt_id
 AND a.learner_id = i.learner_id AND a.domain_id = i.domain_id AND a.concept_id = i.concept
 AND a.enrollment_id = i.enrollment_id AND a.formation_concept_id = i.formation_concept_id AND a.session_id = i.session_id
 JOIN pedagogical_decisions pd ON pd.tenant_id = i.tenant_id AND pd.id = a.decision_id
 AND pd.learner_id = i.learner_id AND pd.domain_id = i.domain_id AND pd.session_id = i.session_id
 AND pd.curriculum_version = a.curriculum_version
 WHERE i.curriculum_invalidated_version = 0 AND a.curriculum_invalidated_version = 0
 AND a.curriculum_version = ? AND a.status = 'evaluated' AND a.submitted_at IS NOT NULL AND a.evaluated_at IS NOT NULL
 AND a.created_at <= a.submitted_at AND a.evaluated_at >= a.submitted_at AND i.created_at >= a.evaluated_at
 AND a.event_protocol = ? AND a.activity_type = i.activity_type AND a.passed = i.success
 AND EXISTS (SELECT 1 FROM learning_events le WHERE le.tenant_id = i.tenant_id AND le.learner_id = i.learner_id
  AND le.domain_id = i.domain_id AND le.enrollment_id = i.enrollment_id AND le.concept_id = i.concept
  AND le.attempt_id = a.id AND le.kind = 'response' AND le.source = 'committed_response'
  AND le.curriculum_version = a.curriculum_version AND le.curriculum_invalidated_version = 0 AND le.occurred_at = a.submitted_at)
 ORDER BY i.created_at DESC, i.id DESC`, scope.TenantID, state.LearnerID, state.DomainID,
		evidence.EnrollmentID, evidence.FormationConceptID, models.BKTCalibrationCandidateHorizon, evidence.CurriculumVersion, models.LearningEventProtocol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Hash exactly the bounded source used by the estimator. An invalidation
	// in this window changes its digest, even if an older row fills the gap.
	hash := sha256.New()
	seen := map[string]bool{}
	for rows.Next() {
		i := &models.Interaction{LearnerID: state.LearnerID, DomainID: state.DomainID, Concept: state.Concept}
		var success int
		var created flexTime
		var provenance, decisionID, decisionJSON string
		if err := rows.Scan(&i.ID, &i.AssessmentAttemptID, &i.SessionID, &i.ActivityType, &success,
			&i.ResponseTime, &i.Confidence, &i.ErrorType, &i.HintsRequested, &created, &provenance, &decisionID, &decisionJSON); err != nil {
			return nil, err
		}
		var decision models.PedagogicalDecision
		if err := json.Unmarshal([]byte(decisionJSON), &decision); err != nil || decision.ID != decisionID ||
			decision.LearnerID != state.LearnerID || decision.DomainID != state.DomainID || decision.SessionID != i.SessionID ||
			decision.CurriculumVersion != evidence.CurriculumVersion || decision.Contract.DecisionID != decisionID ||
			decision.Contract.CurriculumVersion != evidence.CurriculumVersion || decision.Contract.TargetConcept != state.Concept ||
			decision.Contract.RecommendedActivityType != models.ActivityType(i.ActivityType) || decision.Contract.LearningEventProtocol != models.LearningEventProtocol {
			continue
		}
		if seen[i.AssessmentAttemptID] || !calibrationActivity(i.ActivityType) || !calibrationProvenanceAdmissible(provenance) {
			continue
		}
		seen[i.AssessmentAttemptID] = true
		i.Success, i.CreatedAt = success != 0, created.Time.UTC()
		if evidence.Observations == 0 {
			evidence.LastInteractionID = i.ID
		}
		evidence.Observations++
		evidence.Interactions = append(evidence.Interactions, i)
		if err := json.NewEncoder(hash).Encode(i); err != nil {
			return nil, err
		}
		if evidence.Observations == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	evidence.EvidenceHash = hex.EncodeToString(hash.Sum(nil))
	return evidence, nil
}

// SaveBKTCalibrationEstimate accepts only an envelope that still identifies
// the actual current committed source. It never reads applied BKT parameters.
func (s *Store) SaveBKTCalibrationEstimate(ctx context.Context, scope models.TenantScope, state *models.ConceptState, evidence *models.BKTCalibrationEvidence, params models.BKTParams, now time.Time) error {
	if evidence == nil || evidence.Observations < 1 || now.IsZero() {
		return fmt.Errorf("save BKT calibration estimate: missing evidence or time")
	}
	for _, p := range []float64{params.PLearn, params.PForget, params.PSlip, params.PGuess} {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return fmt.Errorf("save BKT calibration estimate: invalid probability")
		}
	}
	actual, err := s.GetBKTCalibrationEvidence(ctx, scope, state, models.BKTCalibrationProfileWindow)
	if err != nil {
		return err
	}
	if !sameCalibrationSource(actual, evidence) {
		return fmt.Errorf("save BKT calibration estimate: source changed or scope mismatch")
	}
	_, err = s.exec(ctx, `INSERT INTO concept_bkt_estimates
 (tenant_id,enrollment_id,formation_version_id,formation_concept_id,learner_id,curriculum_version,
 reference_policy_version,policy_version,p_learn,p_forget,p_slip,p_guess,admissible_observations,last_interaction_id,evidence_hash,updated_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT (tenant_id,enrollment_id,formation_concept_id,curriculum_version,policy_version) DO UPDATE SET
 p_learn=excluded.p_learn,p_forget=excluded.p_forget,p_slip=excluded.p_slip,p_guess=excluded.p_guess,
 admissible_observations=excluded.admissible_observations,last_interaction_id=excluded.last_interaction_id,
 evidence_hash=excluded.evidence_hash,updated_at=excluded.updated_at`, scope.TenantID, actual.EnrollmentID,
		actual.FormationVersionID, actual.FormationConceptID, state.LearnerID, actual.CurriculumVersion,
		models.BKTReferencePolicyVersion, models.BKTIndividualEstimatePolicyVersion, params.PLearn,
		params.PForget, params.PSlip, params.PGuess, actual.Observations, actual.LastInteractionID, actual.EvidenceHash, now.UTC())
	return err
}

func sameCalibrationSource(a, b *models.BKTCalibrationEvidence) bool {
	return a != nil && b != nil && a.TenantID == b.TenantID && a.EnrollmentID == b.EnrollmentID &&
		a.FormationVersionID == b.FormationVersionID && a.FormationConceptID == b.FormationConceptID &&
		a.CurriculumVersion == b.CurriculumVersion && a.Observations == b.Observations &&
		a.LastInteractionID == b.LastInteractionID && a.EvidenceHash == b.EvidenceHash
}

func calibrationActivity(activity string) bool {
	switch models.ActivityType(activity) {
	case models.ActivityRecall, models.ActivityDiagnosticAssessment, models.ActivityNewConcept,
		models.ActivityMasteryChallenge, models.ActivityDebuggingCase, models.ActivityPractice,
		models.ActivityDebugMisconception, models.ActivityFeynmanPrompt, models.ActivityTransferProbe:
		return true
	default:
		return false
	}
}

// The committed protocol establishes provenance, not human identity or a
// certified grade. Host evaluations remain untrusted. Explicit simulation or
// preview declarations are excluded, including nested evaluator metadata.
func calibrationProvenanceAdmissible(raw string) bool {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return false
	}
	var synthetic func(any) bool
	synthetic = func(v any) bool {
		switch node := v.(type) {
		case map[string]any:
			for key, child := range node {
				key = strings.ToLower(key)
				if flag, ok := child.(bool); ok && flag && (key == "synthetic" || key == "is_synthetic" || key == "preview" || key == "is_preview" || key == "simulation" || key == "simulated") {
					return true
				}
				if label, ok := child.(string); ok && (key == "source" || key == "source_kind" || key == "kind" || key == "mode" || key == "environment") {
					switch strings.ToLower(strings.TrimSpace(label)) {
					case "synthetic", "preview", "simulation", "simulated", "test", "demo":
						return true
					}
				}
				if synthetic(child) {
					return true
				}
			}
		case []any:
			for _, child := range node {
				if synthetic(child) {
					return true
				}
			}
		}
		return false
	}
	return !synthetic(value)
}
