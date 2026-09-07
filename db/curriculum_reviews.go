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
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (s *Store) curriculumReviewDomain(ctx context.Context, actor models.Principal, domainID string, lock bool) (string, int, error) {
	if actor.Validate() != nil || !models.OAuthScopeAllows(strings.Join(actor.Scopes, " "), models.OAuthScopeLearnerRead) ||
		slicesContains(actor.Roles, models.RoleServiceAccount) || slicesContains(actor.Roles, models.RoleSupport) {
		return "", 0, storeport.ErrInvalidPrincipal
	}
	if err := s.ValidatePrincipal(ctx, actor); err != nil {
		return "", 0, err
	}
	query := `SELECT d.learner_id,d.graph_version FROM domains d JOIN learners l ON l.id = d.learner_id AND l.tenant_id = d.tenant_id
 WHERE d.id = ? AND d.tenant_id = ? AND l.user_id <> ?`
	args := []any{domainID, actor.TenantID, actor.UserID}
	if !actor.Authorize(models.PermissionCurriculumReview, models.AuthorizationResource{TenantID: actor.TenantID}) {
		if !slicesContains(actor.Roles, models.RoleTrainer) {
			return "", 0, storeport.ErrInvalidPrincipal
		}
		query += ` AND EXISTS (SELECT 1 FROM legacy_domain_enrollments de JOIN enrollments e ON e.tenant_id = de.tenant_id AND e.id = de.enrollment_id
 JOIN cohort_trainers ct ON ct.tenant_id = e.tenant_id AND ct.cohort_id = e.cohort_id
 WHERE de.tenant_id = d.tenant_id AND de.domain_id = d.id AND ct.membership_id = ?)`
		args = append(args, actor.MembershipID)
	}
	if lock {
		query += ` AND d.archived = 0 AND d.deleted_at IS NULL`
		if s.dialect == DialectPostgres {
			query += ` FOR SHARE OF d`
		}
	}
	var learnerID string
	var version int
	err := s.queryRow(ctx, query, args...).Scan(&learnerID, &version)
	return learnerID, version, adjudicationReadError(err)
}

func (s *Store) GetCurriculumReviewMaterial(ctx context.Context, actor models.Principal, domainID string, version int) (*models.CurriculumReviewMaterial, error) {
	if version < 1 || domainID == "" || len(domainID) > 255 {
		return nil, storeport.ErrInvalidAssessmentReviewRequest
	}
	var material *models.CurriculumReviewMaterial
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		learnerID, current, err := txs.curriculumReviewDomain(txCtx, actor, domainID, false)
		if err != nil {
			return err
		}
		material, err = txs.curriculumReviewMaterial(txCtx, learnerID, domainID, version, current)
		return err
	})
	return material, err
}

