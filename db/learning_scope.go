// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"tutor-mcp/models"
)

type learningScopeIDs struct {
	TenantID           string
	EnrollmentID       string
	FormationConceptID string
}

func stableLearningID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0x1f})
	}
	return prefix + hex.EncodeToString(h.Sum(nil))[:24]
}

// provisionDomainEnrollment is the expand/contract dual-write boundary for
// the legacy domain API. The legacy domain and the target catalogue graph are
// committed by the same transaction, so no caller can observe only one side.
func (s *Store) provisionDomainEnrollment(ctx context.Context, domain *models.Domain) error {
	if domain == nil || domain.ID == "" || domain.LearnerID == "" {
		return fmt.Errorf("provision domain enrollment: domain is required")
	}
	var tenantID, userID, membershipID string
	if err := s.queryRow(ctx, `SELECT tenant_id, user_id, membership_id
		FROM learners WHERE id = ?`, domain.LearnerID).Scan(&tenantID, &userID, &membershipID); err != nil {
		return fmt.Errorf("provision domain enrollment identity: %w", err)
	}
	formationID := "domain_formation_" + domain.ID
	versionID := "domain_version_" + domain.ID
	moduleID := "domain_module_" + domain.ID
	cohortID := "domain_cohort_" + domain.ID
	enrollmentID := "domain_enrollment_" + domain.ID
	now := domain.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	if _, err := s.exec(ctx, `INSERT INTO formations
		(id, tenant_id, name, description, status, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?)`,
		formationID, tenantID, domain.Name, domain.PersonalGoal, userID, now, now); err != nil {
		return fmt.Errorf("provision domain formation: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO formation_versions
		(id, tenant_id, formation_id, version, status, metadata_json, created_by, created_at)
		VALUES (?, ?, ?, 1, 'draft', ?, ?, ?)`,
		versionID, tenantID, formationID, `{"legacy_domain_dual_write":true}`, userID, now); err != nil {
		return fmt.Errorf("provision domain formation version: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO formation_modules
		(id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
		VALUES (?, ?, ?, 'legacy_domain', ?, 0, ?)`,
		moduleID, tenantID, versionID, domain.Name, `{"legacy_domain_dual_write":true}`); err != nil {
		return fmt.Errorf("provision domain module: %w", err)
	}

	concepts := append([]string(nil), domain.Graph.Concepts...)
	sort.Strings(concepts)
	conceptIDs := make(map[string]string, len(concepts))
	position := 0
	for i, concept := range concepts {
		if concept == "" {
			continue
		}
		if _, exists := conceptIDs[concept]; exists {
			continue
		}
		conceptID := stableLearningID("domain_concept_", tenantID, domain.ID, concept)
		conceptIDs[concept] = conceptID
		if _, err := s.exec(ctx, `INSERT INTO formation_concepts
			(id, tenant_id, formation_version_id, module_id, stable_key, label, position, metadata_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			conceptID, tenantID, versionID, moduleID,
			stableLearningID("concept_", concept), concept, position, `{"legacy_domain_dual_write":true}`); err != nil {
			return fmt.Errorf("provision domain concept %d: %w", i, err)
		}
		position++
	}
	unmappedConceptID := stableLearningID("domain_unmapped_", tenantID, domain.ID)
	if _, err := s.exec(ctx, `INSERT INTO formation_concepts
		(id, tenant_id, formation_version_id, module_id, stable_key, label, position, metadata_json)
		VALUES (?, ?, ?, ?, 'legacy_unmapped', 'Unmapped legacy evidence', ?, ?)`,
		unmappedConceptID, tenantID, versionID, moduleID, position,
		`{"legacy_domain_dual_write":true,"quarantine":true,"unmapped":true}`); err != nil {
		return fmt.Errorf("provision unmapped domain concept: %w", err)
	}
	for concept, prerequisites := range domain.Graph.Prerequisites {
		conceptID := conceptIDs[concept]
		if conceptID == "" {
			continue
		}
		for _, prerequisite := range prerequisites {
			prerequisiteID := conceptIDs[prerequisite]
			if prerequisiteID == "" || prerequisiteID == conceptID {
				continue
			}
			if _, err := s.exec(ctx, `INSERT INTO concept_prerequisites
				(tenant_id, formation_version_id, concept_id, prerequisite_id)
				VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				tenantID, versionID, conceptID, prerequisiteID); err != nil {
				return fmt.Errorf("provision domain prerequisite: %w", err)
			}
		}
	}
	if _, err := s.exec(ctx, `UPDATE formation_versions
		SET status = 'published', published_at = ?
		WHERE tenant_id = ? AND id = ? AND status = 'draft'`, now, tenantID, versionID); err != nil {
		return fmt.Errorf("publish provisioned domain version: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO cohorts
		(id, tenant_id, formation_version_id, name, capacity, reserved_seats, status,
		 version, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, 1, 'open', 1, ?, ?, ?)`,
		cohortID, tenantID, versionID, "Individual: "+domain.Name, userID, now, now); err != nil {
		return fmt.Errorf("provision domain cohort: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO enrollments
		(id, tenant_id, cohort_id, formation_version_id, user_id, membership_id,
		 learner_id, status, objectives_json, seat_reserved, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, 1, ?, ?)`,
		enrollmentID, tenantID, cohortID, versionID, userID, membershipID,
		domain.LearnerID, `{"legacy_domain_dual_write":true}`, now, now); err != nil {
		return fmt.Errorf("provision domain enrollment: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO legacy_domain_enrollments
		(tenant_id, learner_id, domain_id, enrollment_id) VALUES (?, ?, ?, ?)`,
		tenantID, domain.LearnerID, domain.ID, enrollmentID); err != nil {
		return fmt.Errorf("map domain enrollment: %w", err)
	}
	for concept, conceptID := range conceptIDs {
		if _, err := s.exec(ctx, `INSERT INTO legacy_concept_sources
			(tenant_id, learner_id, enrollment_id, domain_id, concept_label, concept_id)
			VALUES (?, ?, ?, ?, ?, ?)`,
			tenantID, domain.LearnerID, enrollmentID, domain.ID, concept, conceptID); err != nil {
			return fmt.Errorf("map domain concept source: %w", err)
		}
		if _, err := s.exec(ctx, `INSERT INTO legacy_concept_mappings
			(tenant_id, enrollment_id, domain_id, concept_label, concept_id)
			VALUES (?, ?, ?, ?, ?)`,
			tenantID, enrollmentID, domain.ID, concept, conceptID); err != nil {
			return fmt.Errorf("map domain concept: %w", err)
		}
	}
	if _, err := s.exec(ctx, `INSERT INTO legacy_concept_sources
		(tenant_id, learner_id, enrollment_id, domain_id, concept_label, concept_id)
		VALUES (?, ?, ?, ?, '', ?)`,
		tenantID, domain.LearnerID, enrollmentID, domain.ID, unmappedConceptID); err != nil {
		return fmt.Errorf("map unmapped domain concept source: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO legacy_concept_mappings
		(tenant_id, enrollment_id, domain_id, concept_label, concept_id)
		VALUES (?, ?, ?, '', ?)`, tenantID, enrollmentID, domain.ID, unmappedConceptID); err != nil {
		return fmt.Errorf("map unmapped domain concept: %w", err)
	}
	return nil
}

func (s *Store) resolveLearningScope(ctx context.Context, learnerID, domainID, concept string) (learningScopeIDs, error) {
	out, err := s.queryLearningScope(ctx, learnerID, domainID, concept)
	if err == nil {
		return out, nil
	}
	if err != sql.ErrNoRows {
		return learningScopeIDs{}, fmt.Errorf("resolve learning scope: %w", err)
	}
	if err := s.ensureRecoveryEnrollment(ctx, learnerID); err != nil {
		return learningScopeIDs{}, err
	}
	// A missing/unknown legacy domain is never guessed into a real formation.
	// It is attached to the learner's explicit quarantine enrollment instead.
	out, err = s.queryLearningScope(ctx, learnerID, "", concept)
	if err != nil {
		return learningScopeIDs{}, fmt.Errorf("resolve learning scope: %w", err)
	}
	return out, nil
}

func (s *Store) queryLearningScope(ctx context.Context, learnerID, domainID, concept string) (learningScopeIDs, error) {
	var out learningScopeIDs
	err := s.queryRow(ctx, `SELECT mapping.tenant_id, mapping.enrollment_id, concept.concept_id
		FROM legacy_domain_enrollments mapping
		JOIN legacy_concept_mappings concept
		  ON concept.tenant_id = mapping.tenant_id
		 AND concept.enrollment_id = mapping.enrollment_id
		 AND concept.domain_id = mapping.domain_id
		WHERE mapping.learner_id = ? AND mapping.domain_id = ?
		  AND concept.concept_label IN (?, '')
		ORDER BY CASE WHEN concept.concept_label = ? THEN 0 ELSE 1 END
		LIMIT 1`, learnerID, domainID, concept, concept).
		Scan(&out.TenantID, &out.EnrollmentID, &out.FormationConceptID)
	return out, err
}

func (s *Store) ensureRecoveryEnrollment(ctx context.Context, learnerID string) error {
	return s.inTx(ctx, nil, func(txs *Store) error {
		lockClause := ""
		if txs.dialect == DialectPostgres {
			lockClause = " FOR UPDATE"
		}
		var tenantID, userID, membershipID string
		var createdAt time.Time
		if err := txs.queryRow(ctx, `SELECT tenant_id, user_id, membership_id, created_at
			FROM learners WHERE id = ?`+lockClause, learnerID).
			Scan(&tenantID, &userID, &membershipID, &createdAt); err != nil {
			return fmt.Errorf("resolve recovery identity: %w", err)
		}
		var existing int
		if err := txs.queryRow(ctx, `SELECT COUNT(*) FROM legacy_domain_enrollments
			WHERE tenant_id = ? AND learner_id = ? AND domain_id = ''`, tenantID, learnerID).
			Scan(&existing); err != nil {
			return fmt.Errorf("check recovery enrollment: %w", err)
		}
		if existing != 0 {
			return nil
		}

		formationID := "legacy_recovery_formation_" + learnerID
		versionID := "legacy_recovery_version_" + learnerID
		moduleID := "legacy_recovery_module_" + learnerID
		cohortID := "legacy_recovery_cohort_" + learnerID
		enrollmentID := "legacy_recovery_enrollment_" + learnerID
		conceptID := stableLearningID("legacy_unmapped_", tenantID, enrollmentID)
		if _, err := txs.exec(ctx, `INSERT INTO formations
			(id, tenant_id, name, description, status, created_by, created_at, updated_at)
			VALUES (?, ?, 'Legacy recovery', 'Quarantined evidence without a selected domain',
			        'active', ?, ?, ?)`, formationID, tenantID, userID, createdAt, createdAt); err != nil {
			return fmt.Errorf("create recovery formation: %w", err)
		}
		if _, err := txs.exec(ctx, `INSERT INTO formation_versions
			(id, tenant_id, formation_id, version, status, metadata_json, created_by, created_at)
			VALUES (?, ?, ?, 1, 'draft', ?, ?, ?)`,
			versionID, tenantID, formationID, `{"quarantine":true}`, userID, createdAt); err != nil {
			return fmt.Errorf("create recovery version: %w", err)
		}
		if _, err := txs.exec(ctx, `INSERT INTO formation_modules
			(id, tenant_id, formation_version_id, stable_key, title, position, metadata_json)
			VALUES (?, ?, ?, 'recovery', 'Recovery', 0, ?)`,
			moduleID, tenantID, versionID, `{"quarantine":true}`); err != nil {
			return fmt.Errorf("create recovery module: %w", err)
		}
		if _, err := txs.exec(ctx, `INSERT INTO formation_concepts
			(id, tenant_id, formation_version_id, module_id, stable_key, label, position, metadata_json)
			VALUES (?, ?, ?, ?, 'legacy_unmapped', 'Unmapped legacy evidence', 0, ?)`,
			conceptID, tenantID, versionID, moduleID, `{"quarantine":true,"unmapped":true}`); err != nil {
			return fmt.Errorf("create recovery concept: %w", err)
		}
		if _, err := txs.exec(ctx, `UPDATE formation_versions SET status = 'published', published_at = ?
			WHERE tenant_id = ? AND id = ?`, createdAt, tenantID, versionID); err != nil {
			return fmt.Errorf("publish recovery version: %w", err)
		}
		if _, err := txs.exec(ctx, `INSERT INTO cohorts
			(id, tenant_id, formation_version_id, name, capacity, reserved_seats, status,
			 version, created_by, created_at, updated_at)
			VALUES (?, ?, ?, 'Legacy recovery', 1, 1, 'archived', 1, ?, ?, ?)`,
			cohortID, tenantID, versionID, userID, createdAt, createdAt); err != nil {
			return fmt.Errorf("create recovery cohort: %w", err)
		}
		if _, err := txs.exec(ctx, `INSERT INTO enrollments
			(id, tenant_id, cohort_id, formation_version_id, user_id, membership_id,
			 learner_id, status, objectives_json, seat_reserved, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'completed', ?, 1, ?, ?)`,
			enrollmentID, tenantID, cohortID, versionID, userID, membershipID, learnerID,
			`{"quarantine":true}`, createdAt, createdAt); err != nil {
			return fmt.Errorf("create recovery enrollment: %w", err)
		}
		if _, err := txs.exec(ctx, `INSERT INTO legacy_domain_enrollments
			(tenant_id, learner_id, domain_id, enrollment_id) VALUES (?, ?, '', ?)`,
			tenantID, learnerID, enrollmentID); err != nil {
			return fmt.Errorf("map recovery enrollment: %w", err)
		}
		if _, err := txs.exec(ctx, `INSERT INTO legacy_concept_sources
			(tenant_id, learner_id, enrollment_id, domain_id, concept_label, concept_id)
			VALUES (?, ?, ?, '', '', ?)`, tenantID, learnerID, enrollmentID, conceptID); err != nil {
			return fmt.Errorf("map recovery source: %w", err)
		}
		if _, err := txs.exec(ctx, `INSERT INTO legacy_concept_mappings
			(tenant_id, enrollment_id, domain_id, concept_label, concept_id)
			VALUES (?, ?, '', '', ?)`, tenantID, enrollmentID, conceptID); err != nil {
			return fmt.Errorf("map recovery concept: %w", err)
		}
		return nil
	})
}

func (s *Store) EnsureRecoveryEnrollment(ctx context.Context, learnerID string) error {
	return s.ensureRecoveryEnrollment(ctx, learnerID)
}
