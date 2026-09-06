// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"tutor-mcp/assessment"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Shared access predicate, also used for the actor's historical opinion.
// Listing, material and scoring add the eligibility predicate below. Cohort
// access follows the attempt's enrollment, not the domain's current enrollment.
const assessmentReviewFrom = ` FROM assessment_attempts a
 JOIN learners l ON l.id = a.learner_id AND l.tenant_id = a.tenant_id
 LEFT JOIN enrollments e ON e.id = a.enrollment_id AND e.tenant_id = a.tenant_id
   AND e.learner_id = a.learner_id
 WHERE a.tenant_id = ? AND l.user_id <> ?`

const assessmentReviewEligible = ` AND a.decision_id IS NOT NULL AND a.curriculum_invalidated_version = 0
   AND a.trusted_evaluation = 0 AND a.submitted_at IS NOT NULL
   AND (a.status = 'submitted' OR (a.status = 'evaluated' AND a.evaluation_method = 'host_llm'))`

// assessmentReviewAccess runs inside the tenant transaction, rechecking current
// membership/MFA/version rather than trusting stale roles from an API caller.
func (s *Store) assessmentReviewAccess(ctx context.Context, actor models.Principal) (string, []any, error) {
	if !models.OAuthScopeAllows(strings.Join(actor.Scopes, " "), models.OAuthScopeLearnerRead) ||
		slicesContains(actor.Roles, models.RoleServiceAccount) || slicesContains(actor.Roles, models.RoleSupport) {
		return "", nil, storeport.ErrInvalidPrincipal
	}
	if err := s.ValidatePrincipal(ctx, actor); err != nil {
		return "", nil, err
	}
	args := []any{actor.TenantID, actor.UserID}
	if actor.Authorize(models.PermissionAssessmentReview, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return assessmentReviewFrom, args, nil
	}
	if !slicesContains(actor.Roles, models.RoleTrainer) {
		return "", nil, storeport.ErrInvalidPrincipal
	}
	return assessmentReviewFrom + ` AND EXISTS (
 SELECT 1 FROM cohort_trainers ct WHERE ct.tenant_id = a.tenant_id
   AND ct.cohort_id = e.cohort_id AND ct.membership_id = ?)`, append(args, actor.MembershipID), nil
}

// ListAssessmentReviewCandidates reads a bounded queue; it does not assign or
// reserve a reviewer. It is intentionally absent from the public MCP store port.
func (s *Store) ListAssessmentReviewCandidates(ctx context.Context, actor models.Principal, cohortID, after string, limit int) (models.AssessmentReviewPage, error) {
	page := models.AssessmentReviewPage{Items: []models.AssessmentReviewItem{}}
	if actor.Validate() != nil {
		return page, storeport.ErrInvalidPrincipal
	}
	if validatePage(limit) != nil || len(after) > 255 || len(cohortID) > 255 {
		return page, storeport.ErrInvalidAssessmentReviewRequest
	}
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		from, args, err := txs.assessmentReviewAccess(txCtx, actor)
		if err != nil {
			return err
		}
		if cohortID != "" {
			from += ` AND e.cohort_id = ?`
			args = append(args, cohortID)
		}
		from += assessmentReviewEligible + ` AND NOT EXISTS (SELECT 1 FROM assessment_reviews r
 WHERE r.tenant_id = a.tenant_id AND r.attempt_id = a.id AND r.reviewer_user_id = ?)`
		args = append(args, actor.UserID)
		rows, err := txs.query(txCtx, `SELECT a.id, a.activity_type, a.curriculum_version, a.submitted_at`+
			from+` AND a.id > ? ORDER BY a.id LIMIT ?`, append(args, after, limit+1)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item models.AssessmentReviewItem
			if err := rows.Scan(&item.AttemptID, &item.ActivityType, &item.CurriculumVersion, &item.SubmittedAt); err != nil {
				return err
			}
			page.Items = append(page.Items, item)
		}
		return rows.Err()
	})
	if err != nil {
		return models.AssessmentReviewPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = page.Items[len(page.Items)-1].AttemptID
	}
	return page, nil
}

func (s *Store) GetAssessmentReviewMaterial(ctx context.Context, actor models.Principal, attemptID string) (*models.AssessmentReviewMaterial, error) {
	if actor.Validate() != nil {
		return nil, storeport.ErrInvalidPrincipal
	}
	if attemptID == "" || len(attemptID) > 255 {
		return nil, storeport.ErrInvalidAssessmentReviewRequest
	}
	var material models.AssessmentReviewMaterial
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		from, args, err := txs.assessmentReviewAccess(txCtx, actor)
		if err != nil {
			return err
		}
		var rubricJSON, competencyJSON, outcomesJSON string
		var passingScore float64
		err = txs.queryRow(txCtx, `SELECT a.id, a.activity_type, a.curriculum_version, a.submitted_at,
 a.decision_id, a.activity_id, a.activity_version, a.observable, a.curriculum_concept_json,
 a.outcome_ids_json, COALESCE(a.task_text, ''), a.task_content_hash,
 COALESCE(a.response_text, ''), COALESCE(a.response_content_hash, ''), a.rubric_json, a.passing_score, a.created_at`+
			from+assessmentReviewEligible+` AND a.id = ?`, append(args, attemptID)...).Scan(
			&material.AttemptID, &material.ActivityType, &material.CurriculumVersion, &material.SubmittedAt,
			&material.DecisionID, &material.ActivityID, &material.ActivityVersion, &material.Observable, &competencyJSON,
			&outcomesJSON, &material.TaskText, &material.TaskContentHash, &material.ResponseText,
			&material.ResponseContentHash, &rubricJSON, &passingScore, &material.PreparedAt)
		if errors.Is(err, sql.ErrNoRows) {
			// Do not distinguish foreign, self, invalidated or missing attempts.
			return storeport.WrapNotFound(err)
		}
		if err != nil {
			return err
		}
		material.Rubric, err = assessment.ParseRubric(rubricJSON)
		if err != nil || material.Rubric.PassingScore != passingScore ||
			json.Unmarshal([]byte(competencyJSON), &material.Competency) != nil ||
			json.Unmarshal([]byte(outcomesJSON), &material.OutcomeIDs) != nil || material.Competency.ID == "" {
			return storeport.ErrAssessmentReviewMaterialUnavailable
		}
		if material.OutcomeIDs == nil {
			material.OutcomeIDs = []string{}
		}
		material.TextAvailable = strings.TrimSpace(material.TaskText) != "" && strings.TrimSpace(material.ResponseText) != ""
		// Hash the actual frozen projection, not caller-supplied content hashes.
		// Normalize instants across SQLite/PostgreSQL before serializing. The hash
		// field is empty in this versioned preimage, so it is not self-referential.
		material.PreparedAt = material.PreparedAt.UTC()
		material.SubmittedAt = material.SubmittedAt.UTC()
		encoded, err := json.Marshal(material)
		if err != nil {
			return storeport.ErrAssessmentReviewMaterialUnavailable
		}
		material.MaterialHash = reviewDigest(append([]byte("assessment-review-material-v1\n"), encoded...))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &material, nil
}

func reviewDigest(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
