// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const sqliteCatalogAdminMigration = `CREATE TABLE catalog_admin_mutations (
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    idempotency_key TEXT NOT NULL,
    operation TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    response_json TEXT NOT NULL CHECK (json_valid(response_json)),
    actor_user_id TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, idempotency_key)
);`

const postgresCatalogAdminMigration = `CREATE TABLE catalog_admin_mutations (
    tenant_id TEXT NOT NULL REFERENCES tenants(id), idempotency_key TEXT NOT NULL,
    operation TEXT NOT NULL, request_hash TEXT NOT NULL, response_json JSONB NOT NULL,
    actor_user_id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, idempotency_key)
);
ALTER TABLE catalog_admin_mutations ENABLE ROW LEVEL SECURITY;
ALTER TABLE catalog_admin_mutations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON catalog_admin_mutations
USING (tenant_id = current_setting('app.current_tenant', true))
WITH CHECK (tenant_id = current_setting('app.current_tenant', true));`

func catalogRequestHash(request any) (string, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func runCatalogMutation[T any](ctx context.Context, s *Store, actor models.Principal, key, operation string, request any, mutate func(context.Context, *Store) (T, error)) (T, bool, error) {
	var zero T
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 128 || operation == "" {
		return zero, false, fmt.Errorf("catalog mutation: idempotency key is required and bounded")
	}
	requestHash, err := catalogRequestHash(request)
	if err != nil {
		return zero, false, err
	}
	load := func(txCtx context.Context, txs *Store) (T, bool, error) {
		var responseJSON, currentOperation, currentHash string
		err := txs.queryRow(txCtx, `SELECT operation, request_hash, response_json
			FROM catalog_admin_mutations WHERE tenant_id = ? AND idempotency_key = ?`,
			actor.TenantID, key).Scan(&currentOperation, &currentHash, &responseJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return zero, false, nil
		}
		if err != nil {
			return zero, false, err
		}
		if currentOperation != operation || currentHash != requestHash {
			return zero, false, fmt.Errorf("catalog mutation: idempotency key conflict")
		}
		var response T
		if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
			return zero, false, err
		}
		return response, true, nil
	}
	var response T
	replayed := false
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		current, found, err := load(txCtx, txs)
		if err != nil {
			return err
		}
		if found {
			response, replayed = current, true
			return nil
		}
		// Call through the root store with the transaction-bound context. The
		// existing catalog methods then compose into this exact transaction via
		// WithTenantTx's context path instead of trying to open a nested one.
		response, err = mutate(txCtx, s)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(response)
		if err != nil {
			return err
		}
		_, err = txs.exec(txCtx, `INSERT INTO catalog_admin_mutations
			(tenant_id, idempotency_key, operation, request_hash, response_json,
			 actor_user_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, actor.TenantID,
			key, operation, requestHash, string(raw), actor.UserID, time.Now().UTC())
		return err
	})
	if err == nil {
		return response, replayed, nil
	}
	// A concurrent winner can commit the key after this transaction loses its
	// unique insert. Re-read in a fresh tenant transaction and return only an
	// exact request replay; every mismatch remains a conflict.
	var recovered T
	var recoveredOK bool
	recoverErr := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		var inner error
		recovered, recoveredOK, inner = load(txCtx, scoped.(*Store))
		return inner
	})
	if recoverErr == nil && recoveredOK {
		return recovered, true, nil
	}
	return zero, false, err
}

type formationDraftMutationResponse struct {
	Formation models.Formation        `json:"formation"`
	Version   models.FormationVersion `json:"version"`
}

func (s *Store) CreateFormationDraftIdempotent(ctx context.Context, actor models.Principal, idempotencyKey, name, description string) (*models.Formation, *models.FormationVersion, bool, error) {
	request := struct{ Name, Description string }{name, description}
	response, replayed, err := runCatalogMutation(ctx, s, actor, idempotencyKey,
		"formation.create", request, func(txCtx context.Context, txs *Store) (formationDraftMutationResponse, error) {
			formation, version, err := txs.CreateFormationDraft(txCtx, actor, name, description)
			if err != nil {
				return formationDraftMutationResponse{}, err
			}
			return formationDraftMutationResponse{Formation: *formation, Version: *version}, nil
		})
	if err != nil {
		return nil, nil, false, err
	}
	return &response.Formation, &response.Version, replayed, nil
}

