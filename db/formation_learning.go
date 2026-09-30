// Copyright (c) 2026 Arnaud Guiovanna
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
	storeport "tutor-mcp/store"
)

func formationCurriculum(detail *models.FormationVersionDetail, domainID, author string, now time.Time) (*models.CurriculumSnapshot, error) {
	snapshot := &models.CurriculumSnapshot{
		DomainID: domainID, Version: 1,
		Graph:      models.KnowledgeSpace{Concepts: []string{}, Prerequisites: map[string][]string{}},
		Concepts:   []models.CurriculumConcept{},
		Operation:  models.CurriculumOperation{Type: models.CurriculumOperationCreate, Rationale: "enroll in the published formation"},
		Provenance: models.CurriculumProvenance{SourceType: "formation_version", SourceRef: detail.Version.ID, Author: author, Rationale: "formation is the authoritative curriculum"},
		Review:     models.CurriculumReview{Status: models.CurriculumReviewUnreviewed}, CreatedBy: author, CreatedAt: now,
	}
	for _, c := range detail.Concepts {
		concept := models.CurriculumConcept{
			ID: stableLearningID("fcurr_", domainID, c.StableKey), Key: c.StableKey, Label: c.Label,
			FormationConceptID: c.ID, Description: c.Description, Level: c.Level, Status: models.CurriculumConceptActive,
			Outcomes: append([]models.CurriculumOutcome{}, c.Outcomes...), Criteria: append([]models.CurriculumCriterion{}, c.Criteria...),
		}
		if concept.Level == "" {
			concept.Level = models.CurriculumLevelUnspecified
		}
		// Curriculum metadata identities are domain-local. Published concept IDs
		// are a separate shared dimension, carried unchanged on each observation.
		for i := range concept.Outcomes {
			concept.Outcomes[i].ID = stableLearningID("fout_", domainID, c.StableKey, concept.Outcomes[i].ID)
		}
		for i := range concept.Criteria {
			concept.Criteria[i].ID = stableLearningID("fcrit_", domainID, c.StableKey, concept.Criteria[i].ID)
		}
		snapshot.Concepts = append(snapshot.Concepts, concept)
		snapshot.Graph.Concepts = append(snapshot.Graph.Concepts, c.StableKey)
		snapshot.Graph.Prerequisites[c.StableKey] = append([]string{}, c.Prerequisites...)
	}
	if len(snapshot.Concepts) == 0 {
		return nil, fmt.Errorf("formation requires at least one concept")
	}
	if err := validateCurriculumSnapshot(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Store) mapFormationDomain(ctx context.Context, enrollment *models.Enrollment, domainID string, detail *models.FormationVersionDetail) error {
	if _, err := s.exec(ctx, `INSERT INTO legacy_domain_enrollments (tenant_id, learner_id, domain_id, enrollment_id)
	 VALUES (?, ?, ?, ?) ON CONFLICT (tenant_id, learner_id, domain_id) DO UPDATE SET enrollment_id = excluded.enrollment_id`, enrollment.TenantID, enrollment.LearnerID, domainID, enrollment.ID); err != nil {
		return err
	}
	for _, c := range detail.Concepts {
		if _, err := s.exec(ctx, `INSERT INTO legacy_concept_sources (tenant_id, learner_id, enrollment_id, domain_id, concept_label, concept_id)
		 VALUES (?, ?, ?, ?, ?, ?)`, enrollment.TenantID, enrollment.LearnerID, enrollment.ID, domainID, c.StableKey, c.ID); err != nil {
			return err
		}
		if _, err := s.exec(ctx, `INSERT INTO legacy_concept_mappings (tenant_id, enrollment_id, domain_id, concept_label, concept_id)
		 VALUES (?, ?, ?, ?, ?)`, enrollment.TenantID, enrollment.ID, domainID, c.StableKey, c.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) createFormationDomain(ctx context.Context, enrollment *models.Enrollment, actor models.Principal) error {
	if enrollment.LearnerID == "" {
		return fmt.Errorf("enrollment requires a learner membership")
	}
	detail, err := s.loadFormationVersion(ctx, enrollment.TenantID, enrollment.FormationVersionID)
	if err != nil {
		return err
	}
	if detail.Version.Status != "published" || detail.Formation.Status != "active" {
		return storeport.ErrFormationVersionImmutable
	}
	id, err := generateID()
	if err != nil {
		return err
	}
	snapshot, err := formationCurriculum(detail, id, actor.UserID, enrollment.CreatedAt)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot.Graph)
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, `INSERT INTO domains (id, learner_id, tenant_id, name, personal_goal, graph_json, graph_version, created_at, formation_enrollment_id, formation_version_id)
	 VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`, id, enrollment.LearnerID, enrollment.TenantID, detail.Formation.Name, detail.Formation.Description, string(raw), enrollment.CreatedAt, enrollment.ID, enrollment.FormationVersionID); err != nil {
		return err
	}
	if err := s.mapFormationDomain(ctx, enrollment, id, detail); err != nil {
		return err
	}
	if _, err := s.insertCurriculumSnapshot(ctx, enrollment.LearnerID, snapshot, false); err != nil {
		return err
	}
	for _, c := range detail.Concepts {
		if err := s.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(enrollment.LearnerID, id, c.StableKey)); err != nil {
			return err
		}
	}
	enrollment.DomainID = id
	return nil
}

