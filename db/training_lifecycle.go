// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Archived resources remain inspectable by their author. Existing common
// enrollments keep their immutable published version and their evidence.
func (s *Store) trainingArchivedLibrary(ctx context.Context, p models.Principal, in models.TrainingRequest) (models.TrainingPage, error) {
	page := models.TrainingPage{Items: []models.Training{}}
	learner := p.LearnerID
	if !models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeLearnerRead) {
		learner = ""
	}
	q := `SELECT id FROM (
	SELECT 'personal:'||d.id AS id,d.name FROM domains d WHERE d.tenant_id=? AND d.learner_id=? AND d.archived=1 AND d.deleted_at IS NULL AND COALESCE(d.formation_enrollment_id,'')=''
	UNION ALL SELECT 'formation:'||f.id AS id,f.name FROM formations f WHERE f.tenant_id=? AND ` + institutionalFormationSQL + ` AND f.status='archived' AND
	(?=1 OR f.created_by=? OR EXISTS(SELECT 1 FROM formation_trainers ft WHERE ft.tenant_id=f.tenant_id AND ft.formation_id=f.id AND ft.membership_id=?))) resources
	WHERE id>? AND lower(name) LIKE ? ORDER BY id LIMIT ?`
	rows, err := s.query(ctx, q, p.TenantID, learner, p.TenantID, boolInt(trainingOwner(p)), p.UserID, p.MembershipID, in.After, "%"+strings.ToLower(in.Query)+"%", in.Limit+1)
	if err != nil {
		return page, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return page, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(ids) > in.Limit {
		ids = ids[:in.Limit]
		page.NextAfter = ids[len(ids)-1]
	}
	for _, id := range ids {
		doc, err := s.trainingDocument(ctx, p, id)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, doc.Training)
	}
	return page, nil
}

