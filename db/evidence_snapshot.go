// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"tutor-mcp/models"
)

// GetTransferScoresBatchInDomain loads transfer evidence for all requested
// concepts in one round trip. The ordering within each concept is identical to
// GetTransferScoresInDomain: newest first.
func (s *Store) GetTransferScoresBatchInDomain(ctx context.Context, learnerID, domainID string, concepts []string) (map[string][]*models.TransferRecord, error) {
	concepts = normalizeEvidenceConcepts(concepts)
	out := make(map[string][]*models.TransferRecord, len(concepts))
	if len(concepts) == 0 {
		return out, nil
	}

	args := make([]any, 0, 2+len(concepts))
	args = append(args, learnerID, domainID)
	placeholders := make([]string, 0, len(concepts))
	for _, concept := range concepts {
		out[concept] = nil
		placeholders = append(placeholders, "?")
		args = append(args, concept)
	}

	rows, err := s.query(ctx, `SELECT id, learner_id, domain_id, assessment_attempt_id,
       concept_id, context_type, score, session_id, created_at
       FROM transfer_records
       WHERE learner_id = ? AND domain_id = ?
         AND concept_id IN (`+strings.Join(placeholders, ",")+`)
       ORDER BY concept_id, created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("get transfer scores batch: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		r := &models.TransferRecord{}
		var attemptID sql.NullString
		if err := rows.Scan(
			&r.ID, &r.LearnerID, &r.DomainID, &attemptID, &r.ConceptID,
			&r.ContextType, &r.Score, &r.SessionID, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan transfer evidence batch: %w", err)
		}
		r.AssessmentAttemptID = attemptID.String
		out[r.ConceptID] = append(out[r.ConceptID], r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transfer evidence batch: %w", err)
	}
	return out, nil
}

// GetEvaluatedAssessmentAttemptsBatchInDomain loads at most limitPerConcept
// evaluated envelopes for each requested concept in one round trip. ROW_NUMBER
// preserves the existing per-concept cap rather than applying one global LIMIT.
func (s *Store) GetEvaluatedAssessmentAttemptsBatchInDomain(ctx context.Context, learnerID, domainID string, concepts []string, limitPerConcept int) (map[string][]*models.AssessmentAttempt, error) {
	if limitPerConcept <= 0 {
		limitPerConcept = 50
	}
	concepts = normalizeEvidenceConcepts(concepts)
	out := make(map[string][]*models.AssessmentAttempt, len(concepts))
	if len(concepts) == 0 {
		return out, nil
	}

	args := make([]any, 0, 3+len(concepts))
	args = append(args, learnerID, domainID)
	placeholders := make([]string, 0, len(concepts))
	for _, concept := range concepts {
		out[concept] = nil
		placeholders = append(placeholders, "?")
		args = append(args, concept)
	}
	args = append(args, limitPerConcept)

	rows, err := s.query(ctx, `WITH ranked_evidence AS (
       SELECT `+assessmentEvidenceColumns+`,
              ROW_NUMBER() OVER (
                  PARTITION BY a.concept_id ORDER BY a.evaluated_at DESC
              ) AS evidence_rank
       FROM assessment_attempts a
       JOIN domains d ON d.id = a.domain_id AND d.learner_id = a.learner_id
       WHERE a.learner_id = ? AND a.domain_id = ?
         AND a.concept_id IN (`+strings.Join(placeholders, ",")+`)
         AND a.status = 'evaluated'
         AND a.submitted_at IS NOT NULL AND a.evaluated_at IS NOT NULL
       )
       SELECT `+assessmentColumns+`
       FROM ranked_evidence
       WHERE evidence_rank <= ?
       ORDER BY concept_id, evaluated_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("get evaluated assessment attempts batch: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		a, err := scanAssessmentValues(rows)
		if err != nil {
			return nil, err
		}
		out[a.ConceptID] = append(out[a.ConceptID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evaluated assessment attempts batch: %w", err)
	}
	return out, nil
}

func normalizeEvidenceConcepts(concepts []string) []string {
	out := make([]string, 0, len(concepts))
	seen := make(map[string]struct{}, len(concepts))
	for _, concept := range concepts {
		if concept == "" {
			continue
		}
		if _, ok := seen[concept]; ok {
			continue
		}
		seen[concept] = struct{}{}
		out = append(out, concept)
	}
	return out
}
