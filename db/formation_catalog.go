// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (s *Store) ListFormationVersions(ctx context.Context, actor models.Principal, formationID, after string, limit int) (models.FormationVersionPage, error) {
	page := models.FormationVersionPage{Items: []models.FormationVersion{}}
	var cursor int64
	if validatePage(limit) != nil {
		return page, fmt.Errorf("invalid version page")
	}
	if after != "" {
		var err error
		cursor, err = strconv.ParseInt(after, 10, 64)
		if err != nil || cursor < 1 || strconv.FormatInt(cursor, 10) != after {
			return page, fmt.Errorf("invalid version cursor")
		}
	}
	err := s.WithTenantReadTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.formationAccess(txCtx, actor, "formation", formationID, models.PermissionFormationWrite, false); err != nil {
			return err
		}
		rows, err := txs.query(txCtx, `SELECT id, version, status, metadata_json, created_by, created_at, published_at FROM formation_versions WHERE tenant_id=? AND formation_id=? AND (?=0 OR version<?) ORDER BY version DESC LIMIT ?`, actor.TenantID, formationID, cursor, cursor, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v := models.FormationVersion{TenantID: actor.TenantID, FormationID: formationID}
			var published sql.NullTime
			if err := rows.Scan(&v.ID, &v.Version, &v.Status, &v.MetadataJSON, &v.CreatedBy, &v.CreatedAt, &published); err != nil {
				return err
			}
			if published.Valid {
				v.PublishedAt = &published.Time
			}
			page.Items = append(page.Items, v)
		}
		return rows.Err()
	})
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = strconv.FormatInt(page.Items[limit-1].Version, 10)
	}
	return page, err
}

func (s *Store) ReviewFormationVersion(ctx context.Context, actor models.Principal, versionID string) (*models.FormationReview, error) {
	detail, err := s.GetFormationVersion(ctx, actor, versionID)
	if err != nil {
		return nil, err
	}
	review := &models.FormationReview{Detail: detail, Issues: []string{}}
	err = s.WithTenantReadTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		err := txs.queryRow(txCtx, `SELECT c.id FROM cohorts c JOIN formations f ON f.tenant_id=c.tenant_id JOIN formation_versions v ON v.tenant_id=c.tenant_id AND v.id=c.formation_version_id AND v.formation_id=f.id WHERE c.tenant_id=? AND v.id=? AND c.status='open' AND f.enrollment_policy='open' AND f.status<>'archived' ORDER BY c.id LIMIT 1`, actor.TenantID, versionID).Scan(&review.CatalogCohortID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		review.CatalogAvailable = err == nil
		return err
	})
	if err != nil {
		return nil, err
	}
	if detail.Formation.Status == "archived" {
		review.Issues = append(review.Issues, "Cette formation est archivée.")
	}
	if detail.Version.Status != "draft" {
		review.Issues = append(review.Issues, "Cette version est déjà publiée et son contenu est immuable.")
	}
	if len(detail.Modules) == 0 || len(detail.Concepts) == 0 {
		review.Issues = append(review.Issues, "Ajoutez au moins un module et un concept avant de publier.")
	}
	if len(detail.Concepts) > 0 {
		if _, err := formationCurriculum(detail, "validation", actor.UserID, time.Now().UTC()); err != nil {
			review.Issues = append(review.Issues, "Le programme doit être corrigé : "+err.Error())
		}
	}
	review.Publishable = len(review.Issues) == 0
	return review, nil
}