func (s *Store) trainingLifecycle(ctx context.Context, p models.Principal, action string, in models.TrainingRequest) (any, error) {
	if domain, ok := strings.CutPrefix(in.ID, "personal:"); ok && s.dialect == DialectPostgres {
		var locked string
		if err := s.queryRow(ctx, `SELECT id FROM domains WHERE tenant_id=? AND learner_id=? AND id=? AND deleted_at IS NULL FOR UPDATE`, p.TenantID, p.LearnerID, domain).Scan(&locked); err != nil {
			return nil, storeport.ErrFormationUnavailable
		}
	}
	doc, err := s.trainingDocument(ctx, p, in.ID)
	if err != nil {
		return nil, err
	}
	t := doc.Training
	requested := action
	if action == "preview_training_lifecycle" {
		requested = in.Decision
	}
	allowed := map[string]bool{"archive_training": t.CanArchive, "restore_training": t.CanRestore, "delete_training": t.CanDelete, "withdraw_training": t.CanWithdraw}
	if !allowed[requested] {
		return nil, storeport.ErrInvalidPrincipal
	}
	var conversations, enrolled int
	if err = s.queryRow(ctx, `SELECT COUNT(*) FROM conversation_contexts WHERE tenant_id=? AND training_id=?`, p.TenantID, t.ID).Scan(&conversations); err != nil {
		return nil, err
	}
	if t.FormationID != "" {
		if err = s.queryRow(ctx, `SELECT COUNT(*) FROM enrollments e JOIN formation_versions v ON v.tenant_id=e.tenant_id AND v.id=e.formation_version_id WHERE v.tenant_id=? AND v.formation_id=? AND e.status IN ('active','completed')`, p.TenantID, t.FormationID).Scan(&enrolled); err != nil {
			return nil, err
		}
	}
	effects := map[string]any{"enrolled_learners": enrolled, "conversations_affected": conversations, "preserves_learning_history": true, "will_archive": requested == "archive_training" || requested == "withdraw_training"}
	if action == "preview_training_lifecycle" {
		return map[string]any{"id": t.ID, "action": requested, "revision": t.Revision, "can_apply": true, "requires_confirmation": true, "effects": effects}, nil
	}
	if !in.Confirm {
		return nil, fmt.Errorf("explicit confirmation required")
	}
	if in.ExpectedRevision == "" || in.ExpectedRevision != t.Revision {
		return nil, storeport.ErrCurriculumVersionConflict
	}
	status := "archived"
	if t.Kind == "personal" {
		switch requested {
		case "archive_training":
			err = s.ArchiveDomain(ctx, t.DomainID, p.LearnerID)
		case "restore_training":
			err = s.UnarchiveDomain(ctx, t.DomainID, p.LearnerID)
			status = "active"
		case "delete_training":
			err = s.DeleteDomain(ctx, t.DomainID, p.LearnerID)
			status = "deleted"
		}
	} else {
		// All catalogue mutations serialize before changing version children.
		query := `SELECT status FROM formations WHERE tenant_id=? AND id=?`
		if s.dialect == DialectPostgres {
			query += " FOR UPDATE"
		}
		var current string
		if err = s.queryRow(ctx, query, p.TenantID, t.FormationID).Scan(&current); err != nil {
			return nil, err
		}
		fresh, readErr := s.trainingDocument(ctx, p, in.ID)
		if readErr != nil {
			return nil, readErr
		}
		if fresh.Training.Revision != in.ExpectedRevision || fresh.Training.Status != t.Status {
			return nil, storeport.ErrCurriculumVersionConflict
		}
		switch requested {
		case "delete_training":
			err = s.deleteUnpublishedTraining(ctx, p.TenantID, t.FormationID)
			status = "deleted"
		case "restore_training":
			status = "draft"
			var published int
			if err = s.queryRow(ctx, `SELECT COUNT(*) FROM formation_versions WHERE tenant_id=? AND formation_id=? AND status='published'`, p.TenantID, t.FormationID).Scan(&published); err != nil {
				return nil, err
			}
			if published > 0 {
				status = "active"
			}
			_, err = s.exec(ctx, `UPDATE formations SET status=?,updated_at=? WHERE tenant_id=? AND id=?`, status, time.Now().UTC(), p.TenantID, t.FormationID)
		default:
			_, err = s.exec(ctx, `UPDATE formations SET status='archived',updated_at=? WHERE tenant_id=? AND id=?`, time.Now().UTC(), p.TenantID, t.FormationID)
		}
	}
	if err != nil {
		return nil, err
	}
	if requested != "restore_training" {
		// Clear only the current selection. Historical messages, snapshots and
		// assessment bindings remain untouched and must never be replayed.
		if t.Kind == "personal" {
			_, err = s.exec(ctx, `DELETE FROM conversation_contexts WHERE tenant_id=? AND membership_id=? AND training_id=?`, p.TenantID, p.MembershipID, t.ID)
		} else if status == "deleted" {
			_, err = s.exec(ctx, `DELETE FROM conversation_contexts WHERE tenant_id=? AND training_id=?`, p.TenantID, t.ID)
		}
		if err != nil {
			return nil, err
		}
	}
	if err = s.AppendAuditEvent(ctx, p, models.AuditEvent{Action: "training." + requested, TargetType: "training", TargetID: t.ID, Reason: in.Reason}); err != nil {
		return nil, err
	}
	return map[string]any{"id": t.ID, "status": status, "effects": effects}, nil
}

func (s *Store) deleteUnpublishedTraining(ctx context.Context, tenantID, formationID string) error {
	var published int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM formation_versions WHERE tenant_id=? AND formation_id=? AND status<>'draft'`, tenantID, formationID).Scan(&published); err != nil {
		return err
	}
	if published != 0 {
		return storeport.ErrInvalidPrincipal
	}
	for _, table := range []string{"training_submissions", "training_workflows"} {
		if _, err := s.exec(ctx, `DELETE FROM `+table+` WHERE tenant_id=? AND formation_id=?`, tenantID, formationID); err != nil {
			return err
		}
	}
	if _, err := s.exec(ctx, `DELETE FROM training_proposals WHERE tenant_id=? AND training_id=?`, tenantID, "formation:"+formationID); err != nil {
		return err
	}
	for _, table := range []string{"concept_prerequisites", "formation_concepts", "formation_modules"} {
		if _, err := s.exec(ctx, `DELETE FROM `+table+` WHERE tenant_id=? AND formation_version_id IN (SELECT id FROM formation_versions WHERE tenant_id=? AND formation_id=?)`, tenantID, tenantID, formationID); err != nil {
			return err
		}
	}
	for _, table := range []string{"formation_versions", "formation_trainers"} {
		if _, err := s.exec(ctx, `DELETE FROM `+table+` WHERE tenant_id=? AND formation_id=?`, tenantID, formationID); err != nil {
			return err
		}
	}
	result, err := s.exec(ctx, `DELETE FROM formations WHERE tenant_id=? AND id=?`, tenantID, formationID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return storeport.WrapNotFound(sql.ErrNoRows)
	}
	return nil
}
