// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"tutor-mcp/models"
)

// ─── Affect States ──────────────────────────────────────────────────────────

func (s *Store) UpsertAffectState(ctx context.Context, a *models.AffectState) error {
	a.CreatedAt = time.Now().UTC()
	_, err := s.exec(ctx,
		`INSERT INTO affect_states (learner_id, session_id, energy, subject_confidence, satisfaction, perceived_difficulty, next_session_intent, autonomy_score, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(learner_id, session_id) DO UPDATE SET
		    energy               = CASE WHEN excluded.energy > 0 THEN excluded.energy ELSE affect_states.energy END,
		    subject_confidence   = CASE WHEN excluded.subject_confidence > 0 THEN excluded.subject_confidence ELSE affect_states.subject_confidence END,
		    satisfaction         = CASE WHEN excluded.satisfaction > 0 THEN excluded.satisfaction ELSE affect_states.satisfaction END,
		    perceived_difficulty = CASE WHEN excluded.perceived_difficulty > 0 THEN excluded.perceived_difficulty ELSE affect_states.perceived_difficulty END,
		    next_session_intent  = CASE WHEN excluded.next_session_intent > 0 THEN excluded.next_session_intent ELSE affect_states.next_session_intent END,
		    autonomy_score       = CASE WHEN excluded.autonomy_score > 0 THEN excluded.autonomy_score ELSE affect_states.autonomy_score END`,
		a.LearnerID, a.SessionID, a.Energy, a.SubjectConfidence,
		a.Satisfaction, a.PerceivedDifficulty, a.NextSessionIntent, a.AutonomyScore, a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert affect state: %w", err)
	}
	return nil
}

func (s *Store) GetRecentAffectStates(ctx context.Context, learnerID string, limit int) ([]*models.AffectState, error) {
	rows, err := s.query(ctx,
		`SELECT id, learner_id, session_id, energy, subject_confidence, satisfaction, perceived_difficulty, next_session_intent, autonomy_score, created_at
		 FROM affect_states WHERE learner_id = ? ORDER BY created_at DESC LIMIT ?`,
		learnerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get recent affect states: %w", err)
	}
	defer rows.Close()
	return scanAffectStates(rows)
}

func scanAffectStates(rows *sql.Rows) ([]*models.AffectState, error) {
	var states []*models.AffectState
	for rows.Next() {
		a := &models.AffectState{}
		if err := rows.Scan(
			&a.ID, &a.LearnerID, &a.SessionID, &a.Energy, &a.SubjectConfidence,
			&a.Satisfaction, &a.PerceivedDifficulty, &a.NextSessionIntent, &a.AutonomyScore, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan affect state: %w", err)
		}
		states = append(states, a)
	}
	return states, rows.Err()
}

func (s *Store) GetAffectBySession(ctx context.Context, learnerID, sessionID string) (*models.AffectState, error) {
	a := &models.AffectState{}
	err := s.queryRow(ctx,
		`SELECT id, learner_id, session_id, energy, subject_confidence, satisfaction, perceived_difficulty, next_session_intent, autonomy_score, created_at
		 FROM affect_states WHERE learner_id = ? AND session_id = ?`,
		learnerID, sessionID,
	).Scan(
		&a.ID, &a.LearnerID, &a.SessionID, &a.Energy, &a.SubjectConfidence,
		&a.Satisfaction, &a.PerceivedDifficulty, &a.NextSessionIntent, &a.AutonomyScore, &a.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get affect by session: %w", err)
	}
	return a, nil
}

// ─── Calibration Records ────────────────────────────────────────────────────

