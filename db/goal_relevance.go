// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"tutor-mcp/models"
)

// MergeDomainGoalRelevance performs an incremental upsert of a domain's
// goal-relevance vector. Behaviour (per docs/regulation-design/01-goal-decomposer.md
// §4 and OQ-1.2):
//
//   - If the domain has no prior vector, a new one is created with the
//     provided entries.
//   - If a prior vector exists, provided entries are merged into it:
//     existing concepts not present in `relevance` keep their score;
//     concepts in `relevance` overwrite their prior value.
//   - Always writes the current Domain.GraphVersion as ForGraphVersion in
//     the stored JSON so IsGoalRelevanceStale() works.
//   - goal_relevance_version is incremented atomically — distinct from
//     graph_version: it counts set calls, not graph mutations.
//
// Returns the merged vector after persistence (so the caller can compose
// the response payload — uncovered list, covered count, etc.).
func (s *Store) MergeDomainGoalRelevance(ctx context.Context, domainID string, relevance map[string]float64) (*models.GoalRelevance, error) {
	if relevance == nil {
		return nil, fmt.Errorf("relevance map is nil")
	}

	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingJSON string
	var graphVersion int
	err = tx.QueryRowContext(ctx,
		`SELECT goal_relevance_json, graph_version FROM domains WHERE id = ?`,
		domainID,
	).Scan(&existingJSON, &graphVersion)
	if err != nil {
		return nil, fmt.Errorf("read prior goal_relevance: %w", err)
	}

	merged := map[string]float64{}
	if existingJSON != "" {
		var prior models.GoalRelevance
		if err := json.Unmarshal([]byte(existingJSON), &prior); err == nil && prior.Relevance != nil {
			merged = prior.Relevance
		} else if err != nil {
			// Silent fallback on parse error: corrupt JSON is treated as
			// "no prior data" rather than blocking. Logged at WARN so
			// systematic corruption surfaces without disrupting the
			// merge — the new entries will still be persisted.
			slog.Warn("goal_relevance JSON corrupt during merge, treating as empty",
				"domain_id", domainID, "err", err)
		}
	}
	for k, v := range relevance {
		merged[k] = v
	}

	now := time.Now().UTC()
	newPayload := models.GoalRelevance{
		ForGraphVersion: graphVersion,
		Relevance:       merged,
		SetAt:           now,
	}
	newJSON, err := json.Marshal(newPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal goal_relevance: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE domains
		 SET goal_relevance_json = ?, goal_relevance_version = goal_relevance_version + 1
		 WHERE id = ?`,
		string(newJSON), domainID,
	)
	if err != nil {
		return nil, fmt.Errorf("write goal_relevance: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit goal_relevance tx: %w", err)
	}
	return &newPayload, nil
}

// GetDomainGoalRelevance returns the parsed goal-relevance payload for a
// domain. Returns (nil, nil) if no vector has been set yet — callers MUST
// treat this as "uniform fallback (1.0 everywhere)" rather than an error.
func (s *Store) GetDomainGoalRelevance(ctx context.Context, domainID string) (*models.GoalRelevance, error) {
	var raw string
	err := s.queryRow(ctx,
		`SELECT goal_relevance_json FROM domains WHERE id = ?`,
		domainID,
	).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("get goal_relevance: %w", err)
	}
	if raw == "" {
		return nil, nil
	}
	var gr models.GoalRelevance
	if err := json.Unmarshal([]byte(raw), &gr); err != nil {
		// Silent fallback per design (corrupt JSON ≡ no vector). Logged
		// at WARN so systematic corruption surfaces in logs without
		// blocking the session — the caller will use the uniform
		// fallback (1.0 everywhere).
		slog.Warn("goal_relevance JSON corrupt on read, falling back to nil",
			"domain_id", domainID, "err", err)
		return nil, nil
	}
	return &gr, nil
}
