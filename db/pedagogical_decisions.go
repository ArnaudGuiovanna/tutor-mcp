// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/json"
	"fmt"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// CreatePedagogicalDecision freezes the runtime instruction, not generated
// teaching content. The authenticated tenant and learner must own its session.
func (s *Store) CreatePedagogicalDecision(ctx context.Context, scope models.TenantScope, decision *models.PedagogicalDecision) error {
	if decision == nil || decision.ID == "" || decision.LearnerID == "" || scope.LearnerID != decision.LearnerID ||
		decision.DomainID == "" || decision.SessionID == "" || decision.CurriculumVersion < 1 || decision.CreatedAt.IsZero() ||
		decision.PolicyVersion == "" || decision.Contract.DecisionID != decision.ID ||
		decision.Contract.CurriculumVersion != decision.CurriculumVersion || decision.Contract.PolicyVersion != decision.PolicyVersion {
		return fmt.Errorf("create pedagogical decision: invalid contract or scope")
	}
	return s.withPedagogicalScope(ctx, scope, func(txCtx context.Context, txs *Store) error {
		if txs.dialect == DialectPostgres {
			var version int
			if err := txs.queryRow(txCtx, `SELECT graph_version FROM domains WHERE id = ? AND tenant_id = ? AND learner_id = ? FOR SHARE`,
				decision.DomainID, scope.TenantID, decision.LearnerID).Scan(&version); err != nil {
				return err
			}
			if version != decision.CurriculumVersion {
				return fmt.Errorf("create pedagogical decision: stale curriculum")
			}
		}
		var count int
		if err := txs.queryRow(txCtx, `SELECT COUNT(*) FROM domains d
		 JOIN learning_sessions ls ON ls.id = ? AND ls.learner_id = d.learner_id AND ls.tenant_id = d.tenant_id AND ls.domain_id = d.id
		 WHERE d.id = ? AND d.learner_id = ? AND d.tenant_id = ? AND d.graph_version = ?`,
			decision.SessionID, decision.DomainID, decision.LearnerID, scope.TenantID, decision.CurriculumVersion).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("create pedagogical decision: stale curriculum or scope mismatch")
		}
		payload, err := json.Marshal(decision)
		if err != nil {
			return err
		}
		_, err = txs.exec(txCtx, `INSERT INTO pedagogical_decisions
		 (id, tenant_id, learner_id, domain_id, session_id, curriculum_version, snapshot_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, decision.ID, scope.TenantID, decision.LearnerID,
			decision.DomainID, decision.SessionID, decision.CurriculumVersion, string(payload), decision.CreatedAt.UTC())
		return err
	})
}

func (s *Store) GetPedagogicalDecision(ctx context.Context, scope models.TenantScope, decisionID string) (*models.PedagogicalDecision, error) {
	if scope.LearnerID == "" || decisionID == "" {
		return nil, fmt.Errorf("get pedagogical decision: learner and decision required")
	}
	var decision models.PedagogicalDecision
	err := s.withPedagogicalScope(ctx, scope, func(txCtx context.Context, txs *Store) error {
		var payload string
		if err := txs.queryRow(txCtx, `SELECT snapshot_json FROM pedagogical_decisions
		 WHERE id = ? AND tenant_id = ? AND learner_id = ?`, decisionID, scope.TenantID, scope.LearnerID).Scan(&payload); err != nil {
			return err
		}
		return json.Unmarshal([]byte(payload), &decision)
	})
	if err != nil {
		return nil, err
	}
	return &decision, nil
}

// Compose both with the root store used by MCP middleware and with the typed
// transaction store returned to other callers. Never replace an active scope.
func (s *Store) withPedagogicalScope(ctx context.Context, scope models.TenantScope, fn func(context.Context, *Store) error) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if s.inTenantTransaction(ctx, scope) {
		return fn(ctx, s)
	}
	return s.WithTenantTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		return fn(txCtx, scoped.(*Store))
	})
}