func (s *Store) curriculumReviewMaterial(ctx context.Context, learnerID, domainID string, version, current int) (*models.CurriculumReviewMaterial, error) {
	snapshot, err := s.GetCurriculumSnapshot(ctx, learnerID, domainID, version)
	if err != nil {
		return nil, adjudicationReadError(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return &models.CurriculumReviewMaterial{Snapshot: snapshot, MaterialHash: reviewDigest(append([]byte("curriculum-review-v1\n"), raw...)), CurrentVersion: current}, nil
}

func (s *Store) RecordCurriculumReview(ctx context.Context, actor models.Principal, domainID string, version int, key, materialHash, rawFindings string) (*models.CurriculumReviewOpinion, bool, error) {
	if !models.OAuthScopeAllows(strings.Join(actor.Scopes, " "), models.OAuthScopeLearnerWrite) {
		return nil, false, storeport.ErrInvalidPrincipal
	}
	if version < 1 || len(domainID) > 255 || len(key) == 0 || len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsRune(key, 0) {
		return nil, false, storeport.ErrInvalidAssessmentReviewRequest
	}
	var result *models.CurriculumReviewOpinion
	replayed := false
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		learnerID, current, err := txs.curriculumReviewDomain(txCtx, actor, domainID, true)
		if err != nil {
			return err
		}
		if current != version {
			return storeport.ErrAssessmentReviewConflict
		}
		material, err := txs.curriculumReviewMaterial(txCtx, learnerID, domainID, version, current)
		if err != nil {
			return err
		}
		if material.MaterialHash != materialHash {
			return storeport.ErrAssessmentReviewMaterialChanged
		}
		findings, disposition, reviewed, total, err := assessment.ValidateCurriculumFindings(material.Snapshot, rawFindings)
		if err != nil {
			return storeport.ErrInvalidAssessmentReviewRequest
		}
		canonical, err := json.Marshal(findings)
		if err != nil {
			return err
		}
		if len(canonical) > assessment.MaxJSONBytes {
			return storeport.ErrInvalidAssessmentReviewRequest
		}
		hash, id := reviewDigest(canonical), "curriculum_review_"+rand.Text()
		insert, err := txs.exec(txCtx, `INSERT INTO curriculum_review_opinions
 (id,tenant_id,learner_id,domain_id,version,reviewer_user_id,reviewer_membership_id,idempotency_key,
 material_hash,findings_hash,findings_json,disposition,reviewed_sections,total_sections,created_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
			id, actor.TenantID, learnerID, domainID, version, actor.UserID, actor.MembershipID, key, materialHash, hash, string(canonical), disposition, reviewed, total, time.Now().UTC())
		if err != nil {
			return err
		}
		n, err := insert.RowsAffected()
		if err != nil {
			return err
		}
		var previousDomain, previousHash, previousMaterial string
		var previousVersion int
		if err := txs.queryRow(txCtx, `SELECT domain_id,version,findings_hash,material_hash FROM curriculum_review_opinions
 WHERE tenant_id = ? AND reviewer_user_id = ? AND idempotency_key = ?`, actor.TenantID, actor.UserID, key).Scan(&previousDomain, &previousVersion, &previousHash, &previousMaterial); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storeport.ErrAssessmentReviewConflict
			}
			return err
		}
		if previousDomain != domainID || previousVersion != version || previousHash != hash || previousMaterial != materialHash {
			return storeport.ErrAssessmentReviewConflict
		}
		result, err = txs.readOwnCurriculumReview(txCtx, actor, domainID, version)
		if err != nil {
			return err
		}
		replayed = n == 0
		if replayed {
			return nil
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "curriculum.review.record", TargetType: "curriculum_review", TargetID: id})
	})
	return result, replayed, err
}

func (s *Store) GetOwnCurriculumReview(ctx context.Context, actor models.Principal, domainID string, version int) (*models.CurriculumReviewOpinion, error) {
	var result *models.CurriculumReviewOpinion
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, _, err := txs.curriculumReviewDomain(txCtx, actor, domainID, false); err != nil {
			return err
		}
		var err error
		result, err = txs.readOwnCurriculumReview(txCtx, actor, domainID, version)
		return adjudicationReadError(err)
	})
	return result, err
}

func (s *Store) readOwnCurriculumReview(ctx context.Context, actor models.Principal, domainID string, version int) (*models.CurriculumReviewOpinion, error) {
	var r models.CurriculumReviewOpinion
	var raw sql.NullString
	err := s.queryRow(ctx, `SELECT id,domain_id,version,reviewer_user_id,material_hash,findings_hash,findings_json,disposition,reviewed_sections,total_sections,created_at
 FROM curriculum_review_opinions WHERE tenant_id = ? AND reviewer_user_id = ? AND domain_id = ? AND version = ?`, actor.TenantID, actor.UserID, domainID, version).Scan(
		&r.ID, &r.DomainID, &r.Version, &r.ReviewerUserID, &r.MaterialHash, &r.FindingsHash, &raw, &r.Disposition, &r.ReviewedSections, &r.TotalSections, &r.CreatedAt)
	if raw.Valid {
		r.Findings = json.RawMessage(raw.String)
	}
	return &r, err
}
