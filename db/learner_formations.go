// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Compatibility formations represent private free domains or quarantined
// history. Their reserved, server-generated IDs must never enter the shared
// institutional catalog (including program reads by a guessed cohort ID).
const institutionalFormationSQL = `substr(f.id, 1, 7) <> 'legacy_' AND substr(f.id, 1, 17) <> 'domain_formation_'`

func (s *Store) learnerFormationActor(ctx context.Context, actor models.Principal, scope string) error {
	if actor.LearnerID == "" || !actor.Authorize(models.PermissionLearningSelf, models.AuthorizationResource{TenantID: actor.TenantID, OwnerUserID: actor.UserID}) || !models.OAuthScopeAllows(strings.Join(actor.Scopes, " "), scope) {
		return storeport.ErrInvalidPrincipal
	}
	return s.ValidatePrincipal(ctx, actor)
}

func (s *Store) learnerEnrollment(ctx context.Context, actor models.Principal, cohortID string) (*models.Enrollment, error) {
	e := &models.Enrollment{TenantID: actor.TenantID, CohortID: cohortID}
	var completed sql.NullTime
	err := s.queryRow(ctx, `SELECT e.id, e.formation_version_id, e.user_id, e.membership_id, e.learner_id,
 e.status, e.objectives_json, e.seat_reserved, e.created_at, e.updated_at, e.completed_at,
 COALESCE(d.id, '') FROM enrollments e LEFT JOIN domains d ON d.tenant_id = e.tenant_id
 AND d.formation_enrollment_id = e.id AND d.learner_id = e.learner_id AND d.deleted_at IS NULL
 WHERE e.tenant_id = ? AND e.cohort_id = ? AND e.membership_id = ? AND e.user_id = ? AND e.learner_id = ?`,
		actor.TenantID, cohortID, actor.MembershipID, actor.UserID, actor.LearnerID).Scan(
		&e.ID, &e.FormationVersionID, &e.UserID, &e.MembershipID, &e.LearnerID, &e.Status, &e.ObjectivesJSON,
		&e.SeatReserved, &e.CreatedAt, &e.UpdatedAt, &completed, &e.DomainID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if completed.Valid {
		e.CompletedAt = &completed.Time
	}
	return e, nil
}

func (s *Store) learnerOffering(ctx context.Context, actor models.Principal, cohortID string) (*models.LearnerFormation, error) {
	f := &models.LearnerFormation{CohortID: cohortID}
	var starts, ends sql.NullTime
	err := s.queryRow(ctx, `SELECT f.id, f.name, f.description, f.status, f.enrollment_policy,
 c.name, c.status, v.id, v.version, c.capacity - c.reserved_seats, c.starts_at, c.ends_at,
 COALESCE(a.status, '') FROM cohorts c
 JOIN formation_versions v ON v.tenant_id = c.tenant_id AND v.id = c.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 LEFT JOIN formation_admissions a ON a.tenant_id = c.tenant_id AND a.cohort_id = c.id AND a.membership_id = ?
 WHERE c.tenant_id = ? AND c.id = ? AND v.status = 'published' AND `+institutionalFormationSQL, actor.MembershipID, actor.TenantID, cohortID).Scan(
		&f.FormationID, &f.Name, &f.Description, &f.FormationStatus, &f.EnrollmentPolicy, &f.CohortName, &f.CohortStatus,
		&f.VersionID, &f.Version, &f.AvailableSeats, &starts, &ends, &f.AdmissionStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storeport.ErrFormationUnavailable
	}
	if err != nil {
		return nil, err
	}
	if starts.Valid {
		f.StartsAt = &starts.Time
	}
	if ends.Valid {
		f.EndsAt = &ends.Time
	}
	f.Enrollment, err = s.learnerEnrollment(ctx, actor, cohortID)
	return f, err
}

func offeringOpen(f *models.LearnerFormation, now time.Time) bool {
	return f.FormationStatus == "active" && f.CohortStatus == "open" && (f.EndsAt == nil || now.Before(*f.EndsAt))
}

func (s *Store) ListLearnerFormations(ctx context.Context, actor models.Principal, mine bool, after string, limit int) (models.LearnerFormationPage, error) {
	page := models.LearnerFormationPage{Items: []models.LearnerFormation{}}
	if err := validatePage(limit); err != nil {
		return page, err
	}
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if err := txs.learnerFormationActor(txCtx, actor, models.OAuthScopeLearnerRead); err != nil {
			return err
		}
		own := `EXISTS (SELECT 1 FROM enrollments e WHERE e.tenant_id = c.tenant_id AND e.cohort_id = c.id AND e.membership_id = ?)
 OR EXISTS (SELECT 1 FROM formation_admissions a WHERE a.tenant_id = c.tenant_id AND a.cohort_id = c.id AND a.membership_id = ?)`
		query := `SELECT c.id FROM cohorts c JOIN formation_versions v ON v.tenant_id = c.tenant_id AND v.id = c.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 WHERE c.tenant_id = ? AND v.status = 'published' AND ` + institutionalFormationSQL + ` AND c.id > ? AND (` + own
		args := []any{actor.TenantID, after, actor.MembershipID, actor.MembershipID}
		if !mine {
			query += ` OR (f.status = 'active' AND c.status = 'open' AND (c.ends_at IS NULL OR c.ends_at > ?))`
			args = append(args, time.Now().UTC())
		}
		query += `) ORDER BY c.id LIMIT ?`
		args = append(args, limit+1)
		rows, err := txs.query(txCtx, query, args...)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) > limit {
			ids = ids[:limit]
			page.NextAfter = ids[len(ids)-1]
		}
		for _, id := range ids {
			f, err := txs.learnerOffering(txCtx, actor, id)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, *f)
		}
		return nil
	})
	return page, err
}

