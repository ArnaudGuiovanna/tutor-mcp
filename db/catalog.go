// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (s *Store) CreateFormationDraft(ctx context.Context, actor models.Principal, name, description string) (*models.Formation, *models.FormationVersion, error) {
	if !actor.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, nil, fmt.Errorf("create formation: %w", storeport.ErrInvalidPrincipal)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil, fmt.Errorf("create formation: name is required")
	}
	formationID, err := generateID()
	if err != nil {
		return nil, nil, err
	}
	versionID, err := generateID()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	formation := &models.Formation{
		ID: formationID, TenantID: actor.TenantID, Name: name, Description: description,
		Status: "draft", CreatedBy: actor.UserID, CreatedAt: now, UpdatedAt: now,
	}
	version := &models.FormationVersion{
		ID: versionID, TenantID: actor.TenantID, FormationID: formationID,
		Version: 1, Status: "draft", MetadataJSON: "{}", CreatedBy: actor.UserID, CreatedAt: now,
	}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.exec(txCtx, `INSERT INTO formations
            (id, tenant_id, name, description, status, created_by, created_at, updated_at)
            VALUES (?, ?, ?, ?, 'draft', ?, ?, ?)`, formationID, actor.TenantID,
			name, description, actor.UserID, now, now); err != nil {
			return fmt.Errorf("insert formation: %w", err)
		}
		if _, err := txs.exec(txCtx, `INSERT INTO formation_versions
            (id, tenant_id, formation_id, version, status, metadata_json, created_by, created_at)
            VALUES (?, ?, ?, 1, 'draft', '{}', ?, ?)`, versionID, actor.TenantID,
			formationID, actor.UserID, now); err != nil {
			return fmt.Errorf("insert formation version: %w", err)
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "formation.create", TargetType: "formation", TargetID: formationID,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return formation, version, nil
}

func (s *Store) AddFormationModule(ctx context.Context, actor models.Principal, versionID string, input models.FormationModuleInput) (string, error) {
	if !actor.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return "", fmt.Errorf("add module: %w", storeport.ErrInvalidPrincipal)
	}
	if strings.TrimSpace(input.StableKey) == "" || strings.TrimSpace(input.Title) == "" || input.Position < 0 {
		return "", fmt.Errorf("add module: stable key, title and non-negative position are required")
	}
	if input.MetadataJSON == "" {
		input.MetadataJSON = "{}"
	}
	if !json.Valid([]byte(input.MetadataJSON)) {
		return "", fmt.Errorf("add module: metadata must be JSON")
	}
	id, err := generateID()
	if err != nil {
		return "", err
	}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		result, err := txs.exec(txCtx, `INSERT INTO formation_modules
            (id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
            SELECT ?, ?, id, ?, ?, ?, ? FROM formation_versions
            WHERE tenant_id = ? AND id = ? AND status = 'draft'`, id, actor.TenantID,
			input.StableKey, input.Title, input.Position, input.MetadataJSON, actor.TenantID, versionID)
		if err != nil {
			return fmt.Errorf("add module: %w", err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return storeport.ErrFormationVersionImmutable
		}
		return nil
	})
	return id, err
}

