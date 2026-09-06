// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"
)

// CurriculumConceptStatus separates concepts that are currently routable from
// concepts retained solely for historical interpretation. Retired concepts
// never disappear from a curriculum snapshot, so evidence created against an
// older version remains explainable after a split, merge, or removal.
type CurriculumConceptStatus string

const (
	CurriculumConceptActive  CurriculumConceptStatus = "active"
	CurriculumConceptRetired CurriculumConceptStatus = "retired"
)

// CurriculumLevel is deliberately domain-neutral. It describes the intended
// progression within this curriculum, not a school year or a programming-only
// difficulty scale.
type CurriculumLevel string

const (
	CurriculumLevelUnspecified  CurriculumLevel = "unspecified"
	CurriculumLevelFoundation   CurriculumLevel = "foundation"
	CurriculumLevelIntermediate CurriculumLevel = "intermediate"
	CurriculumLevelAdvanced     CurriculumLevel = "advanced"
)

// CurriculumOutcome is an observable capability expected from a learner.
// ID is stable inside the owning concept so criteria and later exports can
// refer to an outcome without coupling themselves to mutable prose.
type CurriculumOutcome struct {
	ID         string `json:"id"`
	Statement  string `json:"statement"`
	Observable string `json:"observable,omitempty"`
}

// CurriculumCriterion describes what acceptable evidence looks like for one
// competency. It is curriculum design metadata; assessment-attempt rubrics
// remain separate immutable evaluation envelopes.
type CurriculumCriterion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Evidence    string `json:"evidence,omitempty"`
}

// CurriculumConcept carries the versioned curriculum description of a stable
// competency. ID and Key never change. Label and the pedagogical metadata may
// evolve in later snapshots. Key is the legacy engine-facing graph token; this
// indirection lets a rename preserve concept_state and historical evidence.
type CurriculumConcept struct {
	ID          string                  `json:"id"`
	Key         string                  `json:"key"`
	Label       string                  `json:"label"`
	Description string                  `json:"description,omitempty"`
	Level       CurriculumLevel         `json:"level"`
	Outcomes    []CurriculumOutcome     `json:"outcomes"`
	Criteria    []CurriculumCriterion   `json:"criteria"`
	Status      CurriculumConceptStatus `json:"status"`
	ReplacedBy  []string                `json:"replaced_by,omitempty"`
}

// CurriculumProvenance records where a curriculum revision came from and why
// it was made. It is intentionally distinct from Review: provenance explains
// authorship/source; review records a later quality decision.
type CurriculumProvenance struct {
	SourceType string `json:"source_type"`
	SourceRef  string `json:"source_ref,omitempty"`
	Author     string `json:"author"`
	Rationale  string `json:"rationale"`
}

type CurriculumReviewStatus string

const (
	CurriculumReviewUnreviewed CurriculumReviewStatus = "unreviewed"
	CurriculumReviewInReview   CurriculumReviewStatus = "in_review"
	CurriculumReviewApproved   CurriculumReviewStatus = "approved"
	CurriculumReviewRejected   CurriculumReviewStatus = "rejected"
)

// CurriculumReview is an auditable annotation, not an authorization claim.
// The authenticated actor that submitted it is persisted separately as
// CreatedBy on the snapshot.
type CurriculumReview struct {
	Status     CurriculumReviewStatus `json:"status"`
	ReviewerID string                 `json:"reviewer_id,omitempty"`
	Notes      string                 `json:"notes,omitempty"`
	ReviewedAt *time.Time             `json:"reviewed_at,omitempty"`
}

type CurriculumOperationType string

const (
	CurriculumOperationCreate              CurriculumOperationType = "create"
	CurriculumOperationBaselineImport      CurriculumOperationType = "baseline_import"
	CurriculumOperationAdd                 CurriculumOperationType = "add"
	CurriculumOperationRename              CurriculumOperationType = "rename"
	CurriculumOperationUpdateMetadata      CurriculumOperationType = "update_metadata"
	CurriculumOperationRepairPrerequisites CurriculumOperationType = "repair_prerequisites"
	CurriculumOperationSplit               CurriculumOperationType = "split"
	CurriculumOperationMerge               CurriculumOperationType = "merge"
	CurriculumOperationRemove              CurriculumOperationType = "remove"
	CurriculumOperationLegacyUpdate        CurriculumOperationType = "legacy_graph_update"
)