func (s *Store) GetLearnerFormation(ctx context.Context, actor models.Principal, cohortID string) (*models.LearnerFormationDetail, error) {
	var out *models.LearnerFormationDetail
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if err := txs.learnerFormationActor(txCtx, actor, models.OAuthScopeLearnerRead); err != nil {
			return err
		}
		f, err := txs.learnerOffering(txCtx, actor, cohortID)
		if err != nil {
			return err
		}
		if !offeringOpen(f, time.Now().UTC()) && f.Enrollment == nil && f.AdmissionStatus == "" {
			return storeport.ErrFormationUnavailable
		}
		d, err := txs.loadFormationVersion(txCtx, actor.TenantID, f.VersionID)
		if err != nil {
			return err
		}
		out = &models.LearnerFormationDetail{Offering: *f, Modules: d.Modules, Concepts: d.Concepts}
		return nil
	})
	return out, err
}

func (s *Store) JoinFormation(ctx context.Context, actor models.Principal, key, cohortID string) (*models.FormationJoinResult, bool, error) {
	return runCatalogMutation(ctx, s, actor, key, "learner.formation.join", models.PermissionLearningSelf,
		struct{ CohortID string }{cohortID}, func(txCtx context.Context, txs *Store) (*models.FormationJoinResult, error) {
			if err := txs.learnerFormationActor(txCtx, actor, models.OAuthScopeLearnerWrite); err != nil {
				return nil, err
			}
			// Same formation-first lock order as staff enrollment, archive and migration.
			if _, err := txs.formationAccess(txCtx, actor, "cohort", cohortID, models.PermissionLearningSelf, true); err != nil {
				return nil, err
			}
			f, err := txs.learnerOffering(txCtx, actor, cohortID)
			if err != nil {
				return nil, err
			}
			e := f.Enrollment
			if e != nil && e.Status == "active" {
				return &models.FormationJoinResult{Status: "active", Enrollment: e}, nil
			}
			if !offeringOpen(f, time.Now().UTC()) {
				return nil, storeport.ErrFormationUnavailable
			}
			if e != nil && (e.Status != "cancelled" || e.DomainID == "" || e.SeatReserved) {
				return nil, storeport.ErrEnrollmentTransition
			}
			switch f.EnrollmentPolicy {
			case "invitation":
				if f.AdmissionStatus != "invited" {
					return nil, storeport.ErrFormationAdmissionRequired
				}
			case "approval":
				if f.AdmissionStatus == "revoked" {
					return nil, storeport.ErrFormationAdmissionRequired
				}
				if f.AdmissionStatus != "approved" {
					if f.AdmissionStatus != "pending" {
						if err := txs.setFormationAdmission(txCtx, actor.TenantID, cohortID, actor.MembershipID, "pending"); err != nil {
							return nil, err
						}
						if err := txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "enrollment.request", TargetType: "cohort", TargetID: cohortID}); err != nil {
							return nil, err
						}
					}
					return &models.FormationJoinResult{Status: "pending"}, nil
				}
			case "open":
			default:
				return nil, storeport.ErrFormationUnavailable
			}
			now := time.Now().UTC()
			result, err := txs.exec(txCtx, `UPDATE cohorts SET reserved_seats = reserved_seats + 1, version = version + 1, updated_at = ?
 WHERE tenant_id = ? AND id = ? AND status = 'open' AND reserved_seats < capacity AND (ends_at IS NULL OR ends_at > ?)`, now, actor.TenantID, cohortID, now)
			if err != nil {
				return nil, err
			}
			if n, _ := result.RowsAffected(); n != 1 {
				return nil, storeport.ErrCohortCapacityReached
			}
			if e == nil {
				id, err := generateID()
				if err != nil {
					return nil, err
				}
				e = &models.Enrollment{ID: id, TenantID: actor.TenantID, CohortID: cohortID, FormationVersionID: f.VersionID,
					UserID: actor.UserID, MembershipID: actor.MembershipID, LearnerID: actor.LearnerID, Status: "active", ObjectivesJSON: "{}", SeatReserved: true, CreatedAt: now, UpdatedAt: now}
				if _, err := txs.exec(txCtx, `INSERT INTO enrollments (id, tenant_id, cohort_id, formation_version_id, user_id, membership_id, learner_id, status, objectives_json, seat_reserved, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, 'active', '{}', 1, ?, ?)`, e.ID, e.TenantID, e.CohortID, e.FormationVersionID, e.UserID, e.MembershipID, e.LearnerID, now, now); err != nil {
					return nil, err
				}
				if err := txs.createFormationDomain(txCtx, e, actor); err != nil {
					return nil, err
				}
			} else {
				if _, err := txs.exec(txCtx, `UPDATE enrollments SET status = 'active', seat_reserved = 1, updated_at = ? WHERE tenant_id = ? AND id = ?`, now, actor.TenantID, e.ID); err != nil {
					return nil, err
				}
				if _, err := txs.exec(txCtx, `UPDATE domains SET archived = 0 WHERE tenant_id = ? AND id = ? AND learner_id = ?`, actor.TenantID, e.DomainID, actor.LearnerID); err != nil {
					return nil, err
				}
				e.Status, e.SeatReserved, e.UpdatedAt = "active", true, now
			}
			if err := txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "enrollment.join", TargetType: "enrollment", TargetID: e.ID}); err != nil {
				return nil, err
			}
			return &models.FormationJoinResult{Status: "active", Enrollment: e}, nil
		})
}

