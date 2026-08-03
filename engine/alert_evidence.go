// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// LoadMasteryAlertEvidence loads the same assessment/transfer facts consumed
// by check_mastery. Keeping this read path shared prevents MASTERY_READY from
// silently evaluating an empty transfer profile while the explicit mastery
// tool sees a recent trusted failure.
func LoadMasteryAlertEvidence(ctx context.Context, store storeport.Store, learnerID, domainID string, states []*models.ConceptState) (map[string]MasteryAlertEvidence, error) {
	out := make(map[string]MasteryAlertEvidence, len(states))
	seen := make(map[string]bool, len(states))
	for _, state := range states {
		if state == nil || state.Concept == "" || seen[state.Concept] {
			continue
		}
		seen[state.Concept] = true
		transfers, err := store.GetTransferScoresInDomain(ctx, learnerID, domainID, state.Concept)
		if err != nil {
			return nil, fmt.Errorf("load mastery alert transfers for %s: %w", state.Concept, err)
		}
		assessments, err := store.GetEvaluatedAssessmentAttemptsInDomain(ctx, learnerID, domainID, state.Concept, 100)
		if err != nil {
			return nil, fmt.Errorf("load mastery alert assessments for %s: %w", state.Concept, err)
		}
		out[state.Concept] = MasteryAlertEvidence{
			Transfers:   transfers,
			Assessments: assessments,
		}
	}
	return out, nil
}