// PublishFormationToCatalogIdempotent publishes content and opens one offering
// for this institution, atomically. It never enrolls a member. Existing learners
// retain their version when a newer catalogue offering replaces the previous one.
func (s *Store) PublishFormationToCatalogIdempotent(ctx context.Context, actor models.Principal, key, versionID string) (*models.FormationPublication, bool, error) {
	if !actor.Authorize(models.PermissionCohortManage, models.AuthorizationResource{TenantID: actor.TenantID}) {
		return nil, false, storeport.ErrInvalidPrincipal
	}
	request := struct{ VersionID string }{versionID}
	result, replayed, err := runCatalogMutation(ctx, s, actor, key, "formation.catalog.publish", models.PermissionFormationWrite, request, func(txCtx context.Context, root *Store) (models.FormationPublication, error) {
		var result models.FormationPublication
		err := root.WithTenantTx(txCtx, actor.TenantScope(), func(innerCtx context.Context, scoped storeport.Store) error {
			txs := scoped.(*Store)
			f, err := txs.formationAccess(innerCtx, actor, "version", versionID, models.PermissionFormationWrite, true)
			if err != nil {
				return err
			}
			if f.Status == "archived" {
				return storeport.ErrFormationVersionImmutable
			}
			detail, err := txs.loadFormationVersion(innerCtx, actor.TenantID, versionID)
			if err != nil {
				return err
			}
			version := &detail.Version
			if version.Status == "draft" {
				version, err = root.PublishFormationVersion(innerCtx, actor, versionID)
				if err != nil {
					return err
				}
			}
			if version.Status != "published" {
				return storeport.ErrFormationVersionImmutable
			}
			if f.EnrollmentPolicy != "open" {
				if err = root.SetFormationEnrollmentPolicy(innerCtx, actor, f.ID, "open"); err != nil {
					return err
				}
			}
			digest := sha256.Sum256([]byte(actor.TenantID + "\x00" + versionID))
			id := "catalog_" + hex.EncodeToString(digest[:16])
			// Only retire automatic catalogue offerings; explicit cohorts and
			// their assignments/admissions are unchanged.
			now := time.Now().UTC()
			if _, err = txs.exec(innerCtx, `UPDATE cohorts SET status='closed',version=version+1,updated_at=? WHERE tenant_id=? AND substr(id,1,8)='catalog_' AND status='open' AND formation_version_id<>? AND formation_version_id IN (SELECT id FROM formation_versions WHERE tenant_id=? AND formation_id=?)`, now, actor.TenantID, versionID, actor.TenantID, f.ID); err != nil {
				return err
			}
			cohort := models.Cohort{ID: id, TenantID: actor.TenantID, FormationVersionID: versionID, Name: f.Name, Capacity: 1000000, Status: "open", Version: 1, CreatedBy: actor.UserID, CreatedAt: now, UpdatedAt: now}
			var existing string
			err = txs.queryRow(innerCtx, `SELECT id FROM cohorts WHERE tenant_id=? AND id=? AND formation_version_id=?`, actor.TenantID, id, versionID).Scan(&existing)
			if errors.Is(err, sql.ErrNoRows) {
				_, err = txs.exec(innerCtx, `INSERT INTO cohorts(id,tenant_id,formation_version_id,name,starts_at,ends_at,capacity,reserved_seats,status,version,created_by,created_at,updated_at) VALUES(?,?,?,?,NULL,NULL,?,0,'open',1,?,?,?)`, id, actor.TenantID, versionID, f.Name, cohort.Capacity, actor.UserID, now, now)
				if err == nil {
					err = txs.AppendAuditEvent(innerCtx, actor, models.AuditEvent{Action: "formation.catalog.publish", TargetType: "cohort", TargetID: id})
				}
			} else if err == nil {
				err = txs.queryRow(innerCtx, `SELECT name,capacity,reserved_seats,status,version,created_by,created_at,updated_at FROM cohorts WHERE tenant_id=? AND id=?`, actor.TenantID, id).Scan(&cohort.Name, &cohort.Capacity, &cohort.ReservedSeats, &cohort.Status, &cohort.Version, &cohort.CreatedBy, &cohort.CreatedAt, &cohort.UpdatedAt)
				if err == nil && cohort.Status != "open" {
					return storeport.ErrFormationUnavailable
				}
			}
			if err != nil {
				return err
			}
			result = models.FormationPublication{Version: *version, Cohort: cohort}
			return nil
		})
		return result, err
	})
	if err != nil {
		return nil, false, err
	}
	return &result, replayed, nil
}
