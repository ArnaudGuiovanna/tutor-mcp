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
)

// parseTimeFlex attempts several common SQLite/Go time formats.
func parseTimeFlex(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 +0000 UTC",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", s)
}

// ─── Misconception Groups ──────────────────────────────────────────────────

// MisconceptionResolutionWindow is the number of most-recent interactions
// inspected to determine whether a misconception is "active" or
// "resolved":
//
//   - any of the last N interactions on the concept carries the
//     misconception_type → status is "active"
//   - none does → status is "resolved"
//
// Single source of truth for the resolution latency. Read by
// computeMisconceptionStatus (below) and referenced semantically by
// [3] Gate Controller, which consumes the derived status (not the raw
// count) via GetActiveMisconceptions. Changing this constant changes
// the resolution latency uniformly across the system.
//
// Documented in docs/regulation-design/03-gate-controller.md OQ-3.3.
const MisconceptionResolutionWindow = 3

type MisconceptionGroup struct {
	Concept           string    `json:"concept"`
	MisconceptionType string    `json:"misconception_type"`
	Count             int       `json:"count"`
	FirstSeen         time.Time `json:"first_seen"`
	LastSeen          time.Time `json:"last_seen"`
	LastErrorDetail   string    `json:"last_error_detail"`
	Status            string    `json:"status"`
}

// GetMisconceptionGroups returns all misconception groups for a learner,
// optionally filtered by concept. Groups are ordered by count descending.
func (s *Store) GetMisconceptionGroups(ctx context.Context, learnerID string, conceptFilter map[string]bool) ([]MisconceptionGroup, error) {
	query := `SELECT concept, misconception_type, COUNT(*) AS cnt, MIN(created_at), MAX(created_at)
		 FROM interactions
		 WHERE learner_id = ? AND misconception_type IS NOT NULL`
	args := []any{learnerID}
	if len(conceptFilter) > 0 {
		placeholders := make([]string, 0, len(conceptFilter))
		for concept := range conceptFilter {
			placeholders = append(placeholders, "?")
			args = append(args, concept)
		}
		query += ` AND concept IN (` + strings.Join(placeholders, ",") + `)`
	}
	query += ` GROUP BY concept, misconception_type ORDER BY cnt DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get misconception groups: %w", err)
	}
	defer rows.Close()

	var groups []MisconceptionGroup
	for rows.Next() {
		var g MisconceptionGroup
		var firstSeen, lastSeen string
		if err := rows.Scan(&g.Concept, &g.MisconceptionType, &g.Count, &firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan misconception group: %w", err)
		}
		g.FirstSeen, _ = parseTimeFlex(firstSeen)
		g.LastSeen, _ = parseTimeFlex(lastSeen)

		// Go-side concept filtering
		if conceptFilter != nil && !conceptFilter[g.Concept] {
			continue
		}

		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Enrich the groups in two set-based passes instead of firing a
	// per-group follow-up query (the former N+1). Both helpers take the
	// concept set scanned above and return a map keyed by (concept,
	// misconception_type), so the loop below is pure in-memory lookup.
	concepts := make([]string, 0, len(groups))
	for _, g := range groups {
		concepts = append(concepts, g.Concept)
	}
	details, err := s.lastMisconceptionDetails(ctx, learnerID, concepts)
	if err != nil {
		return nil, fmt.Errorf("get last misconception details: %w", err)
	}
	statuses, err := s.misconceptionStatuses(ctx, learnerID, concepts)
	if err != nil {
		return nil, fmt.Errorf("get misconception statuses: %w", err)
	}
	for i := range groups {
		key := miscKey(groups[i].Concept, groups[i].MisconceptionType)
		groups[i].LastErrorDetail = details[key]
		if statuses[key] {
			groups[i].Status = "active"
		} else {
			groups[i].Status = "resolved"
		}
	}
	return groups, nil
}

// miscKey joins a (concept, misconception_type) pair into a single map
// key. The NUL separator can't appear in either component, so distinct
// pairs never collide.
func miscKey(concept, misconceptionType string) string {
	return concept + "\x00" + misconceptionType
}

// lastMisconceptionDetails returns, for the given concepts, the most
// recent non-empty misconception_detail per (concept, misconception_type)
// in a single query (set-based replacement for the per-group
// getLastMisconceptionDetail). The window function picks the latest row
// per partition; empty/NULL details map to "".
func (s *Store) lastMisconceptionDetails(ctx context.Context, learnerID string, concepts []string) (map[string]string, error) {
	out := make(map[string]string)
	if len(concepts) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(concepts))
	args := make([]any, 0, len(concepts)+1)
	args = append(args, learnerID)
	for _, c := range concepts {
		placeholders = append(placeholders, "?")
		args = append(args, c)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT concept, misconception_type, misconception_detail
		 FROM (
		    SELECT concept, misconception_type, misconception_detail,
		           ROW_NUMBER() OVER (PARTITION BY concept, misconception_type
		                              ORDER BY created_at DESC, id DESC) AS rn
		    FROM interactions
		    WHERE learner_id = ? AND misconception_type IS NOT NULL
		      AND concept IN (`+strings.Join(placeholders, ",")+`)
		 )
		 WHERE rn = 1`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("query last misconception details: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var concept, misconceptionType string
		var detail sql.NullString
		if err := rows.Scan(&concept, &misconceptionType, &detail); err != nil {
			return nil, fmt.Errorf("scan last misconception detail: %w", err)
		}
		out[miscKey(concept, misconceptionType)] = detail.String
	}
	return out, rows.Err()
}

