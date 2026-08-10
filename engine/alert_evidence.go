// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const evidenceAssessmentLimitPerConcept = 100

// EvidenceSnapshot is the request-scoped, domain-scoped read model shared by
// alerting, mastery classification, orchestration and OLM enrichment. Loading
// it costs two batched store reads regardless of the number of concepts.
type EvidenceSnapshot struct {
	LearnerID string
	DomainID  string

	TransfersByConcept   map[string][]*models.TransferRecord
	AssessmentsByConcept map[string][]*models.AssessmentAttempt
	concepts             map[string]struct{}
}

// LoadEvidenceSnapshot fetches transfer and evaluated-assessment evidence for
// the requested concepts without the former two-queries-per-concept pattern.
func LoadEvidenceSnapshot(ctx context.Context, store storeport.Store, learnerID, domainID string, concepts []string) (*EvidenceSnapshot, error) {
	concepts = uniqueEvidenceConcepts(concepts)
	snapshot := &EvidenceSnapshot{
		LearnerID:            learnerID,
		DomainID:             domainID,
		TransfersByConcept:   make(map[string][]*models.TransferRecord, len(concepts)),
		AssessmentsByConcept: make(map[string][]*models.AssessmentAttempt, len(concepts)),
		concepts:             make(map[string]struct{}, len(concepts)),
	}
	for _, concept := range concepts {
		snapshot.concepts[concept] = struct{}{}
	}
	if len(concepts) == 0 {
		return snapshot, nil
	}

	transfers, err := store.GetTransferScoresBatchInDomain(ctx, learnerID, domainID, concepts)
	if err != nil {
		return nil, fmt.Errorf("load evidence snapshot transfers: %w", err)
	}
	assessments, err := store.GetEvaluatedAssessmentAttemptsBatchInDomain(
		ctx, learnerID, domainID, concepts, evidenceAssessmentLimitPerConcept,
	)
	if err != nil {
		return nil, fmt.Errorf("load evidence snapshot assessments: %w", err)
	}
	snapshot.TransfersByConcept = transfers
	snapshot.AssessmentsByConcept = assessments
	return snapshot, nil
}

// ForConcept returns the evidence slices for one concept. Missing concepts and
// concepts with no evidence intentionally have the same empty result.
func (s *EvidenceSnapshot) ForConcept(concept string) MasteryAlertEvidence {
	if s == nil {
		return MasteryAlertEvidence{}
	}
	return MasteryAlertEvidence{
		Transfers:   s.TransfersByConcept[concept],
		Assessments: s.AssessmentsByConcept[concept],
	}
}

// ForMasteryAlerts projects a snapshot onto the concepts that currently have
// state, preserving the historical alert-loader shape.
func (s *EvidenceSnapshot) ForMasteryAlerts(states []*models.ConceptState) map[string]MasteryAlertEvidence {
	out := make(map[string]MasteryAlertEvidence, len(states))
	for _, state := range states {
		if state == nil || state.Concept == "" {
			continue
		}
		out[state.Concept] = s.ForConcept(state.Concept)
	}
	return out
}

func (s *EvidenceSnapshot) matches(learnerID, domainID string, concepts []string) bool {
	if s == nil || s.LearnerID != learnerID || s.DomainID != domainID {
		return false
	}
	for _, concept := range uniqueEvidenceConcepts(concepts) {
		if _, ok := s.concepts[concept]; !ok {
			return false
		}
	}
	return true
}

func ensureEvidenceSnapshot(ctx context.Context, store storeport.Store, learnerID, domainID string, concepts []string, snapshot *EvidenceSnapshot) (*EvidenceSnapshot, error) {
	if snapshot.matches(learnerID, domainID, concepts) {
		return snapshot, nil
	}
	return LoadEvidenceSnapshot(ctx, store, learnerID, domainID, concepts)
}

func uniqueEvidenceConcepts(concepts []string) []string {
	out := make([]string, 0, len(concepts))
	seen := make(map[string]struct{}, len(concepts))
	for _, concept := range concepts {
		if concept == "" {
			continue
		}
		if _, ok := seen[concept]; ok {
			continue
		}
		seen[concept] = struct{}{}
		out = append(out, concept)
	}
	return out
}

// LoadMasteryAlertEvidence loads the same assessment/transfer facts consumed
// by check_mastery. Keeping this read path shared prevents MASTERY_READY from
// silently evaluating an empty transfer profile while the explicit mastery
// tool sees a recent trusted failure.
func LoadMasteryAlertEvidence(ctx context.Context, store storeport.Store, learnerID, domainID string, states []*models.ConceptState) (map[string]MasteryAlertEvidence, error) {
	concepts := make([]string, 0, len(states))
	for _, state := range states {
		if state != nil {
			concepts = append(concepts, state.Concept)
		}
	}
	snapshot, err := LoadEvidenceSnapshot(ctx, store, learnerID, domainID, concepts)
	if err != nil {
		return nil, err
	}
	return snapshot.ForMasteryAlerts(states), nil
}