// CurriculumOperation is stored with every immutable snapshot. Source and
// target IDs make structural transformations mechanically traceable even when
// their labels later change.
type CurriculumOperation struct {
	Type             CurriculumOperationType `json:"type"`
	SourceConceptIDs []string                `json:"source_concept_ids,omitempty"`
	TargetConceptIDs []string                `json:"target_concept_ids,omitempty"`
	Rationale        string                  `json:"rationale"`
}

// CurriculumSnapshot is a complete immutable version of a domain curriculum.
// Graph contains active immutable Keys; Concepts contains both active and
// retired concepts so older learner evidence remains interpretable.
type CurriculumSnapshot struct {
	DomainID       string                    `json:"domain_id"`
	Version        int                       `json:"version"`
	ParentVersion  int                       `json:"parent_version,omitempty"`
	Graph          KnowledgeSpace            `json:"graph"`
	Concepts       []CurriculumConcept       `json:"concepts"`
	Operation      CurriculumOperation       `json:"operation"`
	Provenance     CurriculumProvenance      `json:"provenance"`
	Review         CurriculumReview          `json:"review"`
	CreatedBy      string                    `json:"created_by"`
	CreatedAt      time.Time                 `json:"created_at"`
	Reconciliation *CurriculumReconciliation `json:"reconciliation,omitempty"`
}

// CurriculumReconciliation is computed by persistence, never accepted as an
// author's assertion of equivalence. Learner values stay in pedagogical
// snapshots, under their own retention/erasure policy, not in curriculum history.
type CurriculumReconciliation struct {
	PolicyVersion         string   `json:"policy_version"`
	InvalidatedConceptIDs []string `json:"invalidated_concept_ids"`
}

// NewCurriculumStableID returns an opaque, URL-safe stable identifier. The
// prefix is descriptive only; uniqueness comes from 128 bits of randomness.
func NewCurriculumStableID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate curriculum id: %w", err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// NewCurriculumConcept materializes the stable identity used by all future
// versions. New concepts start active and explicitly unclassified until a
// curriculum author supplies their outcomes, criteria, and level.
func NewCurriculumConcept(key, label string) (CurriculumConcept, error) {
	id, err := NewCurriculumStableID("concept")
	if err != nil {
		return CurriculumConcept{}, err
	}
	return CurriculumConcept{
		ID:       id,
		Key:      key,
		Label:    label,
		Level:    CurriculumLevelUnspecified,
		Outcomes: []CurriculumOutcome{},
		Criteria: []CurriculumCriterion{},
		Status:   CurriculumConceptActive,
	}, nil
}

// CloneCurriculumSnapshot returns a deep copy suitable for constructing the
// next version without mutating an object returned to another caller.
func CloneCurriculumSnapshot(in *CurriculumSnapshot) *CurriculumSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	if in.Reconciliation != nil {
		r := *in.Reconciliation
		r.InvalidatedConceptIDs = append([]string(nil), r.InvalidatedConceptIDs...)
		out.Reconciliation = &r
	}
	out.Graph.Concepts = append([]string(nil), in.Graph.Concepts...)
	out.Graph.Prerequisites = make(map[string][]string, len(in.Graph.Prerequisites))
	for concept, prerequisites := range in.Graph.Prerequisites {
		out.Graph.Prerequisites[concept] = append([]string(nil), prerequisites...)
	}
	out.Concepts = make([]CurriculumConcept, len(in.Concepts))
	for i := range in.Concepts {
		out.Concepts[i] = in.Concepts[i]
		out.Concepts[i].Outcomes = append([]CurriculumOutcome(nil), in.Concepts[i].Outcomes...)
		out.Concepts[i].Criteria = append([]CurriculumCriterion(nil), in.Concepts[i].Criteria...)
		out.Concepts[i].ReplacedBy = append([]string(nil), in.Concepts[i].ReplacedBy...)
	}
	out.Operation.SourceConceptIDs = append([]string(nil), in.Operation.SourceConceptIDs...)
	out.Operation.TargetConceptIDs = append([]string(nil), in.Operation.TargetConceptIDs...)
	return &out
}