// misconceptionStatuses returns a set of (concept, misconception_type)
// keys that are "active" — i.e. the misconception_type appears among the
// MisconceptionResolutionWindow most recent interactions on that concept.
// Single-query replacement for the per-group computeMisconceptionStatus;
// the window function ranks interactions per concept and we keep the
// top-N, then flag every misconception_type still present in that window.
func (s *Store) misconceptionStatuses(ctx context.Context, learnerID string, concepts []string) (map[string]bool, error) {
	out := make(map[string]bool)
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
		           ROW_NUMBER() OVER (PARTITION BY concept
		                              ORDER BY created_at DESC, id DESC) AS rn
		    FROM interactions
		    WHERE learner_id = ? AND concept IN (`+strings.Join(placeholders, ",")+`)
		 )
		 WHERE rn <= ? AND misconception_type IS NOT NULL`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("query misconception statuses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var concept, misconceptionType string
		if err := rows.Scan(&concept, &misconceptionType); err != nil {
			return nil, fmt.Errorf("scan misconception status: %w", err)
		}
		out[miscKey(concept, misconceptionType)] = true
	}
	return out, rows.Err()
}

// GetDistinctMisconceptionTypes returns all distinct misconception types
// recorded for a learner on a given concept, in alphabetical order.
func (s *Store) GetDistinctMisconceptionTypes(ctx context.Context, learnerID, concept string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT misconception_type FROM interactions
		 WHERE learner_id = ? AND concept = ? AND misconception_type IS NOT NULL
		 ORDER BY misconception_type`,
		learnerID, concept,
	)
	if err != nil {
		return nil, fmt.Errorf("get distinct misconception types: %w", err)
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan misconception type: %w", err)
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

// GetActiveMisconceptions returns only the "active" misconception groups
// for a learner on a specific concept.
func (s *Store) GetActiveMisconceptions(ctx context.Context, learnerID, concept string) ([]MisconceptionGroup, error) {
	filter := map[string]bool{concept: true}
	groups, err := s.GetMisconceptionGroups(ctx, learnerID, filter)
	if err != nil {
		return nil, fmt.Errorf("get active misconceptions: %w", err)
	}

	var active []MisconceptionGroup
	for _, g := range groups {
		if g.Status == "active" {
			active = append(active, g)
		}
	}
	return active, nil
}
