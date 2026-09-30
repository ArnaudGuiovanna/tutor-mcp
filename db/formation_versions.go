// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// formationAccess loads the resource under the current tenant transaction.
// Mutations serialize on the formation before locking versions, cohorts or domains.
func (s *Store) formationAccess(ctx context.Context, actor models.Principal, kind, id string, permission models.Permission, lock bool) (*models.Formation, error) {
	if err := s.ValidatePrincipal(ctx, actor); err != nil {
		return nil, err
	}
	join, predicate := "", "f.id = ?"
	switch kind {
	case "formation":
	case "version":
		join = " JOIN formation_versions v ON v.tenant_id = f.tenant_id AND v.formation_id = f.id"
		predicate = "v.id = ?"
	case "cohort":
		join = " JOIN formation_versions v ON v.tenant_id = f.tenant_id AND v.formation_id = f.id JOIN cohorts c ON c.tenant_id = v.tenant_id AND c.formation_version_id = v.id"
		predicate = "c.id = ?"
	default:
		return nil, fmt.Errorf("invalid catalogue resource")
	}
	q := `SELECT f.id, f.name, f.description, f.status, f.created_by, f.created_at, f.updated_at,
	 f.owner_membership_id, f.enrollment_policy FROM formations f` + join + ` WHERE f.tenant_id = ? AND ` + predicate
	if lock && s.dialect == DialectPostgres {
		q += " FOR UPDATE OF f"
	}
	f := &models.Formation{TenantID: actor.TenantID}
	if err := s.queryRow(ctx, q, actor.TenantID, id).Scan(&f.ID, &f.Name, &f.Description, &f.Status,
		&f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.OwnerMembershipID, &f.EnrollmentPolicy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, storeport.ErrInvalidPrincipal
		}
		return nil, err
	}
	resource := models.AuthorizationResource{TenantID: actor.TenantID, FormationID: f.ID, OwnerMembershipID: f.OwnerMembershipID, OwnerUserID: actor.UserID}
	var assigned int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM formation_trainers WHERE tenant_id = ? AND formation_id = ? AND membership_id = ?`, actor.TenantID, f.ID, actor.MembershipID).Scan(&assigned); err != nil {
		return nil, err
	}
	if assigned > 0 {
		resource.AssignedFormationIDs = []string{f.ID}
	}
	if kind == "cohort" {
		resource.CohortID = id
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM cohort_trainers WHERE tenant_id = ? AND cohort_id = ? AND membership_id = ?`, actor.TenantID, id, actor.MembershipID).Scan(&assigned); err != nil {
			return nil, err
		}
		if assigned > 0 {
			resource.AssignedCohortIDs = []string{id}
		}
	}
	if !actor.Authorize(permission, resource) {
		return nil, storeport.ErrInvalidPrincipal
	}
	return f, nil
}