func (s *Store) AddFormationModuleIdempotent(ctx context.Context, actor models.Principal, idempotencyKey, versionID string, input models.FormationModuleInput) (string, bool, error) {
	request := struct {
		VersionID string
		Input     models.FormationModuleInput
	}{versionID, input}
	response, replayed, err := runCatalogMutation(ctx, s, actor, idempotencyKey,
		"formation.module.add", request, func(txCtx context.Context, txs *Store) (string, error) {
			return txs.AddFormationModule(txCtx, actor, versionID, input)
		})
	return response, replayed, err
}

func (s *Store) AddFormationConceptIdempotent(ctx context.Context, actor models.Principal, idempotencyKey, versionID string, input models.FormationConceptInput) (string, bool, error) {
	request := struct {
		VersionID string
		Input     models.FormationConceptInput
	}{versionID, input}
	response, replayed, err := runCatalogMutation(ctx, s, actor, idempotencyKey,
		"formation.concept.add", request, func(txCtx context.Context, txs *Store) (string, error) {
			return txs.AddFormationConcept(txCtx, actor, versionID, input)
		})
	return response, replayed, err
}

func (s *Store) PublishFormationVersionIdempotent(ctx context.Context, actor models.Principal, idempotencyKey, versionID string) (*models.FormationVersion, bool, error) {
	response, replayed, err := runCatalogMutation(ctx, s, actor, idempotencyKey,
		"formation.publish", struct{ VersionID string }{versionID},
		func(txCtx context.Context, txs *Store) (models.FormationVersion, error) {
			version, err := txs.PublishFormationVersion(txCtx, actor, versionID)
			if err != nil {
				return models.FormationVersion{}, err
			}
			return *version, nil
		})
	if err != nil {
		return nil, false, err
	}
	return &response, replayed, nil
}

func (s *Store) CreateCohortIdempotent(ctx context.Context, actor models.Principal, idempotencyKey, versionID, name string, capacity int, startsAt, endsAt *time.Time) (*models.Cohort, bool, error) {
	request := struct {
		VersionID string
		Name      string
		Capacity  int
		StartsAt  *time.Time
		EndsAt    *time.Time
	}{versionID, name, capacity, startsAt, endsAt}
	response, replayed, err := runCatalogMutation(ctx, s, actor, idempotencyKey,
		"cohort.create", request, func(txCtx context.Context, txs *Store) (models.Cohort, error) {
			cohort, err := txs.CreateCohort(txCtx, actor, versionID, name, capacity, startsAt, endsAt)
			if err != nil {
				return models.Cohort{}, err
			}
			return *cohort, nil
		})
	if err != nil {
		return nil, false, err
	}
	return &response, replayed, nil
}

func (s *Store) EnrollMembershipIdempotent(ctx context.Context, actor models.Principal, idempotencyKey, cohortID, membershipID, objectivesJSON string) (*models.Enrollment, bool, error) {
	request := struct{ CohortID, MembershipID, ObjectivesJSON string }{cohortID, membershipID, objectivesJSON}
	response, replayed, err := runCatalogMutation(ctx, s, actor, idempotencyKey,
		"enrollment.create", request, func(txCtx context.Context, txs *Store) (models.Enrollment, error) {
			enrollment, err := txs.EnrollMembership(txCtx, actor, cohortID, membershipID, objectivesJSON)
			if err != nil {
				return models.Enrollment{}, err
			}
			return *enrollment, nil
		})
	if err != nil {
		return nil, false, err
	}
	return &response, replayed, nil
}

func validatePage(limit int) error {
	if limit < 1 || limit > 100 {
		return fmt.Errorf("page limit must be between 1 and 100")
	}
	return nil
}

func (s *Store) ListFormations(ctx context.Context, actor models.Principal, after string, limit int) (models.FormationPage, error) {
	if !actor.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: actor.TenantID}) || validatePage(limit) != nil {
		return models.FormationPage{}, fmt.Errorf("list formations: invalid authorization or page")
	}
	page := models.FormationPage{}
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		rows, err := scoped.(*Store).query(txCtx, `SELECT id, name, description, status, created_by, created_at, updated_at
			FROM formations WHERE tenant_id = ? AND id > ? ORDER BY id LIMIT ?`, actor.TenantID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item models.Formation
			item.TenantID = actor.TenantID
			if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.Status,
				&item.CreatedBy, &item.CreatedAt, &item.UpdatedAt); err != nil {
				return err
			}
			page.Items = append(page.Items, item)
		}
		return rows.Err()
	})
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = page.Items[len(page.Items)-1].ID
	}
	return page, err
}

