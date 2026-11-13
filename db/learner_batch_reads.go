// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Batched learner reads used by multi-domain MCP views. Each method answers
// for several domains of one learner with a single statement instead of one
// query per domain. They take the caller's models.TenantScope: a scope bound
// to a learner may only read that learner, and outside a tenant transaction
// the call opens one for the scope, like the other scoped Store methods.

func batchLearnerScope(scope models.TenantScope, learnerID string) error {
	if learnerID == "" || (scope.LearnerID != "" && scope.LearnerID != learnerID) {
		return storeport.ErrInvalidPrincipal
	}
	return nil
}

// withBatchReadScope runs fn on a store bound to scope. A pool-level store
// outside a matching tenant transaction opens a read transaction first.
func (s *Store) withBatchReadScope(ctx context.Context, scope models.TenantScope, fn func(context.Context, *Store) error) error {
	if s.root != nil && !s.inTenantTransaction(ctx, scope) && scope.Validate() == nil && scope.TenantID != models.LegacyTenantID {
		return s.WithTenantReadTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
			return fn(txCtx, scoped.(*Store))
		})
	}
	return fn(ctx, s)
}

func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func uniqueDomainIDs(domainIDs []string) []string {
	seen := make(map[string]bool, len(domainIDs))
	out := make([]string, 0, len(domainIDs))
	for _, id := range domainIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// GetConceptStatesByDomains returns the learner's concept states grouped by
// domain ID for every requested domain (missing domains map to no entry).
func (s *Store) GetConceptStatesByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string) (map[string][]*models.ConceptState, error) {
	if err := batchLearnerScope(scope, learnerID); err != nil {
		return nil, err
	}
	ids := uniqueDomainIDs(domainIDs)
	out := make(map[string][]*models.ConceptState, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := s.withBatchReadScope(ctx, scope, func(ctx context.Context, txs *Store) error {
		args := append([]any{learnerID}, anySlice(ids)...)
		rows, err := txs.query(ctx, `SELECT id, learner_id, domain_id, concept, stability, difficulty, elapsed_days, scheduled_days,
			        reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
			        p_slip, p_guess, theta, updated_at
			 FROM concept_states WHERE learner_id = ? AND domain_id IN (`+inPlaceholders(len(ids))+`)`, args...)
		if err != nil {
			return fmt.Errorf("get concept states by domains: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			cs := &models.ConceptState{}
			var lastReview, nextReview sql.NullTime
			if err := rows.Scan(
				&cs.ID, &cs.LearnerID, &cs.DomainID, &cs.Concept, &cs.Stability, &cs.Difficulty,
				&cs.ElapsedDays, &cs.ScheduledDays, &cs.Reps, &cs.Lapses, &cs.CardState,
				&lastReview, &nextReview,
				&cs.PMastery, &cs.PLearn, &cs.PForget, &cs.PSlip, &cs.PGuess,
				&cs.Theta, &cs.UpdatedAt,
			); err != nil {
				return fmt.Errorf("scan concept state row: %w", err)
			}
			if lastReview.Valid {
				cs.LastReview = &lastReview.Time
			}
			if nextReview.Valid {
				cs.NextReview = &nextReview.Time
			}
			out[cs.DomainID] = append(out[cs.DomainID], cs)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetRecentInteractionsByDomains returns, for each requested domain, the
// learner's latest current interactions (newest first, at most limit each).
func (s *Store) GetRecentInteractionsByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string, limit int) (map[string][]*models.Interaction, error) {
	if err := batchLearnerScope(scope, learnerID); err != nil {
		return nil, err
	}
	ids := uniqueDomainIDs(domainIDs)
	out := make(map[string][]*models.Interaction, len(ids))
	if len(ids) == 0 || limit <= 0 {
		return out, nil
	}
	err := s.withBatchReadScope(ctx, scope, func(ctx context.Context, txs *Store) error {
		args := append([]any{learnerID}, anySlice(ids)...)
		args = append(args, limit)
		rows, err := txs.query(ctx, `SELECT `+interactionCols+` FROM (
			SELECT interactions.*, ROW_NUMBER() OVER (PARTITION BY domain_id ORDER BY created_at DESC, id DESC) AS batch_rank
			FROM `+currentInteractionsSQL+` AS interactions
			WHERE learner_id = ? AND domain_id IN (`+inPlaceholders(len(ids))+`)
		) AS ranked WHERE batch_rank <= ? ORDER BY domain_id, created_at DESC, id DESC`, args...)
		if err != nil {
			return fmt.Errorf("get recent interactions by domains: %w", err)
		}
		defer rows.Close()
		interactions, err := scanInteractions(rows)
		if err != nil {
			return err
		}
		for _, interaction := range interactions {
			out[interaction.DomainID] = append(out[interaction.DomainID], interaction)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetInteractionsSinceByDomains returns the learner's current interactions
// created at or after since, grouped by domain and oldest first.
func (s *Store) GetInteractionsSinceByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string, since time.Time) (map[string][]*models.Interaction, error) {
	if err := batchLearnerScope(scope, learnerID); err != nil {
		return nil, err
	}
	ids := uniqueDomainIDs(domainIDs)
	out := make(map[string][]*models.Interaction, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := s.withBatchReadScope(ctx, scope, func(ctx context.Context, txs *Store) error {
		args := append([]any{learnerID}, anySlice(ids)...)
		args = append(args, since)
		rows, err := txs.query(ctx, `SELECT `+interactionCols+` FROM `+currentInteractionsSQL+` AS interactions
			WHERE learner_id = ? AND domain_id IN (`+inPlaceholders(len(ids))+`) AND created_at >= ?
			ORDER BY created_at ASC`, args...)
		if err != nil {
			return fmt.Errorf("get interactions since by domains: %w", err)
		}
		defer rows.Close()
		interactions, err := scanInteractions(rows)
		if err != nil {
			return err
		}
		for _, interaction := range interactions {
			out[interaction.DomainID] = append(out[interaction.DomainID], interaction)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// InsertDomainConceptStates initializes fresh concept states for a personal
// domain in one statement, skipping concepts that already have a state. The
// learning scope of every concept is resolved with one mapping query; a
// formation-backed domain, or a concept without a mapping, falls back to the
// per-concept path that knows how to resolve or quarantine it.
func (s *Store) InsertDomainConceptStates(ctx context.Context, scope models.TenantScope, learnerID, domainID string, concepts []string) error {
	if err := batchLearnerScope(scope, learnerID); err != nil {
		return err
	}
	if domainID == "" || len(concepts) == 0 {
		return nil
	}
	if _, formation, err := s.formationLearningScope(ctx, learnerID, domainID, ""); err != nil || formation {
		if err != nil {
			return err
		}
		return s.insertConceptStatesOneByOne(ctx, learnerID, domainID, concepts)
	}
	rows, err := s.query(ctx, `SELECT concept.concept_label, mapping.tenant_id, mapping.enrollment_id, concept.concept_id
		FROM legacy_domain_enrollments mapping
		JOIN legacy_concept_mappings concept
		  ON concept.tenant_id = mapping.tenant_id
		 AND concept.enrollment_id = mapping.enrollment_id
		 AND concept.domain_id = mapping.domain_id
		WHERE mapping.learner_id = ? AND mapping.domain_id = ?`, learnerID, domainID)
	if err != nil {
		return fmt.Errorf("resolve domain learning scopes: %w", err)
	}
	byLabel := map[string]learningScopeIDs{}
	for rows.Next() {
		var label string
		var ids learningScopeIDs
		if err := rows.Scan(&label, &ids.TenantID, &ids.EnrollmentID, &ids.FormationConceptID); err != nil {
			rows.Close()
			return fmt.Errorf("scan domain learning scope: %w", err)
		}
		byLabel[label] = ids
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate domain learning scopes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var fallback []string
	type pending struct {
		state *models.ConceptState
		ids   learningScopeIDs
	}
	var batch []pending
	seen := map[string]bool{}
	for _, concept := range concepts {
		if concept == "" || seen[concept] {
			continue
		}
		seen[concept] = true
		ids, ok := byLabel[concept]
		if !ok {
			ids, ok = byLabel[""]
		}
		if !ok {
			fallback = append(fallback, concept)
			continue
		}
		state := models.NewConceptStateInDomain(learnerID, domainID, concept)
		state.UpdatedAt = time.Now().UTC()
		batch = append(batch, pending{state: state, ids: ids})
	}
	const columns = 22
	const chunk = 40 // 880 bind parameters, below SQLite's default limit
	for start := 0; start < len(batch); start += chunk {
		end := min(start+chunk, len(batch))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*columns)
		for _, item := range batch[start:end] {
			cs, ids := item.state, item.ids
			values = append(values, "("+inPlaceholders(columns)+")")
			args = append(args, cs.LearnerID, cs.DomainID, cs.Concept, ids.TenantID, ids.EnrollmentID, ids.FormationConceptID,
				cs.Stability, cs.Difficulty, cs.ElapsedDays, cs.ScheduledDays,
				cs.Reps, cs.Lapses, cs.CardState, cs.LastReview, cs.NextReview,
				cs.PMastery, cs.PLearn, cs.PForget, cs.PSlip, cs.PGuess,
				cs.Theta, cs.UpdatedAt)
		}
		if _, err := s.exec(ctx, `INSERT INTO concept_states
			    (learner_id, domain_id, concept, tenant_id, enrollment_id, formation_concept_id,
			     stability, difficulty, elapsed_days, scheduled_days,
			     reps, lapses, card_state, last_review, next_review, p_mastery, p_learn, p_forget,
			     p_slip, p_guess, theta, updated_at)
			 VALUES `+strings.Join(values, ", ")+`
			 ON CONFLICT (learner_id, domain_id, concept) DO NOTHING`, args...); err != nil {
			return fmt.Errorf("insert domain concept states: %w", err)
		}
	}
	return s.insertConceptStatesOneByOne(ctx, learnerID, domainID, fallback)
}

func (s *Store) insertConceptStatesOneByOne(ctx context.Context, learnerID, domainID string, concepts []string) error {
	for _, concept := range concepts {
		if err := s.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(learnerID, domainID, concept)); err != nil {
			return fmt.Errorf("initialize concept %q: %w", concept, err)
		}
	}
	return nil
}

func anySlice(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// GetTransferScoresByDomains returns the learner's current transfer evidence
// grouped by domain then concept, newest first within each concept, for every
// requested domain. It answers the multi-domain progress view with one
// statement; callers keep only the concepts of each domain graph.
func (s *Store) GetTransferScoresByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string) (map[string]map[string][]*models.TransferRecord, error) {
	if err := batchLearnerScope(scope, learnerID); err != nil {
		return nil, err
	}
	ids := uniqueDomainIDs(domainIDs)
	out := make(map[string]map[string][]*models.TransferRecord, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := s.withBatchReadScope(ctx, scope, func(ctx context.Context, txs *Store) error {
		args := append([]any{learnerID}, anySlice(ids)...)
		rows, err := txs.query(ctx, `SELECT id, learner_id, domain_id, assessment_attempt_id,
			       concept_id, context_type, score, session_id, created_at
			FROM `+currentTransfersSQL+` AS transfer_records
			WHERE learner_id = ? AND domain_id IN (`+inPlaceholders(len(ids))+`)
			ORDER BY domain_id, concept_id, created_at DESC`, args...)
		if err != nil {
			return fmt.Errorf("get transfer scores by domains: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			r := &models.TransferRecord{}
			var attemptID sql.NullString
			if err := rows.Scan(
				&r.ID, &r.LearnerID, &r.DomainID, &attemptID, &r.ConceptID,
				&r.ContextType, &r.Score, &r.SessionID, &r.CreatedAt,
			); err != nil {
				return fmt.Errorf("scan transfer evidence by domains: %w", err)
			}
			r.AssessmentAttemptID = attemptID.String
			byConcept := out[r.DomainID]
			if byConcept == nil {
				byConcept = map[string][]*models.TransferRecord{}
				out[r.DomainID] = byConcept
			}
			byConcept[r.ConceptID] = append(byConcept[r.ConceptID], r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate transfer evidence by domains: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetEvaluatedAssessmentAttemptsByDomains returns at most limitPerConcept
// evaluated envelopes per (domain, concept) of the learner, grouped by domain
// then concept, newest first. ROW_NUMBER keeps the per-concept cap of
// GetEvaluatedAssessmentAttemptsBatchInDomain in a single statement.
func (s *Store) GetEvaluatedAssessmentAttemptsByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string, limitPerConcept int) (map[string]map[string][]*models.AssessmentAttempt, error) {
	if err := batchLearnerScope(scope, learnerID); err != nil {
		return nil, err
	}
	if limitPerConcept <= 0 {
		limitPerConcept = 50
	}
	ids := uniqueDomainIDs(domainIDs)
	out := make(map[string]map[string][]*models.AssessmentAttempt, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := s.withBatchReadScope(ctx, scope, func(ctx context.Context, txs *Store) error {
		args := append([]any{learnerID}, anySlice(ids)...)
		args = append(args, limitPerConcept)
		rows, err := txs.query(ctx, `WITH ranked_evidence AS (
			SELECT `+txs.assessmentEvidenceColumns()+`,
			       ROW_NUMBER() OVER (
			           PARTITION BY a.domain_id, a.concept_id ORDER BY COALESCE(j.created_at, a.evaluated_at) DESC
			       ) AS evidence_rank
			FROM `+assessmentEvidenceFrom+`
			WHERE a.learner_id = ? AND a.domain_id IN (`+inPlaceholders(len(ids))+`)
			  AND a.status = 'evaluated' AND a.curriculum_invalidated_version = 0
			  AND a.submitted_at IS NOT NULL AND a.evaluated_at IS NOT NULL
			)
			SELECT `+assessmentColumns+`
			FROM ranked_evidence
			WHERE evidence_rank <= ?
			ORDER BY domain_id, concept_id, evaluated_at DESC`, args...)
		if err != nil {
			return fmt.Errorf("get evaluated assessment attempts by domains: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAssessmentValues(rows)
			if err != nil {
				return err
			}
			byConcept := out[a.DomainID]
			if byConcept == nil {
				byConcept = map[string][]*models.AssessmentAttempt{}
				out[a.DomainID] = byConcept
			}
			byConcept[a.ConceptID] = append(byConcept[a.ConceptID], a)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate evaluated assessment attempts by domains: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