func (s *Store) loadFormationVersion(ctx context.Context, tenantID, versionID string) (*models.FormationVersionDetail, error) {
	d := &models.FormationVersionDetail{Modules: []models.FormationModule{}, Concepts: []models.FormationConcept{}}
	v := &d.Version
	v.TenantID = tenantID
	var published sql.NullTime
	if err := s.queryRow(ctx, `SELECT id, formation_id, version, status, metadata_json, created_by, created_at, published_at
	 FROM formation_versions WHERE tenant_id = ? AND id = ?`, tenantID, versionID).Scan(&v.ID, &v.FormationID, &v.Version, &v.Status, &v.MetadataJSON, &v.CreatedBy, &v.CreatedAt, &published); err != nil {
		return nil, err
	}
	if published.Valid {
		v.PublishedAt = &published.Time
	}
	f := &d.Formation
	f.TenantID = tenantID
	if err := s.queryRow(ctx, `SELECT id, name, description, status, created_by, created_at, updated_at, owner_membership_id, enrollment_policy
	 FROM formations WHERE tenant_id = ? AND id = ?`, tenantID, v.FormationID).Scan(&f.ID, &f.Name, &f.Description, &f.Status, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.OwnerMembershipID, &f.EnrollmentPolicy); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT id, stable_key, title, position, metadata_json FROM formation_modules WHERE tenant_id = ? AND formation_version_id = ? ORDER BY position, stable_key`, tenantID, versionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m models.FormationModule
		if err := rows.Scan(&m.ID, &m.StableKey, &m.Title, &m.Position, &m.MetadataJSON); err != nil {
			rows.Close()
			return nil, err
		}
		d.Modules = append(d.Modules, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.query(ctx, `SELECT c.id, m.stable_key, c.stable_key, c.label, c.position, c.metadata_json
	 FROM formation_concepts c JOIN formation_modules m ON m.tenant_id = c.tenant_id AND m.id = c.module_id
	 WHERE c.tenant_id = ? AND c.formation_version_id = ? ORDER BY m.position, c.position, c.stable_key`, tenantID, versionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c models.FormationConcept
		if err := rows.Scan(&c.ID, &c.ModuleStableKey, &c.StableKey, &c.Label, &c.Position, &c.MetadataJSON); err != nil {
			rows.Close()
			return nil, err
		}
		if err := decodeFormationPedagogy(&c.FormationConceptInput); err != nil {
			rows.Close()
			return nil, err
		}
		c.Prerequisites = []string{}
		d.Concepts = append(d.Concepts, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.query(ctx, `SELECT c.stable_key, p.stable_key FROM concept_prerequisites e
	 JOIN formation_concepts c ON c.tenant_id = e.tenant_id AND c.id = e.concept_id
	 JOIN formation_concepts p ON p.tenant_id = e.tenant_id AND p.id = e.prerequisite_id
	 WHERE e.tenant_id = ? AND e.formation_version_id = ? ORDER BY c.stable_key, p.stable_key`, tenantID, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	indices := map[string]int{}
	for i, c := range d.Concepts {
		indices[c.StableKey] = i
	}
	for rows.Next() {
		var key, prerequisite string
		if err := rows.Scan(&key, &prerequisite); err != nil {
			return nil, err
		}
		i := indices[key]
		d.Concepts[i].Prerequisites = append(d.Concepts[i].Prerequisites, prerequisite)
	}
	return d, rows.Err()
}

func (s *Store) GetFormationVersion(ctx context.Context, actor models.Principal, versionID string) (*models.FormationVersionDetail, error) {
	var detail *models.FormationVersionDetail
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.formationAccess(txCtx, actor, "version", versionID, models.PermissionFormationWrite, false); err != nil {
			return err
		}
		var err error
		detail, err = txs.loadFormationVersion(txCtx, actor.TenantID, versionID)
		return err
	})
	return detail, err
}

type formationPedagogy struct {
	Description string                       `json:"description,omitempty"`
	Level       models.CurriculumLevel       `json:"level,omitempty"`
	Outcomes    []models.CurriculumOutcome   `json:"outcomes,omitempty"`
	Criteria    []models.CurriculumCriterion `json:"criteria,omitempty"`
}

func decodeFormationPedagogy(c *models.FormationConceptInput) error {
	var p formationPedagogy
	if err := json.Unmarshal([]byte(c.MetadataJSON), &p); err != nil {
		return fmt.Errorf("invalid concept metadata: %w", err)
	}
	c.Description, c.Level, c.Outcomes, c.Criteria = p.Description, p.Level, p.Outcomes, p.Criteria
	if c.Level == "" {
		c.Level = models.CurriculumLevelUnspecified
	}
	return nil
}

func normalizeFormationConcept(input *models.FormationConceptInput) error {
	if len(input.StableKey) > 200 || strings.TrimSpace(input.StableKey) != input.StableKey || input.StableKey == "" || strings.ContainsAny(input.StableKey, "\x00\r\n") || len(input.Label) > 500 || len(input.MetadataJSON) > 65536 || len(input.Prerequisites) > 500 {
		return fmt.Errorf("concept fields exceed their limits or contain an invalid key")
	}
	if input.MetadataJSON == "" {
		input.MetadataJSON = "{}"
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input.MetadataJSON), &metadata); err != nil || metadata == nil {
		return fmt.Errorf("metadata must be a JSON object")
	}
	if input.Description == "" && input.Level == "" && input.Outcomes == nil && input.Criteria == nil {
		if err := decodeFormationPedagogy(input); err != nil {
			return err
		}
	}
	if input.Level == "" {
		input.Level = models.CurriculumLevelUnspecified
	}
	if len(input.Description) > 16000 || len(input.Outcomes) > 100 || len(input.Criteria) > 100 {
		return fmt.Errorf("concept pedagogy exceeds its limits")
	}
	concept := models.CurriculumConcept{ID: "validation", Key: input.StableKey, Label: input.Label, Description: input.Description, Level: input.Level, Outcomes: input.Outcomes, Criteria: input.Criteria, Status: models.CurriculumConceptActive}
	if !validCurriculumLevel(input.Level) {
		return fmt.Errorf("invalid concept level")
	}
	if err := validateCurriculumMetadataIDs(concept); err != nil {
		return err
	}
	encoded, _ := json.Marshal(formationPedagogy{input.Description, input.Level, input.Outcomes, input.Criteria})
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	for _, key := range []string{"description", "level", "outcomes", "criteria"} {
		delete(metadata, key)
	}
	for key, value := range fields {
		metadata[key] = value
	}
	raw, err := json.Marshal(metadata)
	input.MetadataJSON = string(raw)
	return err
}

// ReplaceFormationContent replaces a draft atomically, permitting arbitrary
// input order and forward prerequisite references. Published row IDs never change.
func (s *Store) ReplaceFormationContent(ctx context.Context, actor models.Principal, versionID string, modules []models.FormationModuleInput, concepts []models.FormationConceptInput) error {
	if len(modules) == 0 || len(modules) > 100 || len(concepts) == 0 || len(concepts) > 500 {
		return fmt.Errorf("a draft requires 1..100 modules and 1..500 concepts")
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		f, err := txs.formationAccess(txCtx, actor, "version", versionID, models.PermissionFormationWrite, true)
		if err != nil {
			return err
		}
		if f.Status == "archived" {
			return storeport.ErrFormationVersionImmutable
		}
		var status string
		if err := txs.queryRow(txCtx, `SELECT status FROM formation_versions WHERE tenant_id = ? AND id = ?`, actor.TenantID, versionID).Scan(&status); err != nil {
			return err
		}
		if status != "draft" {
			return storeport.ErrFormationVersionImmutable
		}
		for _, table := range []string{"concept_prerequisites", "formation_concepts", "formation_modules"} {
			if _, err := txs.exec(txCtx, `DELETE FROM `+table+` WHERE tenant_id = ? AND formation_version_id = ?`, actor.TenantID, versionID); err != nil {
				return err
			}
		}
		for _, m := range modules {
			if _, err := s.AddFormationModule(txCtx, actor, versionID, m); err != nil {
				return err
			}
		}
		for _, c := range concepts {
			c.Prerequisites = nil
			if _, err := s.AddFormationConcept(txCtx, actor, versionID, c); err != nil {
				return err
			}
		}
		for _, c := range concepts {
			for _, prerequisite := range c.Prerequisites {
				res, err := txs.exec(txCtx, `INSERT INTO concept_prerequisites (tenant_id, formation_version_id, concept_id, prerequisite_id)
			 SELECT ?, ?, c.id, p.id FROM formation_concepts c JOIN formation_concepts p ON p.tenant_id = c.tenant_id AND p.formation_version_id = c.formation_version_id
			 WHERE c.tenant_id = ? AND c.formation_version_id = ? AND c.stable_key = ? AND p.stable_key = ?`, actor.TenantID, versionID, actor.TenantID, versionID, c.StableKey, prerequisite)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n != 1 {
					return fmt.Errorf("unknown prerequisite %q", prerequisite)
				}
			}
		}
		detail, err := txs.loadFormationVersion(txCtx, actor.TenantID, versionID)
		if err != nil {
			return err
		}
		if _, err := formationCurriculum(detail, "validation", actor.UserID, time.Now().UTC()); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "formation.content.replace", TargetType: "formation_version", TargetID: versionID})
	})
}

func (s *Store) CloneFormationVersion(ctx context.Context, actor models.Principal, versionID string) (*models.FormationVersionDetail, error) {
	var result *models.FormationVersionDetail
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		f, err := txs.formationAccess(txCtx, actor, "version", versionID, models.PermissionFormationWrite, true)
		if err != nil {
			return err
		}
		if f.Status == "archived" {
			return storeport.ErrFormationVersionImmutable
		}
		source, err := txs.loadFormationVersion(txCtx, actor.TenantID, versionID)
		if err != nil {
			return err
		}
		id, err := generateID()
		if err != nil {
			return err
		}
		_, err = txs.exec(txCtx, `INSERT INTO formation_versions (id, tenant_id, formation_id, version, status, metadata_json, created_by, created_at)
		 SELECT ?, ?, ?, MAX(version)+1, 'draft', ?, ?, ? FROM formation_versions WHERE tenant_id = ? AND formation_id = ?`, id, actor.TenantID, f.ID, source.Version.MetadataJSON, actor.UserID, time.Now().UTC(), actor.TenantID, f.ID)
		if err != nil {
			return err
		}
		var modules []models.FormationModuleInput
		for _, m := range source.Modules {
			modules = append(modules, m.FormationModuleInput)
		}
		var concepts []models.FormationConceptInput
		for _, c := range source.Concepts {
			concepts = append(concepts, c.FormationConceptInput)
		}
		if err := s.ReplaceFormationContent(txCtx, actor, id, modules, concepts); err != nil {
			return err
		}
		result, err = txs.loadFormationVersion(txCtx, actor.TenantID, id)
		return err
	})
	return result, err
}

func (s *Store) SetFormationEnrollmentPolicy(ctx context.Context, actor models.Principal, formationID, policy string) error {
	if !slices.Contains([]string{"open", "invitation", "approval"}, policy) {
		return fmt.Errorf("invalid enrollment policy")
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.formationAccess(txCtx, actor, "formation", formationID, models.PermissionFormationWrite, true); err != nil {
			return err
		}
		if _, err := txs.exec(txCtx, `UPDATE formations SET enrollment_policy = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`, policy, time.Now().UTC(), actor.TenantID, formationID); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "formation.enrollment_policy", TargetType: "formation", TargetID: formationID, DetailsJSON: `{"policy":` + strconvQuote(policy) + `}`})
	})
}

// AssignFormationTrainer is administration, not delegated authoring authority.
func (s *Store) AssignFormationTrainer(ctx context.Context, actor models.Principal, formationID, membershipID string, assigned bool) error {
	if !actor.Authorize(models.PermissionMembershipManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return storeport.ErrInvalidPrincipal
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.formationAccess(txCtx, actor, "formation", formationID, models.PermissionFormationWrite, true); err != nil {
			return err
		}
		if assigned {
			var raw string
			if err := txs.queryRow(txCtx, `SELECT roles_json FROM tenant_memberships WHERE tenant_id = ? AND id = ? AND status = 'active'`, actor.TenantID, membershipID).Scan(&raw); err != nil {
				return err
			}
			var roles []string
			if err := json.Unmarshal([]byte(raw), &roles); err != nil {
				return err
			}
			if !slices.Contains(roles, models.RolePedagogyManager) {
				return fmt.Errorf("assigned membership must be a pedagogy manager")
			}
			if _, err := txs.exec(txCtx, `INSERT INTO formation_trainers (tenant_id, formation_id, membership_id, assigned_by, assigned_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, actor.TenantID, formationID, membershipID, actor.UserID, time.Now().UTC()); err != nil {
				return err
			}
		} else if _, err := txs.exec(txCtx, `DELETE FROM formation_trainers WHERE tenant_id = ? AND formation_id = ? AND membership_id = ?`, actor.TenantID, formationID, membershipID); err != nil {
			return err
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "formation.assignment", TargetType: "formation", TargetID: formationID, DetailsJSON: fmt.Sprintf(`{"membership_id":%s,"assigned":%t}`, strconvQuote(membershipID), assigned)})
	})
}

