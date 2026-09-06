// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
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
)

var ErrAssessmentStateConflict = errors.New("assessment attempt state conflict")

const assessmentColumns = `id, learner_id, domain_id, concept_id, session_id,
       activity_id, activity_version, activity_type, observable,
       task_text, task_content_hash, response_text, response_content_hash,
       rubric_json, passing_score, status, rubric_score_json, score, passed,
       evaluator_id, evaluation_method, evaluation_provenance_json,
       trusted_evaluation, created_at, submitted_at, evaluated_at, cancelled_at,
       decision_id, curriculum_version, curriculum_concept_json, outcome_ids_json, curriculum_invalidated_version`

// assessmentEvidenceColumns deliberately derives effective trust at the read
// boundary. Both the single-concept and batched evidence paths use this exact
// projection so batching cannot weaken the supported-evaluator or high-stakes
// policies.
const assessmentEvidenceColumns = `a.id, a.learner_id, a.domain_id, a.concept_id, a.session_id,
       a.activity_id, a.activity_version, a.activity_type, a.observable,
       a.task_text, a.task_content_hash, a.response_text, a.response_content_hash,
       a.rubric_json, a.passing_score, a.status, a.rubric_score_json, a.score, a.passed,
       a.evaluator_id, a.evaluation_method, a.evaluation_provenance_json,
       CASE
         WHEN a.trusted_evaluation = 1
          AND a.evaluation_method IN ('external_service','human_review','deterministic')
          AND (d.high_stakes = 0 OR a.evaluation_method = 'human_review')
         THEN 1 ELSE 0
       END AS trusted_evaluation,
       a.created_at, a.submitted_at, a.evaluated_at, a.cancelled_at,
       a.decision_id, a.curriculum_version, a.curriculum_concept_json, a.outcome_ids_json, a.curriculum_invalidated_version`

func (s *Store) CreateAssessmentAttempt(ctx context.Context, a *models.AssessmentAttempt) error {
	return s.inTx(ctx, nil, func(txs *Store) error { return txs.createAssessmentAttempt(ctx, a) })
}

