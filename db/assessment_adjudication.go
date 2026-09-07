// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"tutor-mcp/assessment"
	"tutor-mcp/certification"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// AssessmentAdjudicator is deliberately separate from Store's public MCP port.
// The authority verifier is fixed at bootstrap, not supplied by an HTTP caller.
type AssessmentAdjudicator struct {
	store    *Store
	verifier *certification.Verifier
}

func NewAssessmentAdjudicator(s *Store, verifier *certification.Verifier) *AssessmentAdjudicator {
	return &AssessmentAdjudicator{store: s, verifier: verifier}
}

func (a *AssessmentAdjudicator) AdjudicateAssessment(ctx context.Context, actor models.Principal, attemptID, certificate string) (*models.AssessmentAdjudication, bool, error) {
	if !actor.Authorize(models.PermissionAssessmentAdjudicate, models.AuthorizationResource{TenantID: actor.TenantID}) ||
		!models.OAuthScopeAllows(strings.Join(actor.Scopes, " "), models.OAuthScopeLearnerWrite) {
		return nil, false, storeport.ErrInvalidPrincipal
	}
	verified, err := a.verifier.Verify(certificate, time.Now().UTC())
	if err != nil {
		return nil, false, certification.ErrInvalid
	}
	c, authority := verified.Claims(), verified.Authority()
	if c.TenantID != actor.TenantID || c.AttemptID != attemptID {
		return nil, false, certification.ErrInvalid
	}
	var result *models.AssessmentAdjudication
	replayed := false
	err = a.store.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		s := scoped.(*Store)
		from, args, err := s.assessmentReviewAccess(txCtx, actor)
		if err != nil {
			return err
		}
		var learnerID string
		if err := s.queryRow(txCtx, `SELECT a.learner_id`+from+` AND a.id = ?`, append(args, attemptID)...).Scan(&learnerID); err != nil {
			return adjudicationReadError(err)
		}
		attempt, err := s.GetAssessmentAttemptForUpdate(txCtx, learnerID, attemptID)
		if err != nil {
			return err
		}
		if attempt.Status != models.AssessmentAttemptEvaluated || attempt.CurriculumInvalidatedVersion != 0 || attempt.EvaluationMethod != models.EvaluationMethodHostLLM {
			return storeport.ErrAssessmentReviewConflict
		}
		// A valid retry never creates a second disposition, even if another
		// disposition has since superseded it. Live permissions still apply.
		prior, err := s.readAdjudication(txCtx, actor.TenantID, ` AND j.authority_id = ? AND j.certificate_id = ?`, authority.AuthorityID, c.ID)
		if err == nil {
			if prior.AttemptID != attemptID || prior.CertificateHash != verified.Hash() {
				return storeport.ErrAssessmentReviewConflict
			}
			result, replayed = prior, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var revision int
		if err := s.queryRow(txCtx, `SELECT COALESCE(MAX(revision), 0) FROM assessment_adjudications WHERE tenant_id = ? AND attempt_id = ?`, actor.TenantID, attemptID).Scan(&revision); err != nil {
			return err
		}
		if revision != c.ExpectedRevision {
			return storeport.ErrAssessmentReviewConflict
		}
		var materialHash, scoreHash string
		var rawScore sql.NullString
		if err := s.queryRow(txCtx, `SELECT material_hash, rubric_score_hash, rubric_score_json FROM assessment_reviews
 WHERE tenant_id = ? AND attempt_id = ? AND id = ?`, actor.TenantID, attemptID, c.ReviewID).Scan(&materialHash, &scoreHash, &rawScore); err != nil {
			return adjudicationReadError(err)
		}
		if materialHash != c.MaterialHash || scoreHash != c.ScoreHash {
			return storeport.ErrAssessmentReviewMaterialChanged
		}
		// Rejection can withdraw evidence after plaintext retention. Acceptance
		// requires readable frozen material and recomputes the selected score.
		if c.Verdict == "accept" {
			material, err := a.store.GetAssessmentReviewMaterial(txCtx, actor, attemptID)
			if err != nil {
				return err
			}
			if !material.TextAvailable || !rawScore.Valid || material.MaterialHash != c.MaterialHash {
				return storeport.ErrAssessmentReviewMaterialUnavailable
			}
			score, err := assessment.EvaluateJSON(material.Rubric, rawScore.String)
			if err != nil {
				return storeport.ErrInvalidAssessmentReviewScore
			}
			canonical, err := json.Marshal(score.Score)
			if err != nil || reviewDigest(canonical) != c.ScoreHash {
				return storeport.ErrInvalidAssessmentReviewScore
			}
		}
		id, now := "adjudication_"+rand.Text(), time.Now().UTC()
		_, err = s.exec(txCtx, `INSERT INTO assessment_adjudications
 (id, tenant_id, learner_id, attempt_id, review_id, revision, verdict, authority_id, key_id, certificate_id,
 certificate_hash, actor_user_id, material_hash, score_hash, created_at, certificate_json, authority_public_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, actor.TenantID, learnerID, attemptID, c.ReviewID, revision+1, c.Verdict, authority.AuthorityID, authority.KeyID,
			c.ID, verified.Hash(), actor.UserID, c.MaterialHash, c.ScoreHash, now, certificate, authority.PublicKey)
		if err != nil {
			return err
		}
		if err := s.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "assessment.adjudicate", TargetType: "assessment_adjudication", TargetID: id}); err != nil {
			return err
		}
		result, err = s.readAdjudication(txCtx, actor.TenantID, ` AND j.id = ?`, id)
		return err
	})
	return result, replayed, err
}

func (a *AssessmentAdjudicator) GetAssessmentAdjudication(ctx context.Context, actor models.Principal, attemptID string) (*models.AssessmentAdjudication, error) {
	if !actor.Authorize(models.PermissionAssessmentAdjudicate, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, storeport.ErrInvalidPrincipal
	}
	var result *models.AssessmentAdjudication
	err := a.store.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		s := scoped.(*Store)
		from, args, err := s.assessmentReviewAccess(txCtx, actor)
		if err != nil {
			return err
		}
		var visible string
		if err := s.queryRow(txCtx, `SELECT a.id`+from+` AND a.id = ?`, append(args, attemptID)...).Scan(&visible); err != nil {
			return adjudicationReadError(err)
		}
		result, err = s.readAdjudication(txCtx, actor.TenantID, ` AND j.attempt_id = ? ORDER BY j.revision DESC LIMIT 1`, attemptID)
		return adjudicationReadError(err)
	})
	return result, err
}

func adjudicationReadError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return storeport.WrapNotFound(err)
	}
	return err
}

func (s *Store) readAdjudication(ctx context.Context, tenantID, condition string, args ...any) (*models.AssessmentAdjudication, error) {
	var j models.AssessmentAdjudication
	err := s.queryRow(ctx, `SELECT j.id, j.attempt_id, j.review_id, j.revision, j.verdict, j.authority_id, j.key_id,
 j.certificate_id, j.certificate_hash, j.actor_user_id, j.material_hash, j.score_hash, j.created_at, a.curriculum_invalidated_version
 FROM assessment_adjudications j JOIN assessment_attempts a ON a.tenant_id = j.tenant_id AND a.id = j.attempt_id
 WHERE j.tenant_id = ?`+condition, append([]any{tenantID}, args...)...).Scan(
		&j.ID, &j.AttemptID, &j.ReviewID, &j.Revision, &j.Verdict, &j.AuthorityID, &j.KeyID,
		&j.CertificateID, &j.CertificateHash, &j.ActorUserID, &j.MaterialHash, &j.ScoreHash, &j.CreatedAt, &j.CurriculumInvalidatedVersion)
	return &j, err
}