func (s *Store) AddFormationConcept(ctx context.Context, actor models.Principal, versionID string, input models.FormationConceptInput) (string, error) {
	if !actor.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return "", fmt.Errorf("add concept: %w", storeport.ErrInvalidPrincipal)
	}
	if strings.TrimSpace(input.ModuleStableKey) == "" || strings.TrimSpace(input.StableKey) == "" || strings.TrimSpace(input.Label) == "" || input.Position < 0 {
		return "", fmt.Errorf("add concept: module, stable key, label and position are required")
	}
	if input.MetadataJSON == "" {
		input.MetadataJSON = "{}"
	}
	if !json.Valid([]byte(input.MetadataJSON)) {
		return "", fmt.Errorf("add concept: metadata must be JSON")
	}
	id, err := generateID()
	if err != nil {
		return "", err
	}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var moduleID string
		if err := txs.queryRow(txCtx, `SELECT m.id FROM formation_modules m
            JOIN formation_versions v ON v.tenant_id = m.tenant_id AND v.id = m.formation_version_id
            WHERE m.tenant_id = ? AND m.formation_version_id = ? AND m.stable_key = ? AND v.status = 'draft'`,
			actor.TenantID, versionID, input.ModuleStableKey).Scan(&moduleID); err != nil {
			return fmt.Errorf("add concept: draft module not found: %w", err)
		}
		if _, err := txs.exec(txCtx, `INSERT INTO formation_concepts
            (id, tenant_id, formation_version_id, module_id, stable_key, label, position, metadata_json)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, actor.TenantID, versionID,
			moduleID, input.StableKey, input.Label, input.Position, input.MetadataJSON); err != nil {
			return fmt.Errorf("add concept: %w", err)
		}
		for _, prerequisite := range input.Prerequisites {
			var prerequisiteID string
			if err := txs.queryRow(txCtx, `SELECT id FROM formation_concepts
                WHERE tenant_id = ? AND formation_version_id = ? AND stable_key = ?`,
				actor.TenantID, versionID, prerequisite).Scan(&prerequisiteID); err != nil {
				return fmt.Errorf("add concept prerequisite %q: %w", prerequisite, err)
			}
			if _, err := txs.exec(txCtx, `INSERT INTO concept_prerequisites
                (tenant_id, formation_version_id, concept_id, prerequisite_id)
                VALUES (?, ?, ?, ?)`, actor.TenantID, versionID, id, prerequisiteID); err != nil {
				return fmt.Errorf("add concept prerequisite: %w", err)
			}
		}
		return nil
	})
	return id, err
}

func (s *Store) PublishFormationVersion(ctx context.Context, actor models.Principal, versionID string) (*models.FormationVersion, error) {
	if !actor.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, fmt.Errorf("publish formation: %w", storeport.ErrInvalidPrincipal)
	}
	var published *models.FormationVersion
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var formationID, metadata, createdBy string
		var version int64
		var createdAt time.Time
		if err := txs.queryRow(txCtx, `SELECT v.formation_id, v.version, v.metadata_json, v.created_by, v.created_at
            FROM formation_versions v
            WHERE v.tenant_id = ? AND v.id = ? AND v.status = 'draft'
              AND EXISTS (SELECT 1 FROM formation_modules m WHERE m.tenant_id = v.tenant_id AND m.formation_version_id = v.id)
              AND EXISTS (SELECT 1 FROM formation_concepts c WHERE c.tenant_id = v.tenant_id AND c.formation_version_id = v.id)`,
			actor.TenantID, versionID).Scan(&formationID, &version, &metadata, &createdBy, &createdAt); err != nil {
			return fmt.Errorf("publish formation: incomplete or immutable draft: %w", err)
		}
		now := time.Now().UTC()
		result, err := txs.exec(txCtx, `UPDATE formation_versions
            SET status = 'published', published_at = ?
            WHERE tenant_id = ? AND id = ? AND status = 'draft'`, now, actor.TenantID, versionID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return storeport.ErrFormationVersionImmutable
		}
		if _, err := txs.exec(txCtx, `UPDATE formations SET status = 'active', updated_at = ?
            WHERE tenant_id = ? AND id = ?`, now, actor.TenantID, formationID); err != nil {
			return err
		}
		published = &models.FormationVersion{
			ID: versionID, TenantID: actor.TenantID, FormationID: formationID,
			Version: version, Status: "published", MetadataJSON: metadata,
			CreatedBy: createdBy, CreatedAt: createdAt, PublishedAt: &now,
		}
		if err := txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "formation.publish", TargetType: "formation_version", TargetID: versionID,
		}); err != nil {
			return err
		}
		return txs.appendOutboxEvent(txCtx, models.OutboxEvent{
			TenantID: actor.TenantID, AggregateType: "formation_version", AggregateID: versionID,
			EventType: "formation.version.published", IdempotencyKey: "formation-published:" + versionID,
			PayloadJSON: `{"formation_id":"` + formationID + `","version_id":"` + versionID + `"}`,
			CreatedAt:   now,
		})
	})
	return published, err
}

func (s *Store) CreateCohort(ctx context.Context, actor models.Principal, formationVersionID, name string, capacity int, startsAt, endsAt *time.Time) (*models.Cohort, error) {
	if !actor.Authorize(models.PermissionCohortManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, fmt.Errorf("create cohort: %w", storeport.ErrInvalidPrincipal)
	}
	if strings.TrimSpace(name) == "" || capacity < 1 || (startsAt != nil && endsAt != nil && !endsAt.After(*startsAt)) {
		return nil, fmt.Errorf("create cohort: invalid name, capacity or dates")
	}
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	cohort := &models.Cohort{
		ID: id, TenantID: actor.TenantID, FormationVersionID: formationVersionID,
		Name: name, StartsAt: startsAt, EndsAt: endsAt, Capacity: capacity,
		Status: "open", Version: 1, CreatedBy: actor.UserID, CreatedAt: now, UpdatedAt: now,
	}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		result, err := txs.exec(txCtx, `INSERT INTO cohorts
            (id, tenant_id, formation_version_id, name, starts_at, ends_at, capacity,
             reserved_seats, status, version, created_by, created_at, updated_at)
            SELECT ?, ?, id, ?, ?, ?, ?, 0, 'open', 1, ?, ?, ? FROM formation_versions
            WHERE tenant_id = ? AND id = ? AND status = 'published'`, id, actor.TenantID,
			name, startsAt, endsAt, capacity, actor.UserID, now, now, actor.TenantID, formationVersionID)
		if err != nil {
			return fmt.Errorf("create cohort: %w", err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("create cohort: published formation version not found")
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "cohort.create", TargetType: "cohort", TargetID: id,
		})
	})
	return cohort, err
}

func (s *Store) AssignCohortTrainer(ctx context.Context, actor models.Principal, cohortID, trainerMembershipID string) error {
	if !actor.Authorize(models.PermissionCohortManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return fmt.Errorf("assign trainer: %w", storeport.ErrInvalidPrincipal)
	}
	return s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		rolePredicate := `EXISTS (SELECT 1 FROM json_each(tm.roles_json) WHERE value = 'trainer')`
		if txs.dialect == DialectPostgres {
			rolePredicate = `jsonb_exists(tm.roles_json, 'trainer')`
		}
		result, err := txs.exec(txCtx, `INSERT INTO cohort_trainers
            (tenant_id, cohort_id, membership_id, assigned_by, assigned_at)
            SELECT ?, c.id, tm.id, ?, ? FROM cohorts c JOIN tenant_memberships tm
              ON tm.tenant_id = c.tenant_id
            WHERE c.tenant_id = ? AND c.id = ? AND tm.id = ? AND tm.status = 'active'
              AND `+rolePredicate+`
            ON CONFLICT DO NOTHING`, actor.TenantID, actor.UserID, time.Now().UTC(),
			actor.TenantID, cohortID, trainerMembershipID)
		if err != nil {
			return fmt.Errorf("assign trainer: %w", err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("assign trainer: cohort or active trainer membership not found")
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "cohort.trainer.assign", TargetType: "cohort", TargetID: cohortID,
			DetailsJSON: `{"membership_id":` + strconvQuote(trainerMembershipID) + `}`,
		})
	})
}

func (s *Store) EnrollMembership(ctx context.Context, actor models.Principal, cohortID, membershipID, objectivesJSON string) (*models.Enrollment, error) {
	if !actor.Authorize(models.PermissionCohortManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, fmt.Errorf("enroll membership: %w", storeport.ErrInvalidPrincipal)
	}
	if objectivesJSON == "" {
		objectivesJSON = "{}"
	}
	if !json.Valid([]byte(objectivesJSON)) {
		return nil, fmt.Errorf("enroll membership: objectives must be JSON")
	}
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	var enrollment *models.Enrollment
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		var formationVersionID, userID string
		var learnerID sql.NullString
		if err := txs.queryRow(txCtx, `SELECT c.formation_version_id, tm.user_id, tm.learner_id
            FROM cohorts c JOIN tenant_memberships tm ON tm.tenant_id = c.tenant_id
            WHERE c.tenant_id = ? AND c.id = ? AND tm.id = ? AND tm.status = 'active'`,
			actor.TenantID, cohortID, membershipID).Scan(&formationVersionID, &userID, &learnerID); err != nil {
			return fmt.Errorf("enroll membership: cohort or membership not found: %w", err)
		}
		result, err := txs.exec(txCtx, `UPDATE cohorts
            SET reserved_seats = reserved_seats + 1, version = version + 1, updated_at = ?
            WHERE tenant_id = ? AND id = ? AND status = 'open' AND reserved_seats < capacity`,
			time.Now().UTC(), actor.TenantID, cohortID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return storeport.ErrCohortCapacityReached
		}
		now := time.Now().UTC()
		if _, err := txs.exec(txCtx, `INSERT INTO enrollments
            (id, tenant_id, cohort_id, formation_version_id, user_id, membership_id,
             learner_id, status, objectives_json, seat_reserved, created_at, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, 1, ?, ?)`, id, actor.TenantID,
			cohortID, formationVersionID, userID, membershipID, nullString(learnerID.String),
			objectivesJSON, now, now); err != nil {
			return fmt.Errorf("enroll membership: %w", err)
		}
		enrollment = &models.Enrollment{
			ID: id, TenantID: actor.TenantID, CohortID: cohortID,
			FormationVersionID: formationVersionID, UserID: userID,
			MembershipID: membershipID, LearnerID: learnerID.String, Status: "active",
			ObjectivesJSON: objectivesJSON, SeatReserved: true, CreatedAt: now, UpdatedAt: now,
		}
		return txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{
			Action: "enrollment.create", TargetType: "enrollment", TargetID: id,
		})
	})
	return enrollment, err
}

func strconvQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
