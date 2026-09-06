// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	storeport "tutor-mcp/store"
)

// GetLearnerContextOverview loads the session-opening projection in exactly
// three queries, independently of learner history cardinality. In particular,
// it never selects password_hash, email, webhook_url, or full interaction rows.
func (s *Store) GetLearnerContextOverview(ctx context.Context, learnerID string, now time.Time) (*storeport.LearnerContextOverview, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	overview, err := s.getLearnerContextIdentityAndDomains(ctx, learnerID)
	if err != nil {
		return nil, err
	}
	states, err := s.getLearnerContextConceptStates(ctx, learnerID)
	if err != nil {
		return nil, err
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	interactions, err := s.getLearnerContextTodayInteractions(ctx, learnerID, today)
	if err != nil {
		return nil, err
	}
	overview.ConceptStates = states
	overview.TodayInteractions = interactions
	return overview, nil
}

func (s *Store) getLearnerContextIdentityAndDomains(ctx context.Context, learnerID string) (*storeport.LearnerContextOverview, error) {
	rows, err := s.query(ctx, `
		SELECT l.id, l.objective, l.profile_json, l.created_at, l.last_active,
		       d.id, d.name, d.graph_json, d.archived, d.high_stakes,
		       d.priority_rank, d.created_at
		FROM learners l
		LEFT JOIN domains d
		  ON d.learner_id = l.id AND d.deleted_at IS NULL
		WHERE l.id = ?
		ORDER BY d.created_at DESC`, learnerID)
	if err != nil {
		return nil, fmt.Errorf("get learner context identity and domains: %w", err)
	}
	defer rows.Close()

	overview := &storeport.LearnerContextOverview{}
	found := false
	for rows.Next() {
		var profileJSON sql.NullString
		var lastActive sql.NullTime
		var domainID, domainName, graphJSON sql.NullString
		var archived, highStakes, priorityRank sql.NullInt64
		var domainCreatedAt sql.NullTime
		if err := rows.Scan(
			&overview.Learner.ID,
			&overview.Learner.Objective,
			&profileJSON,
			&overview.Learner.CreatedAt,
			&lastActive,
			&domainID,
			&domainName,
			&graphJSON,
			&archived,
			&highStakes,
			&priorityRank,
			&domainCreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan learner context identity and domains: %w", err)
		}
		found = true
		if profileJSON.Valid {
			overview.Learner.ProfileJSON = profileJSON.String
		} else {
			overview.Learner.ProfileJSON = "{}"
		}
		if lastActive.Valid {
			overview.Learner.LastActive = lastActive.Time
		}
		if !domainID.Valid {
			continue
		}

		domain := storeport.LearnerContextDomain{
			ID:         domainID.String,
			Name:       domainName.String,
			Archived:   archived.Valid && archived.Int64 != 0,
			HighStakes: highStakes.Valid && highStakes.Int64 != 0,
		}
		if priorityRank.Valid {
			rank := int(priorityRank.Int64)
			domain.PriorityRank = &rank
		}
		if domainCreatedAt.Valid {
			domain.CreatedAt = domainCreatedAt.Time
		}
		if !graphJSON.Valid || graphJSON.String == "" {
			return nil, fmt.Errorf("scan learner context domain %q: graph is empty", domain.ID)
		}
		if err := json.Unmarshal([]byte(graphJSON.String), &domain.Graph); err != nil {
			return nil, fmt.Errorf("scan learner context domain %q graph: %w", domain.ID, err)
		}
		overview.Domains = append(overview.Domains, domain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate learner context identity and domains: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("learner not found: %w", storeport.WrapNotFound(sql.ErrNoRows))
	}
	return overview, nil
}

func (s *Store) getLearnerContextConceptStates(ctx context.Context, learnerID string) ([]storeport.LearnerContextConceptState, error) {
	rows, err := s.query(ctx, `
		SELECT domain_id, concept, stability, card_state, last_review, p_mastery
		FROM concept_states
		WHERE learner_id = ?`, learnerID)
	if err != nil {
		return nil, fmt.Errorf("get learner context concept states: %w", err)
	}
	defer rows.Close()

	var states []storeport.LearnerContextConceptState
	for rows.Next() {
		var state storeport.LearnerContextConceptState
		var domainID sql.NullString
		var lastReview sql.NullTime
		if err := rows.Scan(
			&domainID,
			&state.Concept,
			&state.Stability,
			&state.CardState,
			&lastReview,
			&state.PMastery,
		); err != nil {
			return nil, fmt.Errorf("scan learner context concept state: %w", err)
		}
		if domainID.Valid {
			state.DomainID = domainID.String
		}
		if lastReview.Valid {
			reviewedAt := lastReview.Time
			state.LastReview = &reviewedAt
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate learner context concept states: %w", err)
	}
	return states, nil
}

func (s *Store) getLearnerContextTodayInteractions(ctx context.Context, learnerID string, today time.Time) ([]storeport.LearnerContextInteractionCount, error) {
	rows, err := s.query(ctx, `
		SELECT domain_id, concept, COUNT(*)
		FROM interactions
		WHERE learner_id = ? AND created_at >= ?
		GROUP BY domain_id, concept`, learnerID, today.UTC())
	if err != nil {
		return nil, fmt.Errorf("get learner context today's interactions: %w", err)
	}
	defer rows.Close()

	var counts []storeport.LearnerContextInteractionCount
	for rows.Next() {
		var count storeport.LearnerContextInteractionCount
		var domainID sql.NullString
		if err := rows.Scan(&domainID, &count.Concept, &count.Count); err != nil {
			return nil, fmt.Errorf("scan learner context today's interactions: %w", err)
		}
		if domainID.Valid {
			count.DomainID = domainID.String
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate learner context today's interactions: %w", err)
	}
	return counts, nil
}

// GetLearnerContextNarrativeSignals combines per-concept history, the
// consecutive UTC-day streak, and the five newest autonomy scores in one SQL
// result set. Window functions collapse an arbitrarily long activity history
// to a single streak row instead of returning every historical date to Go.
func (s *Store) GetLearnerContextNarrativeSignals(ctx context.Context, learnerID, domainID string, concepts []string, now time.Time) (*storeport.LearnerContextNarrativeSignals, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	query, args := s.learnerContextNarrativeQuery(learnerID, domainID, concepts, now)
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get learner context narrative signals: %w", err)
	}
	defer rows.Close()

	signals := &storeport.LearnerContextNarrativeSignals{}
	for rows.Next() {
		var kind string
		var kindOrder, position int
		var history storeport.LearnerContextConceptHistory
		var recentSuccess int
		var autonomyScore float64
		var streak int
		if err := rows.Scan(
			&kindOrder,
			&position,
			&kind,
			&history.Concept,
			&history.TotalBefore,
			&history.SuccessfulBefore,
			&recentSuccess,
			&autonomyScore,
			&streak,
		); err != nil {
			return nil, fmt.Errorf("scan learner context narrative signal: %w", err)
		}
		switch kind {
		case "concept":
			history.RecentSuccess = recentSuccess != 0
			signals.ConceptHistory = append(signals.ConceptHistory, history)
		case "streak":
			signals.SessionStreak = streak
		case "affect":
			signals.RecentAutonomyScores = append(signals.RecentAutonomyScores, autonomyScore)
		default:
			return nil, fmt.Errorf("scan learner context narrative signal: unknown kind %q", kind)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate learner context narrative signals: %w", err)
	}
	return signals, nil
}

// learnerContextNarrativeQuery keeps the production query and the opt-in
// PostgreSQL plan gate on exactly the same SQL. Returning the arguments also
// prevents performance tests from drifting into a synthetic approximation.
func (s *Store) learnerContextNarrativeQuery(learnerID, domainID string, concepts []string, now time.Time) (string, []any) {
	historySince := now.Add(-30 * 24 * time.Hour)
	milestoneSince := now.Add(-7 * 24 * time.Hour)

	domainHistorySQL := `domain_history AS (
		SELECT '' AS concept, 0 AS total_before, 0 AS successful_before,
		       0 AS recent_success
		WHERE 1 = 0
	)`
	args := make([]any, 0, len(concepts)+6)
	if len(concepts) > 0 {
		placeholders := make([]string, len(concepts))
		for i, concept := range concepts {
			placeholders[i] = "?"
			args = append(args, concept)
		}
		domainHistorySQL = `domain_history AS (
		SELECT i.concept,
		       SUM(CASE WHEN i.created_at < ? THEN 1 ELSE 0 END) AS total_before,
		       SUM(CASE WHEN i.created_at < ? AND i.success = 1 THEN 1 ELSE 0 END) AS successful_before,
		       MAX(CASE WHEN i.success = 1 AND i.created_at >= ? THEN 1 ELSE 0 END) AS recent_success
		FROM ` + currentInteractionsSQL + ` i
		WHERE i.learner_id = ? AND i.domain_id = ?
		  AND i.concept IN (` + strings.Join(placeholders, ",") + `)
		  AND (i.created_at < ? OR (i.success = 1 AND i.created_at >= ?))
		GROUP BY i.concept
	)`
		conceptArgs := append([]any(nil), args...)
		args = []any{historySince, historySince, milestoneSince, learnerID, domainID}
		args = append(args, conceptArgs...)
		args = append(args, historySince, milestoneSince)
	}

	dateExpr := s.utcDateExpr("created_at")
	today := now.Format("2006-01-02")
	streakSQL := `CASE
		WHEN COUNT(*) = 0 THEN 0
		WHEN julianday(?) - julianday(MAX(latest_day)) > 1 THEN 0
		ELSE COALESCE(SUM(CASE
			WHEN julianday(day) = julianday(latest_day) - (rn - 1) THEN 1
			ELSE 0 END), 0)
	END`
	if s.dialect == DialectPostgres {
		streakSQL = `CASE
			WHEN COUNT(*) = 0 THEN 0
			WHEN CAST(? AS date) - CAST(MAX(latest_day) AS date) > 1 THEN 0
			ELSE COALESCE(SUM(CASE
				WHEN CAST(day AS date) = CAST(latest_day AS date) - CAST(rn - 1 AS integer) THEN 1
				ELSE 0 END), 0)
		END`
	}

	query := `WITH ` + domainHistorySQL + `,
	activity_days AS (
		SELECT DISTINCT ` + dateExpr + ` AS day
		FROM interactions
		WHERE learner_id = ?
	),
	ranked_days AS (
		SELECT day, ROW_NUMBER() OVER (ORDER BY day DESC) AS rn,
		       MAX(day) OVER () AS latest_day
		FROM activity_days
	),
	streak_value AS (
		SELECT ` + streakSQL + ` AS streak
		FROM ranked_days
	),
	recent_affect AS (
		SELECT autonomy_score,
		       ROW_NUMBER() OVER (ORDER BY created_at DESC) AS position
		FROM affect_states
		WHERE learner_id = ?
		ORDER BY created_at DESC
		LIMIT 5
	)
	SELECT 0 AS kind_order, 0 AS position, 'concept' AS kind, concept,
	       total_before, successful_before, recent_success,
	       0.0 AS autonomy_score, 0 AS streak
	FROM domain_history
	UNION ALL
	SELECT 1, 0, 'streak', '', 0, 0, 0, 0.0, streak
	FROM streak_value
	UNION ALL
	SELECT 2, position, 'affect', '', 0, 0, 0, autonomy_score, 0
	FROM recent_affect
	ORDER BY kind_order, position`

	args = append(args, learnerID, today, learnerID)
	return query, args
}
