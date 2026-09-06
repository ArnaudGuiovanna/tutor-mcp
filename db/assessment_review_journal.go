// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"tutor-mcp/assessment"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// RecordAssessmentReview records only an opinion. It cannot certify trust,
// replace a host grade, or write interactions/BKT/FSRS/transfer evidence.
// Eligibility and current authorization are rechecked even for retries.
func (s *Store) RecordAssessmentReview(ctx context.Context, actor models.Principal, attemptID, key, materialHash, rawScore string) (*models.AssessmentReview, bool, error) {
	if actor.Validate() != nil || !models.OAuthScopeAllows(strings.Join(actor.Scopes, " "), models.OAuthScopeLearnerWrite) {
		return nil, false, storeport.ErrInvalidPrincipal
	}
	digest, err := hex.DecodeString(materialHash)
	if attemptID == "" || len(attemptID) > 255 || len(key) == 0 || len(key) > 128 || strings.TrimSpace(key) != key ||
		strings.ContainsRune(key, 0) || err != nil || len(digest) != 32 || materialHash != strings.ToLower(materialHash) {
		return nil, false, storeport.ErrInvalidAssessmentReviewRequest
	}
	var review *models.AssessmentReview
	replayed := false
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		from, args, err := txs.assessmentReviewAccess(txCtx, actor)
		if err != nil {
			return err
		}
		var learnerID, domainID string
		if err := txs.queryRow(txCtx, `SELECT a.learner_id, a.domain_id`+from+assessmentReviewEligible+` AND a.id = ?`, append(args, attemptID)...).Scan(&learnerID, &domainID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storeport.WrapNotFound(err)
			}
			return err
		}
		// Same lock order as curriculum reconciliation and host evaluation. Read
		// material again after locking: a revision/redaction may have won the race.
		if txs.dialect == DialectPostgres {
			if err := txs.queryRow(txCtx, `SELECT id FROM domains WHERE tenant_id = ? AND id = ? FOR SHARE`, actor.TenantID, domainID).Scan(&domainID); err != nil {
				return err
			}
			var lockedID string
			if err := txs.queryRow(txCtx, `SELECT id FROM assessment_attempts WHERE tenant_id = ? AND id = ? FOR UPDATE`, actor.TenantID, attemptID).Scan(&lockedID); err != nil {
				return storeport.WrapNotFound(err)
			}
		}
		material, err := s.GetAssessmentReviewMaterial(txCtx, actor, attemptID)
		if err != nil {
			return err
		}
		if !material.TextAvailable {
			return storeport.ErrAssessmentReviewMaterialUnavailable
		}
		if material.MaterialHash != materialHash {
			return storeport.ErrAssessmentReviewMaterialChanged
		}
		result, err := assessment.EvaluateJSON(material.Rubric, rawScore)
		if err != nil {
			return storeport.ErrInvalidAssessmentReviewScore
		}
		canonical, err := json.Marshal(result.Score)
		if err != nil {
			return storeport.ErrInvalidAssessmentReviewScore
		}
		scoreHash := reviewDigest(canonical)
		id := "review_" + rand.Text()
		passed := 0
		if result.Passed {
			passed = 1
		}
		insert, err := txs.exec(txCtx, `INSERT INTO assessment_reviews
 (id, tenant_id, learner_id, attempt_id, reviewer_user_id, reviewer_membership_id, reviewer_token_version,
 idempotency_key, material_hash, rubric_score_hash, rubric_score_json, total, passed, created_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			id, actor.TenantID, learnerID, attemptID, actor.UserID, actor.MembershipID, actor.TokenVersion,
			key, materialHash, scoreHash, string(canonical), result.Total, passed, time.Now().UTC())
		if err != nil {
			return err
		}
		n, err := insert.RowsAffected()
		if err != nil {
			return err
		}
		var recordedAttempt, recordedHash, recordedScoreHash string
		if err := txs.queryRow(txCtx, `SELECT attempt_id, material_hash, rubric_score_hash FROM assessment_reviews
 WHERE tenant_id = ? AND reviewer_user_id = ? AND idempotency_key = ?`, actor.TenantID, actor.UserID, key).Scan(&recordedAttempt, &recordedHash, &recordedScoreHash); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storeport.ErrAssessmentReviewConflict
			}
			return err
		}
		if recordedAttempt != attemptID || recordedHash != materialHash || recordedScoreHash != scoreHash {
			return storeport.ErrAssessmentReviewConflict
		}
		replayed = n == 0
		review, err = s.GetOwnAssessmentReview(txCtx, actor, attemptID)
		if err != nil {
			return err
		}
		if replayed {
			return nil
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "assessment.review.record", TargetType: "assessment_review", TargetID: id,
		})
	})
	if err != nil {
		return nil, false, err
	}
	return review, replayed, nil
}

// GetOwnAssessmentReview exposes only the requesting account's opinion. It
// remains historical after curriculum invalidation, but access is still live.
func (s *Store) GetOwnAssessmentReview(ctx context.Context, actor models.Principal, attemptID string) (*models.AssessmentReview, error) {
	if actor.Validate() != nil {
		return nil, storeport.ErrInvalidPrincipal
	}
	if attemptID == "" || len(attemptID) > 255 {
		return nil, storeport.ErrInvalidAssessmentReviewRequest
	}
	var review models.AssessmentReview
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		from, args, err := txs.assessmentReviewAccess(txCtx, actor)
		if err != nil {
			return err
		}
		var raw sql.NullString
		var passed, trusted int
		err = txs.queryRow(txCtx, `SELECT r.id, r.attempt_id, r.reviewer_user_id, r.reviewer_membership_id,
 r.reviewer_token_version, r.material_hash, r.rubric_score_hash, r.rubric_score_json, r.total,
 r.passed, r.trusted_evaluation, r.created_at, a.curriculum_invalidated_version
 FROM assessment_reviews r JOIN assessment_attempts a ON a.id = r.attempt_id AND a.tenant_id = r.tenant_id
 WHERE r.tenant_id = ? AND r.reviewer_user_id = ? AND r.attempt_id = ?
 AND EXISTS (SELECT 1`+from+` AND a.id = r.attempt_id)`, append([]any{actor.TenantID, actor.UserID, attemptID}, args...)...).Scan(
			&review.ID, &review.AttemptID, &review.ReviewerUserID, &review.ReviewerMembershipID, &review.ReviewerTokenVersion,
			&review.MaterialHash, &review.RubricScoreHash, &raw, &review.Total, &passed, &trusted, &review.CreatedAt, &review.CurriculumInvalidatedVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return storeport.WrapNotFound(err)
		}
		if err != nil {
			return err
		}
		if raw.Valid {
			review.RubricScore = json.RawMessage(raw.String)
		}
		review.Passed, review.TrustedEvaluation = passed == 1, trusted == 1
		review.CreatedAt = review.CreatedAt.UTC()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &review, nil
}
