// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
)

// UpdateDomainPhase persists a phase transition for a domain. The
// caller pre-computes phaseEntryEntropy when the new phase is
// DIAGNOSTIC (snapshot of the current mean binary entropy) so the
// FSM can later compare it against the running entropy. For non-
// DIAGNOSTIC targets, pass 0 — the column will be set to NULL via
// the sql.NullFloat64 wrapper.
//
// Used by [2] PhaseController. Idempotent at the row level: writing
// the same phase twice is a no-op semantically (timestamp updates).
func (s *Store) UpdateDomainPhase(ctx context.Context, domainID string, phase models.Phase, phaseEntryEntropy float64, now time.Time) error {
	var entropyArg any
	if phase == models.PhaseDiagnostic {
		entropyArg = phaseEntryEntropy
	} else {
		// Reset entropy snapshot when leaving DIAGNOSTIC — it would
		// be stale otherwise.
		entropyArg = nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE domains
		 SET phase = ?, phase_changed_at = ?, phase_entry_entropy = ?
		 WHERE id = ?`,
		string(phase), now, entropyArg, domainID,
	)
	if err != nil {
		return fmt.Errorf("update domain phase: %w", err)
	}
	return nil
}

// GetActiveMisconceptionsBatch returns a map[concept]bool indicating
// which concepts in `concepts` have at least one active misconception
// for the given learner. Single-query batch version of
// GetActiveMisconceptions used by [2] orchestrator pre-fetch.
//
// "Active" semantics follow MisconceptionResolutionWindow (3 most
// recent interactions, see misconceptions.go). Returns empty map on
// no matches; never nil.
func (s *Store) GetActiveMisconceptionsBatch(ctx context.Context, learnerID string, concepts []string) (map[string]bool, error) {
	out := make(map[string]bool, len(concepts))
	if len(concepts) == 0 {
		return out, nil
	}

	placeholders := make([]string, 0, len(concepts))
	args := make([]any, 0, len(concepts)+2)
	args = append(args, learnerID)
	for _, c := range concepts {
		placeholders = append(placeholders, "?")
		args = append(args, c)
	}
	args = append(args, MisconceptionResolutionWindow)

	rows, err := s.db.QueryContext(ctx,
		`SELECT concept, misconception_type
		 FROM (
		    SELECT concept, misconception_type,
		           ROW_NUMBER() OVER (PARTITION BY concept ORDER BY created_at DESC, id DESC) AS rn
		    FROM interactions
		    WHERE learner_id = ? AND concept IN (`+strings.Join(placeholders, ",")+`)
		 )
		 WHERE rn <= ? AND misconception_type IS NOT NULL`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("batch active misconceptions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var concept, misconceptionType string
		if err := rows.Scan(&concept, &misconceptionType); err != nil {
			return nil, fmt.Errorf("scan active misconception batch: %w", err)
		}
		_ = misconceptionType
		out[concept] = true
	}
	return out, rows.Err()
}

// GetFirstActiveMisconception returns the highest-count active
// misconception group on a (learner, concept) pair, or nil if none.
// Used by [2] orchestrator to pass a *MisconceptionGroup to
// SelectAction (which expects a single misconception, not a list).
func (s *Store) GetFirstActiveMisconception(ctx context.Context, learnerID, concept string) (*models.MisconceptionGroup, error) {
	groups, err := s.GetActiveMisconceptions(ctx, learnerID, concept)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	first := groups[0]
	return &first, nil
}

// GetRecentConceptsByDomain returns the concepts practised most
// recently in this domain by the learner, in descending chronological
// order, deduplicated by concept (keeping the first-seen — i.e.
// most-recent — occurrence). Used by [2] for the anti-rep input of
// [3] Gate.
//
// `limit` caps the number of *interactions* scanned (not concepts).
// A typical value is ~20 to surface the last few unique concepts even
// when one concept dominates recent traffic.
//
// The function filters by domain_id via the domain's concept set
// (we don't store domain_id on interactions; the relationship is
// derived from the concept membership). Caller passes the concept
// set; we keep it Go-side for clarity.
func (s *Store) GetRecentConceptsByDomain(ctx context.Context, learnerID string, domainConcepts []string, limit int) ([]string, error) {
	if len(domainConcepts) == 0 || limit <= 0 {
		return nil, nil
	}
	conceptSet := make(map[string]bool, len(domainConcepts))
	for _, c := range domainConcepts {
		conceptSet[c] = true
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT concept FROM interactions
		 WHERE learner_id = ?
		 ORDER BY created_at DESC
		 LIMIT ?`,
		learnerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("recent concepts query: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan concept: %w", err)
		}
		if !conceptSet[c] || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountInteractionsSince returns the count of interactions for a
// learner whose created_at is >= since. Used by [2] to count
// diagnostic items lazily (interactions since phase_changed_at).
//
// `domainConcepts` filters Go-side (interactions don't carry domain_id;
// we filter by membership). Pass nil/empty to count across all
// concepts (useful for tests).
func (s *Store) CountInteractionsSince(ctx context.Context, learnerID string, since time.Time, domainConcepts []string) (int, error) {
	if len(domainConcepts) == 0 {
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM interactions
			 WHERE learner_id = ? AND created_at >= ?`,
			learnerID, since,
		).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count interactions: %w", err)
		}
		return n, nil
	}
	conceptSet := make(map[string]bool, len(domainConcepts))
	for _, c := range domainConcepts {
		conceptSet[c] = true
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT concept FROM interactions
		 WHERE learner_id = ? AND created_at >= ?`,
		learnerID, since,
	)
	if err != nil {
		return 0, fmt.Errorf("count interactions filtered: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return 0, fmt.Errorf("scan concept: %w", err)
		}
		if conceptSet[c] {
			n++
		}
	}
	return n, rows.Err()
}

// GetActionHistoryForConcept returns the rotation/streak counts for a
// concept. The InteractionsAboveBKT field counts consecutive
// successful interactions on the concept (strict success streak from
// the most recent backwards) — a simple proxy for "stable above
// mastery" since we don't snapshot historical PMastery values. The
// proxy is sound when used after a successful BKT update push.
func (s *Store) GetActionHistoryForConcept(ctx context.Context, learnerID, concept string, recentLimit int) (models.ActionHistoryCounts, error) {
	if recentLimit <= 0 {
		recentLimit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT activity_type, success FROM interactions
		 WHERE learner_id = ? AND concept = ?
		 ORDER BY created_at DESC
		 LIMIT ?`,
		learnerID, concept, recentLimit,
	)
	if err != nil {
		return models.ActionHistoryCounts{}, fmt.Errorf("action history: %w", err)
	}
	defer rows.Close()

	var h models.ActionHistoryCounts
	streakLive := true
	for rows.Next() {
		var actType string
		var success int
		if err := rows.Scan(&actType, &success); err != nil {
			return models.ActionHistoryCounts{}, fmt.Errorf("scan history: %w", err)
		}
		switch actType {
		case string(models.ActivityMasteryChallenge):
			h.MasteryChallengeCount++
		case string(models.ActivityFeynmanPrompt):
			h.FeynmanCount++
		case string(models.ActivityTransferProbe):
			h.TransferCount++
		}
		// Streak: count consecutive successes from the head (most recent).
		if streakLive {
			if success == 1 {
				h.InteractionsAboveBKT++
			} else {
				streakLive = false
			}
		}
	}
	return h, rows.Err()
}

// Compile-time check that we keep using sql.NullTime even if all
// callers later disappear (defensive — keep imports tidy).
var _ = sql.NullTime{}