func (s *Store) ListCohorts(ctx context.Context, actor models.Principal, after string, limit int) (models.CohortPage, error) {
	if !actor.Authorize(models.PermissionCohortManage, models.AuthorizationResource{TenantID: actor.TenantID}) || validatePage(limit) != nil {
		return models.CohortPage{}, fmt.Errorf("list cohorts: invalid authorization or page")
	}
	page := models.CohortPage{}
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		rows, err := scoped.(*Store).query(txCtx, `SELECT id, formation_version_id, name, starts_at, ends_at,
			capacity, reserved_seats, status, version, created_by, created_at, updated_at
			FROM cohorts WHERE tenant_id = ? AND id > ? ORDER BY id LIMIT ?`, actor.TenantID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item models.Cohort
			var startsAt, endsAt sql.NullTime
			item.TenantID = actor.TenantID
			if err := rows.Scan(&item.ID, &item.FormationVersionID, &item.Name, &startsAt,
				&endsAt, &item.Capacity, &item.ReservedSeats, &item.Status, &item.Version,
				&item.CreatedBy, &item.CreatedAt, &item.UpdatedAt); err != nil {
				return err
			}
			if startsAt.Valid {
				item.StartsAt = &startsAt.Time
			}
			if endsAt.Valid {
				item.EndsAt = &endsAt.Time
			}
			page.Items = append(page.Items, item)
		}
		return rows.Err()
	})
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = page.Items[len(page.Items)-1].ID
	}
	return page, err
}

func (s *Store) ListCohortEnrollments(ctx context.Context, actor models.Principal, cohortID, after string, limit int) (models.EnrollmentPage, error) {
	resource := models.AuthorizationResource{TenantID: actor.TenantID, CohortID: cohortID}
	if slicesContains(actor.Roles, models.RoleTrainer) {
		resource.AssignedCohortIDs = []string{}
	}
	if validatePage(limit) != nil {
		return models.EnrollmentPage{}, fmt.Errorf("list enrollments: invalid page")
	}
	page := models.EnrollmentPage{}
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if slicesContains(actor.Roles, models.RoleTrainer) {
			var assigned int
			if err := txs.queryRow(txCtx, `SELECT COUNT(*) FROM cohort_trainers
				WHERE tenant_id = ? AND cohort_id = ? AND membership_id = ?`, actor.TenantID,
				cohortID, actor.MembershipID).Scan(&assigned); err != nil || assigned != 1 {
				return storeport.ErrInvalidPrincipal
			}
			resource.AssignedCohortIDs = []string{cohortID}
		}
		if !actor.Authorize(models.PermissionProgressRead, resource) {
			return storeport.ErrInvalidPrincipal
		}
		rows, err := txs.query(txCtx, `SELECT id, formation_version_id, user_id, membership_id,
			learner_id, status, objectives_json, seat_reserved, created_at, updated_at, completed_at
			FROM enrollments WHERE tenant_id = ? AND cohort_id = ? AND id > ? ORDER BY id LIMIT ?`,
			actor.TenantID, cohortID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item models.Enrollment
			var learnerID sql.NullString
			var completedAt sql.NullTime
			item.TenantID, item.CohortID = actor.TenantID, cohortID
			if err := rows.Scan(&item.ID, &item.FormationVersionID, &item.UserID,
				&item.MembershipID, &learnerID, &item.Status, &item.ObjectivesJSON,
				&item.SeatReserved, &item.CreatedAt, &item.UpdatedAt, &completedAt); err != nil {
				return err
			}
			item.LearnerID = learnerID.String
			if completedAt.Valid {
				item.CompletedAt = &completedAt.Time
			}
			page.Items = append(page.Items, item)
		}
		return rows.Err()
	})
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = page.Items[len(page.Items)-1].ID
	}
	return page, err
}

func slicesContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (s *Store) GetCohortReport(ctx context.Context, actor models.Principal, cohortID string) (models.CohortReport, error) {
	page, err := s.ListCohortEnrollments(ctx, actor, cohortID, "", 1)
	if err != nil {
		return models.CohortReport{}, err
	}
	_ = page
	report := models.CohortReport{TenantID: actor.TenantID, CohortID: cohortID}
	err = s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		return scoped.(*Store).queryRow(txCtx, `SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN enrollment.status = 'active' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN enrollment.status = 'completed' THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(state.p_mastery), 0)
			FROM enrollments enrollment LEFT JOIN learner_concept_states state
			 ON state.tenant_id = enrollment.tenant_id AND state.enrollment_id = enrollment.id
			WHERE enrollment.tenant_id = ? AND enrollment.cohort_id = ?`, actor.TenantID, cohortID).
			Scan(&report.EnrollmentCount, &report.ActiveCount, &report.CompletedCount, &report.AverageMastery)
	})
	return report, err
}
