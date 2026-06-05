// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// ConceptMasteryDelta estimates per-concept mastery trajectory over a window.
// For each domain concept with interactions, returns (now, approximate past, delta),
// sorted by descending delta. Only concepts with positive delta are returned.
//
// Note: past mastery is approximated from success ratio on interactions before `since`.
// Exact historical BKT snapshots are not persisted — this is good enough for a
// learner-facing trajectory narrative.
func (s *Store) ConceptMasteryDelta(learnerID string, domainConcepts []string, since time.Time, limit int) ([]models.ConceptDelta, error) {
	if limit <= 0 {
		limit = 3
	}

	// Current mastery per concept (BKT p_mastery).
	states, err := s.GetConceptStatesByLearner(learnerID)
	if err != nil {
		return nil, fmt.Errorf("mastery delta: get states: %w", err)
	}
	stateByConcept := make(map[string]*models.ConceptState)
	for _, cs := range states {
		stateByConcept[cs.Concept] = cs
	}

	// Only concepts we actually have a current state for can yield a
	// delta — restrict the (formerly per-concept) history aggregation to
	// those, in one GROUP BY query keyed by concept.
	relevant := make([]string, 0, len(domainConcepts))
	for _, concept := range domainConcepts {
		if stateByConcept[concept] != nil {
			relevant = append(relevant, concept)
		}
	}

	// Counts of total/successful interactions before `since` per concept
	// — approximates past mastery. Concepts with no prior interactions are
	// simply absent from the map (handled as the totalBefore == 0 case).
	type beforeCounts struct{ total, success int }
	before := make(map[string]beforeCounts, len(relevant))
	if len(relevant) > 0 {
		placeholders := make([]string, 0, len(relevant))
		args := make([]any, 0, len(relevant)+2)
		args = append(args, learnerID)
		for _, c := range relevant {
			placeholders = append(placeholders, "?")
			args = append(args, c)
		}
		args = append(args, since.UTC())
		rows, err := s.db.Query(
			`SELECT concept, COUNT(*), COALESCE(SUM(success), 0)
			 FROM interactions
			 WHERE learner_id = ? AND concept IN (`+strings.Join(placeholders, ",")+`)
			   AND created_at < ?
			 GROUP BY concept`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf("mastery delta: history counts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var concept string
			var bc beforeCounts
			if err := rows.Scan(&concept, &bc.total, &bc.success); err != nil {
				return nil, fmt.Errorf("mastery delta: scan history: %w", err)
			}
			before[concept] = bc
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("mastery delta: history rows: %w", err)
		}
	}

	var deltas []models.ConceptDelta
	for _, concept := range relevant {
		cs := stateByConcept[concept]
		bc := before[concept]

		var masteryWas float64
		if bc.total == 0 {
			masteryWas = 0.1 // initial BKT prior
		} else {
			masteryWas = float64(bc.success) / float64(bc.total)
		}

		delta := cs.PMastery - masteryWas
		if delta <= 0.05 {
			continue // not enough movement to narrate
		}

		deltas = append(deltas, models.ConceptDelta{
			Concept:    concept,
			MasteryNow: cs.PMastery,
			MasteryWas: masteryWas,
			Delta:      delta,
		})
	}

	sort.Slice(deltas, func(i, j int) bool { return deltas[i].Delta > deltas[j].Delta })
	if len(deltas) > limit {
		deltas = deltas[:limit]
	}
	return deltas, nil
}

// MilestonesInWindow returns concepts newly mastered in the window [since, now].
// "Newly mastered" = current PMastery >= MasteryBKT() AND the most recent
// interaction on that concept is after `since` (approximation).
func (s *Store) MilestonesInWindow(learnerID string, domainConcepts []string, since time.Time) ([]string, error) {
	states, err := s.GetConceptStatesByLearner(learnerID)
	if err != nil {
		return nil, fmt.Errorf("milestones: get states: %w", err)
	}
	domainSet := make(map[string]bool, len(domainConcepts))
	for _, c := range domainConcepts {
		domainSet[c] = true
	}

	// Candidate concepts: in-domain AND currently above the mastery
	// threshold. The "recently active" check below is then resolved for
	// all candidates in a single set-based query instead of one COUNT(*)
	// per concept.
	var candidates []string
	for _, cs := range states {
		if !domainSet[cs.Concept] {
			continue
		}
		if cs.PMastery < algorithms.MasteryBKT() {
			continue
		}
		candidates = append(candidates, cs.Concept)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Concepts with at least one successful interaction since `since`.
	placeholders := make([]string, 0, len(candidates))
	args := make([]any, 0, len(candidates)+2)
	args = append(args, learnerID)
	for _, c := range candidates {
		placeholders = append(placeholders, "?")
		args = append(args, c)
	}
	args = append(args, since.UTC())
	rows, err := s.db.Query(
		`SELECT DISTINCT concept FROM interactions
		 WHERE learner_id = ? AND concept IN (`+strings.Join(placeholders, ",")+`)
		   AND success = 1 AND created_at >= ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("milestones: active concepts: %w", err)
	}
	defer rows.Close()

	var milestones []string
	for rows.Next() {
		var concept string
		if err := rows.Scan(&concept); err != nil {
			return nil, fmt.Errorf("milestones: scan concept: %w", err)
		}
		milestones = append(milestones, concept)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("milestones: rows: %w", err)
	}
	sort.Strings(milestones)
	return milestones, nil
}

// CountInteractionsByConcept returns the total number of interactions on a given concept.
func (s *Store) CountInteractionsByConcept(learnerID, concept string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM interactions WHERE learner_id = ? AND concept = ?`,
		learnerID, concept,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count interactions by concept: %w", err)
	}
	return count, nil
}

// CountSessionsOnConcept approximates the number of distinct study sessions the
// learner has had on a concept. Uses distinct calendar dates with at least one
// interaction on that concept (rough proxy — good enough for Hidi-Renninger phase
// inference). The substr(created_at, 1, 10) works around modernc-sqlite's ISO 8601
// serialization (the built-in DATE() doesn't parse the 'T' separator).
func (s *Store) CountSessionsOnConcept(learnerID, concept string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT substr(created_at, 1, 10)) FROM interactions
		 WHERE learner_id = ? AND concept = ?`,
		learnerID, concept,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count sessions on concept: %w", err)
	}
	return count, nil
}

// CountLearnerSessionStreak returns the consecutive-day streak for a learner,
// computed via substr-based date extraction (works with modernc's ISO serialization).
func (s *Store) CountLearnerSessionStreak(learnerID string) (int, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT substr(created_at, 1, 10) AS d FROM interactions
		 WHERE learner_id = ? ORDER BY d DESC`,
		learnerID,
	)
	if err != nil {
		return 0, fmt.Errorf("count session streak: %w", err)
	}
	defer rows.Close()

	streak := 0
	expected := time.Now().UTC().Truncate(24 * time.Hour)
	for rows.Next() {
		var dateStr string
		if err := rows.Scan(&dateStr); err != nil {
			return streak, nil
		}
		d, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			return streak, nil
		}
		if streak == 0 {
			diff := expected.Sub(d).Hours() / 24
			if diff > 1 {
				return 0, nil
			}
			streak = 1
			expected = d.AddDate(0, 0, -1)
			continue
		}
		if d.Equal(expected) {
			streak++
			expected = d.AddDate(0, 0, -1)
		} else {
			break
		}
	}
	return streak, nil
}

// SelfInitiatedRatio returns the ratio of self_initiated interactions on a concept
// (0 if no interactions).
func (s *Store) SelfInitiatedRatio(learnerID, concept string) (float64, error) {
	var total, selfInit int
	err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(self_initiated), 0)
		 FROM interactions WHERE learner_id = ? AND concept = ?`,
		learnerID, concept,
	).Scan(&total, &selfInit)
	if err != nil {
		return 0, fmt.Errorf("self-initiated ratio: %w", err)
	}
	if total == 0 {
		return 0, nil
	}
	return float64(selfInit) / float64(total), nil
}

// LastFailureOnConcept returns the most recent failed interaction on a concept,
// or nil if none exists within `window`.
func (s *Store) LastFailureOnConcept(learnerID, concept string, window time.Duration) (*models.Interaction, error) {
	cutoff := time.Now().UTC().Add(-window)
	rows, err := s.db.Query(
		`SELECT `+interactionCols+` FROM interactions
		 WHERE learner_id = ? AND concept = ? AND success = 0 AND created_at >= ?
		 ORDER BY created_at DESC LIMIT 1`,
		learnerID, concept, cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("last failure on concept: %w", err)
	}
	defer rows.Close()
	items, err := scanInteractions(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items[0], nil
}
