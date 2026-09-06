// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"tutor-mcp/models"
)

// Derived relations retain base-table RLS in PostgreSQL (unlike owner-run SQL
// views). Audit, exports and retention keep using the unfiltered base tables.
const currentInteractionsSQL = `(SELECT * FROM interactions WHERE curriculum_invalidated_version = 0)`
const currentTransfersSQL = `(SELECT * FROM transfer_records WHERE curriculum_invalidated_version = 0)`

// lockCurriculumForEvidence takes the domain lock before assessment/state locks.
// Revisions take its exclusive counterpart through their graph-version CAS, so
// either an observation commits before invalidation, or it sees the new contract.
// Call inside the same transaction as the learner-model write.
func (s *Store) lockCurriculumForEvidence(ctx context.Context, learnerID, domainID, concept string) (int, error) {
	if domainID == "" {
		return 0, nil
	} // legacy unscoped internal observations
	query := `SELECT graph_version, graph_json FROM domains WHERE id = ? AND learner_id = ? AND deleted_at IS NULL`
	if s.dialect == DialectPostgres {
		query += ` FOR SHARE`
	}
	var version int
	var raw string
	if err := s.queryRow(ctx, query, domainID, learnerID).Scan(&version, &raw); err != nil {
		return 0, err
	}
	var graph models.KnowledgeSpace
	if err := json.Unmarshal([]byte(raw), &graph); err != nil {
		return 0, err
	}
	for _, key := range graph.Concepts {
		if key == concept {
			return version, nil
		}
	}
	return 0, fmt.Errorf("curriculum concept is no longer active: %q", concept)
}

func (s *Store) reconcileCurriculumLearning(ctx context.Context, learnerID string, previous, next *models.CurriculumSnapshot) error {
	current := make(map[string]models.CurriculumConcept, len(next.Concepts))
	for _, c := range next.Concepts {
		current[c.ID] = c
	}
	r := &models.CurriculumReconciliation{
		PolicyVersion:         "2026-09-curriculum-v1",
		InvalidatedConceptIDs: []string{},
	}
	// Discard copied/host-authored reconciliation claims. All effects below are
	// derived from the actual parent and the validated candidate under the CAS.
	next.Reconciliation = r
	for _, old := range previous.Concepts {
		updated, ok := current[old.ID]
		if !ok {
			return fmt.Errorf("curriculum identity %q must be retained; retire it instead", old.ID)
		}
		if old.Key != updated.Key {
			return fmt.Errorf("curriculum key cannot change")
		}
		if old.Status == updated.Status && models.SameCurriculumDefinition(old, updated) {
			continue
		}
		// A retired definition cannot contribute new evidence, but a reactivation
		// must never resurrect estimates or observations from its prior life.
		if old.Status == models.CurriculumConceptRetired && updated.Status == models.CurriculumConceptRetired {
			continue
		}
		r.InvalidatedConceptIDs = append(r.InvalidatedConceptIDs, old.ID)
		_, err := s.GetConceptStateInDomain(ctx, learnerID, next.DomainID, old.Key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := s.UpsertConceptState(ctx, models.NewConceptStateInDomain(learnerID, next.DomainID, old.Key)); err != nil {
				return err
			}
		}
		for _, target := range []struct{ table, concept string }{
			{"interactions", "concept"}, {"assessment_attempts", "concept_id"}, {"transfer_records", "concept_id"},
		} {
			// Identifiers are a closed internal list, never generated/user input.
			if _, err := s.exec(ctx, `UPDATE `+target.table+` SET curriculum_invalidated_version = ?
			 WHERE learner_id = ? AND domain_id = ? AND `+target.concept+` = ? AND curriculum_invalidated_version = 0`,
				next.Version, learnerID, next.DomainID, old.Key); err != nil {
				return fmt.Errorf("invalidate %s: %w", target.table, err)
			}
		}
	}
	sort.Strings(r.InvalidatedConceptIDs)
	return nil
}
