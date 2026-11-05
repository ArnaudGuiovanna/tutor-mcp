// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const resumeTrainingChoiceLimit = 20

func trainingContext(t models.Training, domain *models.Domain) *models.TrainingContext {
	return &models.TrainingContext{
		ID: t.ID, Kind: t.Kind, Name: t.Name, Description: t.Description,
		DomainID: domain.ID, FormationID: t.FormationID,
		LearningVersionID: domain.FormationVersionID, CurriculumRevision: strconv.Itoa(domain.GraphVersion),
	}
}

// ResolveResumeTrainings searches the entire authorized set of existing
// learning domains. Its output bound is applied AFTER exact matching, so an
// unseen second page cannot turn an ambiguous name into an automatic choice.
// Resuming never enrolls a learner into a newly discovered catalogue entry.
func (s *Store) ResolveResumeTrainings(ctx context.Context, p models.Principal, id, name string) (models.ResumeTrainingChoices, error) {
	out := models.ResumeTrainingChoices{Items: []models.TrainingContext{}}
	if len(id) > 255 || len(name) > 200 || (id != "" && name != "") {
		return out, fmt.Errorf("invalid resume training target")
	}
	if p.LearnerID == "" || !models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeLearnerRead) {
		return out, storeport.ErrInvalidPrincipal
	}
	err := s.WithTenantReadTx(ctx, p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		tx := scoped.(*Store)
		if err := tx.ValidatePrincipal(ctx, p); err != nil {
			return err
		}
		rows, err := tx.query(ctx, `SELECT id,name FROM (
 SELECT 'personal:'||d.id AS id,d.name AS name FROM domains d
 WHERE d.tenant_id=? AND d.learner_id=? AND d.deleted_at IS NULL AND d.archived=0 AND COALESCE(d.formation_enrollment_id,'')=''
 UNION
 SELECT 'formation:'||f.id AS id,f.name AS name FROM domains d
 JOIN enrollments e ON e.tenant_id=d.tenant_id AND e.id=d.formation_enrollment_id
 JOIN formation_versions v ON v.tenant_id=e.tenant_id AND v.id=e.formation_version_id
 JOIN formations f ON f.tenant_id=v.tenant_id AND f.id=v.formation_id
 WHERE d.tenant_id=? AND d.learner_id=? AND d.deleted_at IS NULL AND d.archived=0
 AND e.membership_id=? AND e.status IN ('active','completed') AND v.status='published' AND `+institutionalFormationSQL+`
 ) resumable ORDER BY id`, p.TenantID, p.LearnerID, p.TenantID, p.LearnerID, p.MembershipID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var candidate, candidateName string
			if err = rows.Scan(&candidate, &candidateName); err != nil {
				break
			}
			if id != "" && candidate != id {
				continue
			}
			if name != "" && !strings.EqualFold(strings.TrimSpace(candidateName), strings.TrimSpace(name)) {
				continue
			}
			ids = append(ids, candidate)
			if len(ids) > resumeTrainingChoiceLimit {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, candidate := range ids {
			// Reuse the normal live authorization and enrolled-version checks.
			// The projection cannot establish legacy baselines or expand drafts.
			doc, err := tx.trainingDocumentView(ctx, p, candidate, true)
			if err != nil {
				return err
			}
			if doc.Training.DomainID == "" {
				return storeport.ErrFormationUnavailable
			}
			domain, err := tx.GetLearnerDomainByID(ctx, p.LearnerID, doc.Training.DomainID)
			if err != nil {
				return err
			}
			if domain.Archived || domain.DeletedAt != nil {
				return storeport.ErrFormationUnavailable
			}
			if len(out.Items) == resumeTrainingChoiceLimit {
				out.HasMore = true
				break
			}
			out.Items = append(out.Items, *trainingContext(doc.Training, domain))
		}
		return nil
	})
	return out, err
}