func (s *Store) LeaveFormation(ctx context.Context, actor models.Principal, key, cohortID string) (*models.FormationJoinResult, bool, error) {
	return runCatalogMutation(ctx, s, actor, key, "learner.formation.leave", models.PermissionLearningSelf,
		struct{ CohortID string }{cohortID}, func(txCtx context.Context, txs *Store) (*models.FormationJoinResult, error) {
			if err := txs.learnerFormationActor(txCtx, actor, models.OAuthScopeLearnerWrite); err != nil {
				return nil, err
			}
			if _, err := txs.formationAccess(txCtx, actor, "cohort", cohortID, models.PermissionLearningSelf, true); err != nil {
				return nil, err
			}
			f, err := txs.learnerOffering(txCtx, actor, cohortID)
			if err != nil {
				return nil, err
			}
			if f.AdmissionStatus == "pending" {
				if err := txs.setFormationAdmission(txCtx, actor.TenantID, cohortID, actor.MembershipID, "cancelled"); err != nil {
					return nil, err
				}
				if err := txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "enrollment.request.cancel", TargetType: "cohort", TargetID: cohortID}); err != nil {
					return nil, err
				}
			}
			e := f.Enrollment
			if e == nil || e.Status == "cancelled" {
				return &models.FormationJoinResult{Status: "cancelled", Enrollment: e}, nil
			}
			if e.Status != "active" || !e.SeatReserved || e.DomainID == "" {
				return nil, storeport.ErrEnrollmentTransition
			}
			now := time.Now().UTC()
			if txs.dialect == DialectPostgres {
				var lockedID string
				if err := txs.queryRow(txCtx, `SELECT id FROM domains WHERE tenant_id = ? AND id = ? AND learner_id = ? FOR UPDATE`, actor.TenantID, e.DomainID, actor.LearnerID).Scan(&lockedID); err != nil {
					return nil, err
				}
			}
			if _, err := txs.exec(txCtx, `UPDATE enrollments SET status = 'cancelled', seat_reserved = 0, updated_at = ? WHERE tenant_id = ? AND id = ?`, now, actor.TenantID, e.ID); err != nil {
				return nil, err
			}
			result, err := txs.exec(txCtx, `UPDATE cohorts SET reserved_seats = reserved_seats - 1, version = version + 1, updated_at = ? WHERE tenant_id = ? AND id = ? AND reserved_seats > 0`, now, actor.TenantID, cohortID)
			if err != nil {
				return nil, err
			}
			if n, _ := result.RowsAffected(); n != 1 {
				return nil, storeport.ErrEnrollmentTransition
			}
			if err := txs.ArchiveDomain(txCtx, e.DomainID, actor.LearnerID); err != nil {
				return nil, err
			}
			if _, err := txs.exec(txCtx, `UPDATE learning_sessions SET status = 'closed', closed_at = ? WHERE tenant_id = ? AND learner_id = ? AND domain_id = ? AND status = 'open'`, now, actor.TenantID, actor.LearnerID, e.DomainID); err != nil {
				return nil, err
			}
			e.Status, e.SeatReserved, e.UpdatedAt = "cancelled", false, now
			if err := txs.AppendAuditEvent(txCtx, actor, models.AuditEvent{Action: "enrollment.leave", TargetType: "enrollment", TargetID: e.ID}); err != nil {
				return nil, err
			}
			return &models.FormationJoinResult{Status: "cancelled", Enrollment: e}, nil
		})
}