// RemoveFormation archives anything ever published. Only wholly unpublished
// drafts can be deleted; evidence and published definitions are never deleted.
func (s *Store) RemoveFormation(ctx context.Context, actor models.Principal, formationID string) (string, error) {
	status := "archived"
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.formationAccess(txCtx, actor, "formation", formationID, models.PermissionFormationDelete, true); err != nil {
			return err
		}
		var published int
		if err := txs.queryRow(txCtx, `SELECT COUNT(*) FROM formation_versions WHERE tenant_id = ? AND formation_id = ? AND status <> 'draft'`, actor.TenantID, formationID).Scan(&published); err != nil {
			return err
		}
		if published > 0 {
			if _, err := txs.exec(txCtx, `UPDATE formations SET status = 'archived', updated_at = ? WHERE tenant_id = ? AND id = ?`, time.Now().UTC(), actor.TenantID, formationID); err != nil {
				return err
			}
		} else {
			for _, table := range []string{"concept_prerequisites", "formation_concepts", "formation_modules"} {
				if _, err := txs.exec(txCtx, `DELETE FROM `+table+` WHERE tenant_id = ? AND formation_version_id IN (SELECT id FROM formation_versions WHERE tenant_id = ? AND formation_id = ?)`, actor.TenantID, actor.TenantID, formationID); err != nil {
					return err
				}
			}
			for _, table := range []string{"formation_versions", "formation_trainers"} {
				if _, err := txs.exec(txCtx, `DELETE FROM `+table+` WHERE tenant_id = ? AND formation_id = ?`, actor.TenantID, formationID); err != nil {
					return err
				}
			}
			if _, err := txs.exec(txCtx, `DELETE FROM formations WHERE tenant_id = ? AND id = ?`, actor.TenantID, formationID); err != nil {
				return err
			}
			status = "deleted"
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "formation." + status, TargetType: "formation", TargetID: formationID})
	})
	return status, err
}
