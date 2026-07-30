// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"tutor-mcp/models"
)

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

// GetMisconceptionGroups returns all misconception groups for a learner,
// optionally filtered by concept. Groups are ordered by count descending.
func (s *Store) GetMisconceptionGroups(ctx context.Context, learnerID string, conceptFilter map[string]bool) ([]models.MisconceptionGroup, error) {
	return s.getMisconceptionGroups(ctx, learnerID, "", conceptFilter, false)
}

func (s *Store) GetMisconceptionGroupsInDomain(ctx context.Context, learnerID, domainID string, conceptFilter map[string]bool) ([]models.MisconceptionGroup, error) {
	return s.getMisconceptionGroups(ctx, learnerID, domainID, conceptFilter, true)
}

func (s *Store) getMisconceptionGroups(ctx context.Context, learnerID, domainID string, conceptFilter map[string]bool, exactDomain bool) ([]models.MisconceptionGroup, error) {
	query := `SELECT concept, misconception_type, COUNT(*) AS cnt, MIN(created_at), MAX(created_at)
		 FROM interactions
		 WHERE learner_id = ? AND misconception_type IS NOT NULL`
	args := []any{learnerID}
	if exactDomain {
		query += ` AND domain_id = ?`
		args = append(args, domainID)
	}
	if len(conceptFilter) > 0 {
		placeholders := make([]string, 0, len(conceptFilter))
		for concept := range conceptFilter {
			placeholders = append(placeholders, "?")
			args = append(args, concept)
		}
		query += ` AND concept IN (` + strings.Join(placeholders, ",") + `)`
	}
	query += ` GROUP BY concept, misconception_type ORDER BY cnt DESC`

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get misconception groups: %w", err)
	}
	defer rows.Close()

	var groups []models.MisconceptionGroup
	for rows.Next() {
		var g models.MisconceptionGroup
		var firstSeen, lastSeen flexTime
		if err := rows.Scan(&g.Concept, &g.MisconceptionType, &g.Count, &firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan misconception group: %w", err)
		}
		g.FirstSeen = firstSeen.Time
		g.LastSeen = lastSeen.Time

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
	details, err := s.lastMisconceptionDetails(ctx, learnerID, domainID, concepts, exactDomain)
	if err != nil {
		return nil, fmt.Errorf("get last misconception details: %w", err)
	}
	statuses, err := s.misconceptionStatuses(ctx, learnerID, domainID, concepts, exactDomain)
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
func (s *Store) lastMisconceptionDetails(ctx context.Context, learnerID, domainID string, concepts []string, exactDomain bool) (map[string]string, error) {
	out := make(map[string]string)
	if len(concepts) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(concepts))
	args := make([]any, 0, len(concepts)+2)
	args = append(args, learnerID)
	domainClause := ""
	if exactDomain {
		domainClause = " AND domain_id = ?"
		args = append(args, domainID)
	}
	for _, c := range concepts {
		placeholders = append(placeholders, "?")
		args = append(args, c)
	}
	rows, err := s.query(ctx,
		`SELECT concept, misconception_type, misconception_detail
		 FROM (
		    SELECT concept, misconception_type, misconception_detail,
		           ROW_NUMBER() OVER (PARTITION BY concept, misconception_type
		                              ORDER BY created_at DESC, id DESC) AS rn
		    FROM interactions
		    WHERE learner_id = ?`+domainClause+` AND misconception_type IS NOT NULL
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
func (s *Store) misconceptionStatuses(ctx context.Context, learnerID, domainID string, concepts []string, exactDomain bool) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(concepts) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(concepts))
	args := make([]any, 0, len(concepts)+3)
	args = append(args, learnerID)
	domainClause := ""
	if exactDomain {
		domainClause = " AND domain_id = ?"
		args = append(args, domainID)
	}
	for _, c := range concepts {
		placeholders = append(placeholders, "?")
		args = append(args, c)
	}
	args = append(args, MisconceptionResolutionWindow)
	rows, err := s.query(ctx,
		`SELECT concept, misconception_type
		 FROM (
		    SELECT concept, misconception_type,
		           ROW_NUMBER() OVER (PARTITION BY concept
		                              ORDER BY created_at DESC, id DESC) AS rn
		    FROM interactions
		    WHERE learner_id = ?`+domainClause+` AND concept IN (`+strings.Join(placeholders, ",")+`)
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
	return s.getDistinctMisconceptionTypes(ctx, learnerID, "", concept, false)
}

func (s *Store) GetDistinctMisconceptionTypesInDomain(ctx context.Context, learnerID, domainID, concept string) ([]string, error) {
	return s.getDistinctMisconceptionTypes(ctx, learnerID, domainID, concept, true)
}

func (s *Store) getDistinctMisconceptionTypes(ctx context.Context, learnerID, domainID, concept string, exactDomain bool) ([]string, error) {
	query := `SELECT DISTINCT misconception_type FROM interactions
		 WHERE learner_id = ? AND concept = ? AND misconception_type IS NOT NULL`
	args := []any{learnerID, concept}
	if exactDomain {
		query += ` AND domain_id = ?`
		args = append(args, domainID)
	}
	query += ` ORDER BY misconception_type`
	rows, err := s.query(ctx,
		query,
		args...,
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
func (s *Store) GetActiveMisconceptions(ctx context.Context, learnerID, concept string) ([]models.MisconceptionGroup, error) {
	filter := map[string]bool{concept: true}
	groups, err := s.GetMisconceptionGroups(ctx, learnerID, filter)
	if err != nil {
		return nil, fmt.Errorf("get active misconceptions: %w", err)
	}

	var active []models.MisconceptionGroup
	for _, g := range groups {
		if g.Status == "active" {
			active = append(active, g)
		}
	}
	return active, nil
}

func (s *Store) GetActiveMisconceptionsInDomain(ctx context.Context, learnerID, domainID, concept string) ([]models.MisconceptionGroup, error) {
	filter := map[string]bool{concept: true}
	groups, err := s.GetMisconceptionGroupsInDomain(ctx, learnerID, domainID, filter)
	if err != nil {
		return nil, fmt.Errorf("get active misconceptions in domain: %w", err)
	}

	var active []models.MisconceptionGroup
	for _, group := range groups {
		if group.Status == "active" {
			active = append(active, group)
		}
	}
	return active, nil
}
