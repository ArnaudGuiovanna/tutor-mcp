// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var _ storeport.QualifiedDiagnosticObservationReader = (*Store)(nil)

// ListQualifiedDiagnosticObservationsInDomain returns, oldest first, every
// diagnostic observation of scope.LearnerID in domainID since since that
// qualifies under the same conditions as
// GetQualifiedDiagnosticConceptsSinceInDomain: an evaluated, unassisted
// DIAGNOSTIC_ASSESSMENT interaction of the current curriculum bound to its
// submitted assessment attempt. Unlike that concept list it keeps each
// observation with its outcome and time. It runs in the caller's tenant
// transaction, or opens a read-only one bound to scope.
func (s *Store) ListQualifiedDiagnosticObservationsInDomain(ctx context.Context, scope models.TenantScope, domainID string, since time.Time) ([]models.DiagnosticObservation, error) {
	if err := scope.Validate(); err != nil || scope.LearnerID == "" || domainID == "" {
		return nil, fmt.Errorf("list qualified diagnostic observations: %w", storeport.ErrInvalidPrincipal)
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var observations []models.DiagnosticObservation
		err := s.WithTenantReadTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			observations, innerErr = scoped.(*Store).ListQualifiedDiagnosticObservationsInDomain(txCtx, scope, domainID, since)
			return innerErr
		})
		return observations, err
	}
	rows, err := s.query(ctx,
		`SELECT i.concept, i.success, i.created_at
		 FROM `+currentInteractionsSQL+` i
		 JOIN assessment_attempts a
		   ON a.id = i.assessment_attempt_id
		  AND a.learner_id = i.learner_id
		  AND a.domain_id = i.domain_id
		  AND a.concept_id = i.concept
		  AND a.activity_type = i.activity_type
		 WHERE i.learner_id = ? AND i.domain_id = ? AND i.created_at >= ?
		   AND i.activity_type = 'DIAGNOSTIC_ASSESSMENT'
		   AND i.hints_requested = 0
		   AND a.status = 'evaluated' AND a.curriculum_invalidated_version = 0
		   AND a.submitted_at IS NOT NULL
		   AND a.evaluated_at IS NOT NULL
		 ORDER BY i.created_at, i.id`,
		scope.LearnerID, domainID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("list qualified diagnostic observations: %w", err)
	}
	defer rows.Close()
	var observations []models.DiagnosticObservation
	for rows.Next() {
		var observation models.DiagnosticObservation
		var observedAt flexTime
		if err := rows.Scan(&observation.Concept, &observation.Success, &observedAt); err != nil {
			return nil, fmt.Errorf("scan qualified diagnostic observation: %w", err)
		}
		observation.ObservedAt = observedAt.Time.UTC()
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate qualified diagnostic observations: %w", err)
	}
	return observations, nil
}

// ListConceptsWithNonPlacementEvidenceInDomain returns, sorted, the concepts
// of domainID with at least one current interaction of scope.LearnerID that
// ListQualifiedDiagnosticObservationsInDomain(since) would not return. The
// whole history is read: a placement must never overwrite the estimate of a
// concept that has any other evidence, however old. The CASE makes an
// interaction with a NULL qualifying column count as other evidence.
func (s *Store) ListConceptsWithNonPlacementEvidenceInDomain(ctx context.Context, scope models.TenantScope, domainID string, since time.Time) ([]string, error) {
	if err := scope.Validate(); err != nil || scope.LearnerID == "" || domainID == "" {
		return nil, fmt.Errorf("list concepts with non-placement evidence: %w", storeport.ErrInvalidPrincipal)
	}
	if !s.inTenantTransaction(ctx, scope) && s.root != nil {
		var concepts []string
		err := s.WithTenantReadTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
			var innerErr error
			concepts, innerErr = scoped.(*Store).ListConceptsWithNonPlacementEvidenceInDomain(txCtx, scope, domainID, since)
			return innerErr
		})
		return concepts, err
	}
	rows, err := s.query(ctx,
		`SELECT DISTINCT i.concept
		 FROM `+currentInteractionsSQL+` i
		 LEFT JOIN assessment_attempts a
		   ON a.id = i.assessment_attempt_id
		  AND a.learner_id = i.learner_id
		  AND a.domain_id = i.domain_id
		  AND a.concept_id = i.concept
		  AND a.activity_type = i.activity_type
		  AND a.status = 'evaluated' AND a.curriculum_invalidated_version = 0
		  AND a.submitted_at IS NOT NULL
		  AND a.evaluated_at IS NOT NULL
		 WHERE i.learner_id = ? AND i.domain_id = ?
		   AND CASE WHEN i.created_at >= ?
		             AND i.activity_type = 'DIAGNOSTIC_ASSESSMENT'
		             AND i.hints_requested = 0
		             AND a.id IS NOT NULL
		       THEN 0 ELSE 1 END = 1
		 ORDER BY i.concept`,
		scope.LearnerID, domainID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("list concepts with non-placement evidence: %w", err)
	}
	defer rows.Close()
	var concepts []string
	for rows.Next() {
		var concept string
		if err := rows.Scan(&concept); err != nil {
			return nil, fmt.Errorf("scan concept with non-placement evidence: %w", err)
		}
		concepts = append(concepts, concept)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate concepts with non-placement evidence: %w", err)
	}
	return concepts, nil
}