func (s *Store) createAssessmentAttempt(ctx context.Context, a *models.AssessmentAttempt) error {
	if a == nil || a.ID == "" || a.LearnerID == "" || a.DomainID == "" ||
		a.ConceptID == "" || a.ActivityID == "" || a.ActivityVersion < 1 ||
		a.ActivityType == "" || a.Observable == "" || a.RubricJSON == "" {
		return fmt.Errorf("create assessment attempt: required field missing")
	}
	if a.TaskText == "" && a.TaskContentHash == "" {
		return fmt.Errorf("create assessment attempt: task text or content hash is required")
	}
	if a.Status == "" {
		a.Status = models.AssessmentAttemptPrepared
	}
	if a.Status != models.AssessmentAttemptPrepared {
		return fmt.Errorf("create assessment attempt: initial status must be prepared")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	version, err := s.lockCurriculumForEvidence(ctx, a.LearnerID, a.DomainID, a.ConceptID)
	if err != nil {
		return err
	}
	if a.CurriculumVersion > 0 && a.CurriculumVersion != version {
		return fmt.Errorf("create assessment attempt: stale curriculum")
	}
	scope, err := s.resolveLearningScope(ctx, a.LearnerID, a.DomainID, a.ConceptID)
	if err != nil {
		return err
	}
	if a.DecisionID != "" {
		if s.dialect == DialectPostgres {
			var version int
			if err := s.queryRow(ctx, `SELECT graph_version FROM domains WHERE id = ? AND learner_id = ? AND tenant_id = ? FOR SHARE`,
				a.DomainID, a.LearnerID, scope.TenantID).Scan(&version); err != nil {
				return err
			}
			if version != a.CurriculumVersion {
				return fmt.Errorf("create assessment attempt: stale curriculum")
			}
		}
		var payload string
		if err := s.queryRow(ctx, `SELECT p.snapshot_json FROM pedagogical_decisions p
		 JOIN domains d ON d.id = p.domain_id AND d.graph_version = p.curriculum_version
		 WHERE p.id = ? AND p.tenant_id = ? AND p.learner_id = ? AND p.domain_id = ? AND p.session_id = ?`,
			a.DecisionID, scope.TenantID, a.LearnerID, a.DomainID, a.SessionID).Scan(&payload); err != nil {
			return fmt.Errorf("create assessment attempt: decision unavailable or stale: %w", err)
		}
		var decision models.PedagogicalDecision
		if err := json.Unmarshal([]byte(payload), &decision); err != nil {
			return err
		}
		competency, err := json.Marshal(decision.Contract.Competency)
		if err != nil {
			return err
		}
		if decision.Contract.TargetConcept != a.ConceptID || string(decision.Contract.RecommendedActivityType) != a.ActivityType ||
			decision.CurriculumVersion != a.CurriculumVersion || string(competency) != a.CurriculumConceptJSON {
			return fmt.Errorf("create assessment attempt: decision contract mismatch")
		}
	}
	if a.OutcomeIDsJSON == "" {
		a.OutcomeIDsJSON = "[]"
	}
	_, err = s.exec(ctx, `INSERT INTO assessment_attempts
		(id, learner_id, domain_id, concept_id, tenant_id, enrollment_id, formation_concept_id,
		 session_id, activity_id,
		 activity_version, activity_type, observable, task_text,
		 task_content_hash, rubric_json, passing_score, status, created_at,
		 decision_id, curriculum_version, curriculum_concept_json, outcome_ids_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.LearnerID, a.DomainID, a.ConceptID,
		scope.TenantID, scope.EnrollmentID, scope.FormationConceptID, nullString(a.SessionID),
		a.ActivityID, a.ActivityVersion, a.ActivityType, a.Observable,
		nullString(a.TaskText), a.TaskContentHash, a.RubricJSON, a.PassingScore,
		string(a.Status), a.CreatedAt.UTC(), nullString(a.DecisionID), a.CurriculumVersion, a.CurriculumConceptJSON, a.OutcomeIDsJSON)
	if err != nil {
		return fmt.Errorf("create assessment attempt: %w", err)
	}
	return nil
}

func (s *Store) GetAssessmentAttempt(ctx context.Context, learnerID, attemptID string) (*models.AssessmentAttempt, error) {
	return s.getAssessmentAttempt(ctx, learnerID, attemptID, false)
}

func (s *Store) GetAssessmentAttemptForUpdate(ctx context.Context, learnerID, attemptID string) (*models.AssessmentAttempt, error) {
	// Discover scope without locking the attempt, then serialize with domain
	// revision before taking its row lock. The final read sees invalidation.
	attempt, err := s.getAssessmentAttempt(ctx, learnerID, attemptID, false)
	if err != nil {
		return nil, err
	}
	if _, err := s.lockCurriculumForEvidence(ctx, learnerID, attempt.DomainID, attempt.ConceptID); err != nil {
		return nil, err
	}
	return s.getAssessmentAttempt(ctx, learnerID, attemptID, true)
}

func (s *Store) getAssessmentAttempt(ctx context.Context, learnerID, attemptID string, forUpdate bool) (*models.AssessmentAttempt, error) {
	query := `SELECT ` + assessmentColumns + ` FROM assessment_attempts WHERE id = ? AND learner_id = ?`
	if forUpdate && s.dialect == DialectPostgres {
		query += ` FOR UPDATE`
	}
	return scanAssessmentAttempt(s.queryRow(ctx, query, attemptID, learnerID))
}

func scanAssessmentAttempt(row *sql.Row) (*models.AssessmentAttempt, error) {
	return scanAssessmentValues(row)
}

func scanAssessmentValues(scanner assessmentScanner) (*models.AssessmentAttempt, error) {
	a := &models.AssessmentAttempt{}
	var sessionID, taskText, responseText, responseHash, scoreJSON, evaluatorID, method, provenance, decisionID sql.NullString
	var submittedAt, evaluatedAt, cancelledAt sql.NullTime
	var passed, trusted int
	if err := scanner.Scan(
		&a.ID, &a.LearnerID, &a.DomainID, &a.ConceptID, &sessionID,
		&a.ActivityID, &a.ActivityVersion, &a.ActivityType, &a.Observable,
		&taskText, &a.TaskContentHash, &responseText, &responseHash,
		&a.RubricJSON, &a.PassingScore, &a.Status, &scoreJSON, &a.Score, &passed,
		&evaluatorID, &method, &provenance, &trusted, &a.CreatedAt,
		&submittedAt, &evaluatedAt, &cancelledAt,
		&decisionID, &a.CurriculumVersion, &a.CurriculumConceptJSON, &a.OutcomeIDsJSON, &a.CurriculumInvalidatedVersion,
	); err != nil {
		return nil, fmt.Errorf("get assessment attempt: %w", err)
	}
	a.SessionID = sessionID.String
	a.DecisionID = decisionID.String
	a.TaskText = taskText.String
	a.ResponseText = responseText.String
	a.ResponseContentHash = responseHash.String
	a.RubricScoreJSON = scoreJSON.String
	a.EvaluatorID = evaluatorID.String
	a.EvaluationMethod = models.EvaluationMethod(method.String)
	a.EvaluationProvenanceJSON = provenance.String
	a.Passed = passed != 0
	a.TrustedEvaluation = trusted != 0
	if submittedAt.Valid {
		t := submittedAt.Time
		a.SubmittedAt = &t
	}
	if evaluatedAt.Valid {
		t := evaluatedAt.Time
		a.EvaluatedAt = &t
	}
	if cancelledAt.Valid {
		t := cancelledAt.Time
		a.CancelledAt = &t
	}
	return a, nil
}

func (s *Store) SubmitAssessmentAttempt(ctx context.Context, learnerID, attemptID, responseText, responseHash string, now time.Time) error {
	if responseText == "" && responseHash == "" {
		return fmt.Errorf("submit assessment attempt: response text or content hash is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := s.exec(ctx, `UPDATE assessment_attempts
        SET response_text = ?, response_content_hash = ?, status = 'submitted', submitted_at = ?
        WHERE id = ? AND learner_id = ? AND status = 'prepared' AND curriculum_invalidated_version = 0`,
		nullString(responseText), responseHash, now.UTC(), attemptID, learnerID)
	if err != nil {
		return fmt.Errorf("submit assessment attempt: %w", err)
	}
	return requireAssessmentTransition(res)
}

// CompleteAssessmentEvaluation is the persistence port exposed to MCP tools.
// It can record host grading for routing, but can never mint trusted evidence.
func (s *Store) CompleteAssessmentEvaluation(ctx context.Context, learnerID, attemptID, rubricScoreJSON, evaluatorID string, method models.EvaluationMethod, provenanceJSON string, score float64, passed bool, now time.Time) error {
	if method != models.EvaluationMethodHostLLM {
		return fmt.Errorf("complete assessment evaluation: public evaluator must be host_llm")
	}
	return s.completeUntrustedAssessmentEvaluation(ctx, learnerID, attemptID, rubricScoreJSON, evaluatorID, method, provenanceJSON, score, passed, now)
}

func (s *Store) completeUntrustedAssessmentEvaluation(ctx context.Context, learnerID, attemptID, rubricScoreJSON, evaluatorID string, method models.EvaluationMethod, provenanceJSON string, score float64, passed bool, now time.Time) error {
	if rubricScoreJSON == "" || evaluatorID == "" || method == "" {
		return fmt.Errorf("complete assessment evaluation: scoring and evaluator identity are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := s.exec(ctx, `UPDATE assessment_attempts
        SET rubric_score_json = ?, evaluator_id = ?, evaluation_method = ?,
            evaluation_provenance_json = ?, score = ?, passed = ?,
            trusted_evaluation = ?, status = 'evaluated', evaluated_at = ?
        WHERE id = ? AND learner_id = ? AND status = 'submitted' AND curriculum_invalidated_version = 0`,
		rubricScoreJSON, evaluatorID, string(method), nullString(provenanceJSON),
		score, boolToInt(passed), 0, now.UTC(), attemptID, learnerID)
	if err != nil {
		return fmt.Errorf("complete assessment evaluation: %w", err)
	}
	return requireAssessmentTransition(res)
}

func (s *Store) CancelAssessmentAttempt(ctx context.Context, learnerID, attemptID string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := s.exec(ctx, `UPDATE assessment_attempts
        SET status = 'cancelled', cancelled_at = ?
        WHERE id = ? AND learner_id = ? AND status IN ('prepared', 'submitted')`,
		now.UTC(), attemptID, learnerID)
	if err != nil {
		return fmt.Errorf("cancel assessment attempt: %w", err)
	}
	return requireAssessmentTransition(res)
}

// GetEvaluatedAssessmentAttemptsInDomain returns assessment envelopes that
// completed the prepare -> submit -> evaluate protocol, including failures and
// untrusted host evaluations. The trusted_evaluation value exposed to callers
// is deliberately recomputed here: a raw/stale trusted bit cannot bypass the
// supported-method allow-list or the human-only high-stakes policy.
func (s *Store) GetEvaluatedAssessmentAttemptsInDomain(ctx context.Context, learnerID, domainID, conceptID string, limit int) ([]*models.AssessmentAttempt, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.query(ctx, `SELECT `+assessmentEvidenceColumns+` FROM assessment_attempts a
		JOIN domains d ON d.id = a.domain_id AND d.learner_id = a.learner_id
		WHERE a.learner_id = ? AND a.domain_id = ? AND a.concept_id = ?
		  AND a.status = 'evaluated' AND a.curriculum_invalidated_version = 0
		  AND a.submitted_at IS NOT NULL AND a.evaluated_at IS NOT NULL
		ORDER BY a.evaluated_at DESC LIMIT ?`, learnerID, domainID, conceptID, limit)
	if err != nil {
		return nil, fmt.Errorf("get evaluated assessment attempts: %w", err)
	}
	defer rows.Close()
	var out []*models.AssessmentAttempt
	for rows.Next() {
		a, scanErr := scanAssessmentValues(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evaluated assessment attempts: %w", err)
	}
	return out, nil
}

func requireAssessmentTransition(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("assessment transition result: %w", err)
	}
	if n != 1 {
		return ErrAssessmentStateConflict
	}
	return nil
}

func (s *Store) GetTrustedPassedAssessmentAttemptsInDomain(ctx context.Context, learnerID, domainID, conceptID string, limit int) ([]*models.AssessmentAttempt, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.query(ctx, `SELECT `+assessmentEvidenceColumns+` FROM assessment_attempts a
		JOIN domains d ON d.id = a.domain_id AND d.learner_id = a.learner_id
		WHERE a.learner_id = ? AND a.domain_id = ? AND a.concept_id = ?
		  AND a.status = 'evaluated' AND a.passed = 1 AND a.trusted_evaluation = 1 AND a.curriculum_invalidated_version = 0
		  AND a.evaluation_method IN ('external_service','human_review','deterministic')
		  AND (d.high_stakes = 0 OR a.evaluation_method = 'human_review')
		ORDER BY a.evaluated_at DESC LIMIT ?`, learnerID, domainID, conceptID, limit)
	if err != nil {
		return nil, fmt.Errorf("get trusted assessment attempts: %w", err)
	}
	defer rows.Close()
	var out []*models.AssessmentAttempt
	for rows.Next() {
		// sql.Rows and sql.Row have deliberately different Scan-bearing types;
		// keep the list scanner local and explicit.
		a, err := scanAssessmentValues(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trusted assessment attempts: %w", err)
	}
	return out, nil
}

// HasHumanReviewedEvaluationInDomain is the sole high-stakes review gate. It
// requires a trusted, completed human_review record and verifies domain
// ownership in the same query; host-LLM and external-service labels never pass.
func (s *Store) HasHumanReviewedEvaluationInDomain(ctx context.Context, learnerID, domainID string) (bool, error) {
	var count int
	err := s.queryRow(ctx,
		`SELECT COUNT(*)
		 FROM assessment_attempts a
		 JOIN domains d ON d.id = a.domain_id AND d.learner_id = a.learner_id
		 WHERE a.learner_id = ? AND a.domain_id = ?
		   AND d.learner_id = ? AND d.archived = 0 AND d.deleted_at IS NULL
		   AND a.status = 'evaluated' AND a.trusted_evaluation = 1 AND a.curriculum_invalidated_version = 0
		   AND a.evaluation_method = 'human_review'`,
		learnerID, domainID, learnerID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check human-reviewed evaluation: %w", err)
	}
	return count > 0, nil
}

type assessmentScanner interface {
	Scan(dest ...any) error
}
