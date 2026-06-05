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

const pedagogicalSnapshotCols = `id, interaction_id, learner_id, domain_id, concept, activity_type, before_json, observation_json, after_json, decision_json, interpretation_brief, created_at`

func (s *Store) CreatePedagogicalSnapshot(ctx context.Context, snapshot *models.PedagogicalSnapshot) error {
	return createPedagogicalSnapshotWithQ(s.db, snapshot)
}

// CreatePedagogicalSnapshotTx runs CreatePedagogicalSnapshot inside the
// supplied transaction (used by applyInteraction to group the snapshot
// write with the interaction + concept_states upsert).
func (s *Store) CreatePedagogicalSnapshotTx(tx *sql.Tx, snapshot *models.PedagogicalSnapshot) error {
	return createPedagogicalSnapshotWithQ(tx, snapshot)
}

func createPedagogicalSnapshotWithQ(q querier, snapshot *models.PedagogicalSnapshot) error {
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	} else {
		snapshot.CreatedAt = snapshot.CreatedAt.UTC()
	}
	result, err := q.Exec(
		`INSERT INTO pedagogical_snapshots
		    (interaction_id, learner_id, domain_id, concept, activity_type, before_json, observation_json, after_json, decision_json, interpretation_brief, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snapshot.InteractionID, snapshot.LearnerID, snapshot.DomainID, snapshot.Concept,
		snapshot.ActivityType, snapshot.BeforeJSON, snapshot.ObservationJSON,
		snapshot.AfterJSON, snapshot.DecisionJSON, snapshot.InterpretationBrief, snapshot.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create pedagogical snapshot: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get pedagogical snapshot id: %w", err)
	}
	snapshot.ID = id
	return nil
}

func (s *Store) GetPedagogicalSnapshots(ctx context.Context, learnerID, domainID, concept string, limit int) ([]*models.PedagogicalSnapshot, error) {
	if learnerID == "" {
		return nil, fmt.Errorf("learner_id is required")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	where := []string{"learner_id = ?"}
	args := []any{learnerID}
	if domainID != "" {
		where = append(where, "domain_id = ?")
		args = append(args, domainID)
	}
	if concept != "" {
		where = append(where, "concept = ?")
		args = append(args, concept)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pedagogicalSnapshotCols+`
		 FROM pedagogical_snapshots
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY created_at DESC, interaction_id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("get pedagogical snapshots: %w", err)
	}
	defer rows.Close()

	return scanPedagogicalSnapshots(rows)
}

func scanPedagogicalSnapshots(rows *sql.Rows) ([]*models.PedagogicalSnapshot, error) {
	var out []*models.PedagogicalSnapshot
	for rows.Next() {
		snapshot := &models.PedagogicalSnapshot{}
		if err := rows.Scan(
			&snapshot.ID, &snapshot.InteractionID, &snapshot.LearnerID,
			&snapshot.DomainID, &snapshot.Concept, &snapshot.ActivityType,
			&snapshot.BeforeJSON, &snapshot.ObservationJSON, &snapshot.AfterJSON,
			&snapshot.DecisionJSON, &snapshot.InterpretationBrief, &snapshot.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan pedagogical snapshot: %w", err)
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}