func (s *Store) CreateCalibrationPrediction(ctx context.Context, r *models.CalibrationRecord) error {
	r.CreatedAt = time.Now().UTC()
	_, err := s.exec(ctx,
		`INSERT INTO calibration_records (prediction_id, learner_id, concept_id, predicted, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		r.PredictionID, r.LearnerID, r.ConceptID, r.Predicted, r.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create calibration prediction: %w", err)
	}
	return nil
}

// CompleteCalibrationRecord finalises a calibration prediction owned by the
// supplied learner. The learner_id filter is enforced at the DB layer so that
// any future caller cannot accidentally reintroduce the IDOR closed by issues
// #34/#87 — defence-in-depth, mirrors Store.ArchiveDomain.
func (s *Store) CompleteCalibrationRecord(ctx context.Context, predictionID, learnerID string, actual, delta float64) error {
	result, err := s.exec(ctx,
		`UPDATE calibration_records SET actual = ?, delta = ?
		 WHERE prediction_id = ? AND learner_id = ?`,
		actual, delta, predictionID, learnerID,
	)
	if err != nil {
		return fmt.Errorf("complete calibration record: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("calibration record not found: %s", predictionID)
	}
	return nil
}

// GetCalibrationRecord fetches a calibration prediction scoped to the supplied
// learner. The learner_id filter is enforced at the DB layer so callers cannot
// accidentally surface another learner's row — defence-in-depth, mirrors
// Store.ArchiveDomain.
func (s *Store) GetCalibrationRecord(ctx context.Context, predictionID, learnerID string) (*models.CalibrationRecord, error) {
	r := &models.CalibrationRecord{}
	var actual, delta sql.NullFloat64
	err := s.queryRow(ctx,
		`SELECT prediction_id, learner_id, concept_id, predicted, actual, delta, created_at
		 FROM calibration_records WHERE prediction_id = ? AND learner_id = ?`,
		predictionID, learnerID,
	).Scan(&r.PredictionID, &r.LearnerID, &r.ConceptID, &r.Predicted, &actual, &delta, &r.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("calibration record not found: %s", predictionID)
		}
		return nil, fmt.Errorf("get calibration record: %w", err)
	}
	if actual.Valid {
		r.Actual = &actual.Float64
	}
	if delta.Valid {
		r.Delta = &delta.Float64
	}
	return r, nil
}

func (s *Store) GetCalibrationBias(ctx context.Context, learnerID string, limit int) (float64, error) {
	var bias sql.NullFloat64
	err := s.queryRow(ctx,
		`SELECT AVG(delta) FROM (
		    SELECT delta FROM calibration_records
		    WHERE learner_id = ? AND delta IS NOT NULL
		    ORDER BY created_at DESC LIMIT ?
		)`, learnerID, limit,
	).Scan(&bias)
	if err != nil || !bias.Valid {
		return 0, nil
	}
	return bias.Float64, nil
}

func (s *Store) GetCalibrationBiasHistory(ctx context.Context, learnerID string, limit int) ([]float64, error) {
	rows, err := s.query(ctx,
		`SELECT delta FROM calibration_records
		 WHERE learner_id = ? AND delta IS NOT NULL
		 ORDER BY created_at DESC LIMIT ?`,
		learnerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get calibration bias history: %w", err)
	}
	defer rows.Close()
	var deltas []float64
	for rows.Next() {
		var d float64
		if err := rows.Scan(&d); err != nil {
			return deltas, nil
		}
		deltas = append(deltas, d)
	}
	return deltas, nil
}

// ─── Transfer Records ───────────────────────────────────────────────────────

func (s *Store) CreateTransferRecord(ctx context.Context, r *models.TransferRecord) error {
	r.CreatedAt = time.Now().UTC()
	_, err := s.exec(ctx,
		`INSERT INTO transfer_records (learner_id, concept_id, context_type, score, session_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.LearnerID, r.ConceptID, r.ContextType, r.Score, r.SessionID, r.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create transfer record: %w", err)
	}
	return nil
}

func (s *Store) GetTransferScores(ctx context.Context, learnerID, conceptID string) ([]*models.TransferRecord, error) {
	rows, err := s.query(ctx,
		`SELECT id, learner_id, concept_id, context_type, score, session_id, created_at
		 FROM transfer_records WHERE learner_id = ? AND concept_id = ?
		 ORDER BY created_at DESC`,
		learnerID, conceptID,
	)
	if err != nil {
		return nil, fmt.Errorf("get transfer scores: %w", err)
	}
	defer rows.Close()
	var records []*models.TransferRecord
	for rows.Next() {
		r := &models.TransferRecord{}
		if err := rows.Scan(&r.ID, &r.LearnerID, &r.ConceptID, &r.ContextType, &r.Score, &r.SessionID, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan transfer record: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// GetTransferRecordsByLearner returns every transfer probe record for the
// learner across all concepts. Used by the metacognitive alert pipeline to
// detect TRANSFER_BLOCKED (mastered concepts whose context-transfer scores
// remain below 0.50 on 2+ contexts). Newest-first.
func (s *Store) GetTransferRecordsByLearner(ctx context.Context, learnerID string) ([]*models.TransferRecord, error) {
	rows, err := s.query(ctx,
		`SELECT id, learner_id, concept_id, context_type, score, session_id, created_at
		 FROM transfer_records WHERE learner_id = ?
		 ORDER BY created_at DESC`,
		learnerID,
	)
	if err != nil {
		return nil, fmt.Errorf("get transfer records by learner: %w", err)
	}
	defer rows.Close()
	var records []*models.TransferRecord
	for rows.Next() {
		r := &models.TransferRecord{}
		if err := rows.Scan(&r.ID, &r.LearnerID, &r.ConceptID, &r.ContextType, &r.Score, &r.SessionID, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan transfer record: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// ─── Autonomy Queries ───────────────────────────────────────────────────────

func (s *Store) GetHintStatsForMastered(ctx context.Context, learnerID string, threshold float64) (hints int, total int, err error) {
	err = s.queryRow(ctx,
		`SELECT COALESCE(SUM(i.hints_requested), 0), COUNT(*)
		 FROM interactions i
		 JOIN concept_states cs ON i.learner_id = cs.learner_id AND i.concept = cs.concept
		 WHERE i.learner_id = ? AND cs.p_mastery >= ?`,
		learnerID, threshold,
	).Scan(&hints, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("get hint stats for mastered: %w", err)
	}
	return hints, total, nil
}

func (s *Store) UpdateAffectAutonomyScore(ctx context.Context, learnerID, sessionID string, score float64) error {
	_, err := s.exec(ctx,
		`UPDATE affect_states SET autonomy_score = ? WHERE learner_id = ? AND session_id = ?`,
		score, learnerID, sessionID,
	)
	if err != nil {
		return fmt.Errorf("update affect autonomy score: %w", err)
	}
	return nil
}

func (s *Store) CountProactiveReviews(ctx context.Context, learnerID string, since time.Time) (proactive int, total int, err error) {
	err = s.queryRow(ctx,
		`SELECT COALESCE(SUM(is_proactive_review), 0), COUNT(*)
		 FROM interactions
		 WHERE learner_id = ? AND created_at >= ? AND activity_type != 'NEW_CONCEPT' AND activity_type != 'REST' AND activity_type != 'SETUP_DOMAIN'`,
		learnerID, since,
	).Scan(&proactive, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("count proactive reviews: %w", err)
	}
	return proactive, total, nil
}