func (s *Store) formationLearningScope(ctx context.Context, learnerID, domainID, key string) (learningScopeIDs, bool, error) {
	var out learningScopeIDs
	var status string
	if s.dialect == DialectPostgres {
		// Learning events and version migration already lock the domain first.
		// Serialize formation writes and leave on that same row, then read
		// enrollment status in a fresh statement after any lock wait.
		var id string
		err := s.queryRow(ctx, `SELECT id FROM domains WHERE id = ? AND learner_id = ? AND formation_enrollment_id <> '' AND deleted_at IS NULL FOR UPDATE`, domainID, learnerID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return out, false, nil
		}
		if err != nil {
			return out, true, err
		}
	}
	query := `SELECT d.tenant_id, d.formation_enrollment_id, e.status FROM domains d
	 JOIN enrollments e ON e.tenant_id = d.tenant_id AND e.id = d.formation_enrollment_id
	 WHERE d.id = ? AND d.learner_id = ? AND d.formation_enrollment_id <> '' AND d.deleted_at IS NULL`
	err := s.queryRow(ctx, query, domainID, learnerID).Scan(&out.TenantID, &out.EnrollmentID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, true, err
	}
	if status != "active" {
		return out, true, fmt.Errorf("formation enrollment is not active")
	}
	if key == "" {
		return out, true, nil
	} // session and narrative scope, not a concept
	err = s.queryRow(ctx, `SELECT concept_id FROM legacy_concept_mappings WHERE tenant_id = ? AND enrollment_id = ? AND domain_id = ? AND concept_label = ?`, out.TenantID, out.EnrollmentID, domainID, key).Scan(&out.FormationConceptID)
	if err != nil {
		return out, true, fmt.Errorf("unknown formation concept: %w", err)
	}
	return out, true, nil
}

func (s *Store) ensureFreeDomainAllowed(ctx context.Context, learnerID string) error {
	var tenantID, raw string
	if err := s.queryRow(ctx, `SELECT t.id, t.policy_json FROM learners l JOIN tenants t ON t.id = l.tenant_id WHERE l.id = ?`, learnerID).Scan(&tenantID, &raw); err != nil {
		return err
	}
	var policy struct {
		AllowFreeDomains *bool `json:"allow_free_domains"`
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return err
	}
	allowed := tenantID == models.LegacyTenantID
	if policy.AllowFreeDomains != nil {
		allowed = *policy.AllowFreeDomains
	}
	if !allowed {
		return fmt.Errorf("free domains are disabled by the institution; enroll in a formation")
	}
	return nil
}

func (s *Store) SetInstitutionLearningPolicy(ctx context.Context, actor models.Principal, allowFreeDomains bool) error {
	if !actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return storeport.ErrInvalidPrincipal
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if err := txs.ValidatePrincipal(txCtx, actor); err != nil {
			return err
		}
		query := `SELECT policy_json FROM tenants WHERE id = ?`
		if txs.dialect == DialectPostgres {
			query += " FOR UPDATE"
		}
		var raw string
		if err := txs.queryRow(txCtx, query, actor.TenantID).Scan(&raw); err != nil {
			return err
		}
		var policy map[string]any
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return err
		}
		if policy == nil {
			policy = map[string]any{}
		}
		policy["allow_free_domains"] = allowFreeDomains
		encoded, err := json.Marshal(policy)
		if err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE tenants SET policy_json = ?, updated_at = ? WHERE id = ?`, string(encoded), time.Now().UTC(), actor.TenantID); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "tenant.learning_policy", TargetType: "tenant", TargetID: actor.TenantID, DetailsJSON: fmt.Sprintf(`{"allow_free_domains":%t}`, allowFreeDomains)})
	})
}
