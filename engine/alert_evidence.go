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

// EvidenceStore is the persistence surface of an EvidenceSnapshot: batched
// evaluated assessment attempts and transfer records of one domain.
type EvidenceStore interface {
	storeport.AssessmentStore
	storeport.MetacognitionStore
}

// LoadEvidenceSnapshot fetches transfer and evaluated-assessment evidence for
// the requested concepts without the former two-queries-per-concept pattern.
func LoadEvidenceSnapshot(ctx context.Context, store EvidenceStore, learnerID, domainID string, concepts []string) (*EvidenceSnapshot, error) {
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

// DomainEvidenceReader is the optional multi-domain evidence read of the SQL
// store (db/learner_batch_reads.go): one statement per evidence kind answers
// for every domain of a learner view. Results are grouped by domain then
// concept, newest first.
type DomainEvidenceReader interface {
	GetTransferScoresByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string) (map[string]map[string][]*models.TransferRecord, error)
	GetEvaluatedAssessmentAttemptsByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string, limitPerConcept int) (map[string]map[string][]*models.AssessmentAttempt, error)
}

// LoadEvidenceSnapshots loads the evidence snapshot of every domain, keyed by
// domain ID, each restricted to the concepts of its graph. A store
// implementing DomainEvidenceReader answers with two statements in total;
// other stores fall back to LoadEvidenceSnapshot per domain.
func LoadEvidenceSnapshots(ctx context.Context, store EvidenceStore, scope models.TenantScope, learnerID string, domains []*models.Domain) (map[string]*EvidenceSnapshot, error) {
	out := make(map[string]*EvidenceSnapshot, len(domains))
	reader, batched := store.(DomainEvidenceReader)
	if !batched {
		for _, domain := range domains {
			snapshot, err := LoadEvidenceSnapshot(ctx, store, learnerID, domain.ID, domain.Graph.Concepts)
			if err != nil {
				return nil, err
			}
			out[domain.ID] = snapshot
		}
		return out, nil
	}
	ids := make([]string, 0, len(domains))
	for _, domain := range domains {
		ids = append(ids, domain.ID)
	}
	transfers, err := reader.GetTransferScoresByDomains(ctx, scope, learnerID, ids)
	if err != nil {
		return nil, fmt.Errorf("load evidence snapshot transfers: %w", err)
	}
	assessments, err := reader.GetEvaluatedAssessmentAttemptsByDomains(ctx, scope, learnerID, ids, evidenceAssessmentLimitPerConcept)
	if err != nil {
		return nil, fmt.Errorf("load evidence snapshot assessments: %w", err)
	}
	for _, domain := range domains {
		concepts := uniqueEvidenceConcepts(domain.Graph.Concepts)
		snapshot := &EvidenceSnapshot{
			LearnerID:            learnerID,
			DomainID:             domain.ID,
			TransfersByConcept:   make(map[string][]*models.TransferRecord, len(concepts)),
			AssessmentsByConcept: make(map[string][]*models.AssessmentAttempt, len(concepts)),
			concepts:             make(map[string]struct{}, len(concepts)),
		}
		for _, concept := range concepts {
			snapshot.concepts[concept] = struct{}{}
			snapshot.TransfersByConcept[concept] = transfers[domain.ID][concept]
			snapshot.AssessmentsByConcept[concept] = assessments[domain.ID][concept]
		}
		out[domain.ID] = snapshot
	}
	return out, nil
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

func ensureEvidenceSnapshot(ctx context.Context, store EvidenceStore, learnerID, domainID string, concepts []string, snapshot *EvidenceSnapshot) (*EvidenceSnapshot, error) {
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
