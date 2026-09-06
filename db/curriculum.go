// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// EnsureCurriculumBaseline lazily imports a pre-versioning domain into an
// immutable snapshot. New domains already have this row; the lazy path exists
// for installations upgraded from a schema that only stored graph_json.
func (s *Store) EnsureCurriculumBaseline(ctx context.Context, learnerID, domainID string) (*models.CurriculumSnapshot, error) {
	var result *models.CurriculumSnapshot
	err := s.inTx(ctx, nil, func(txs *Store) error {
		existing, err := txs.getCurriculumSnapshot(ctx, learnerID, domainID, 0)
		if err == nil {
			result = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		domain, err := txs.GetDomainByID(ctx, domainID)
		if err != nil || domain.LearnerID != learnerID {
			return fmt.Errorf("curriculum baseline: domain not found")
		}
		baseline, err := buildCurriculumBaseline(domain, models.CurriculumOperationBaselineImport, learnerID, time.Now().UTC())
		if err != nil {
			return err
		}
		inserted, err := txs.insertCurriculumSnapshot(ctx, learnerID, baseline, true)
		if err != nil {
			return err
		}
		if inserted {
			result = baseline
			return nil
		}
		// Another PostgreSQL transaction may have won the baseline race. Its
		// stable IDs are canonical; discard ours and return the committed row.
		result, err = txs.getCurriculumSnapshot(ctx, learnerID, domainID, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func buildCurriculumBaseline(domain *models.Domain, operation models.CurriculumOperationType, actor string, now time.Time) (*models.CurriculumSnapshot, error) {
	concepts := make([]models.CurriculumConcept, 0, len(domain.Graph.Concepts))
	for _, key := range domain.Graph.Concepts {
		concept, err := models.NewCurriculumConcept(key, key)
		if err != nil {
			return nil, err
		}
		concepts = append(concepts, concept)
	}
	return &models.CurriculumSnapshot{
		DomainID: domain.ID,
		Version:  domain.GraphVersion,
		Graph:    cloneKnowledgeSpace(domain.Graph),
		Concepts: concepts,
		Operation: models.CurriculumOperation{
			Type:      operation,
			Rationale: "establish immutable curriculum history from the active domain graph",
		},
		Provenance: models.CurriculumProvenance{
			SourceType: "domain_graph",
			Author:     actor,
			Rationale:  "baseline of the learner's declared curriculum",
		},
		Review:    models.CurriculumReview{Status: models.CurriculumReviewUnreviewed},
		CreatedBy: actor,
		CreatedAt: now,
	}, nil
}

func cloneKnowledgeSpace(graph models.KnowledgeSpace) models.KnowledgeSpace {
	out := models.KnowledgeSpace{
		Concepts:      append([]string(nil), graph.Concepts...),
		Prerequisites: make(map[string][]string, len(graph.Prerequisites)),
	}
	for concept, prerequisites := range graph.Prerequisites {
		out.Prerequisites[concept] = append([]string(nil), prerequisites...)
	}
	return out
}

// GetCurriculumSnapshot returns the requested immutable version. version=0
// selects the latest snapshot. Ownership is checked against the denormalized
// learner_id retained with the audit row, so history remains readable after a
// domain is tombstoned.
func (s *Store) GetCurriculumSnapshot(ctx context.Context, learnerID, domainID string, version int) (*models.CurriculumSnapshot, error) {
	return s.getCurriculumSnapshot(ctx, learnerID, domainID, version)
}

func (s *Store) getCurriculumSnapshot(ctx context.Context, learnerID, domainID string, version int) (*models.CurriculumSnapshot, error) {
	query := `SELECT snapshot_json FROM curriculum_versions WHERE learner_id = ? AND domain_id = ?`
	args := []any{learnerID, domainID}
	if version > 0 {
		query += ` AND version = ?`
		args = append(args, version)
	} else {
		query += ` ORDER BY version DESC LIMIT 1`
	}
	var raw string
	if err := s.queryRow(ctx, query, args...).Scan(&raw); err != nil {
		return nil, err
	}
	var snapshot models.CurriculumSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return nil, fmt.Errorf("decode curriculum snapshot: %w", err)
	}
	if err := validateCurriculumSnapshot(&snapshot); err != nil {
		return nil, fmt.Errorf("validate stored curriculum snapshot: %w", err)
	}
	return &snapshot, nil
}

func (s *Store) ListCurriculumSnapshots(ctx context.Context, learnerID, domainID string, limit int) ([]*models.CurriculumSnapshot, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.query(ctx, `SELECT snapshot_json FROM curriculum_versions
		WHERE learner_id = ? AND domain_id = ? ORDER BY version DESC LIMIT ?`, learnerID, domainID, limit)
	if err != nil {
		return nil, fmt.Errorf("list curriculum snapshots: %w", err)
	}
	defer rows.Close()

	result := make([]*models.CurriculumSnapshot, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan curriculum snapshot: %w", err)
		}
		var snapshot models.CurriculumSnapshot
		if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
			return nil, fmt.Errorf("decode curriculum snapshot: %w", err)
		}
		if err := validateCurriculumSnapshot(&snapshot); err != nil {
			return nil, fmt.Errorf("validate stored curriculum snapshot: %w", err)
		}
		result = append(result, &snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The bounded SQL query selects the latest versions efficiently; callers
	// receive them in chronological order for direct audit replay.
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result, nil
}

// CompareAndSwapCurriculum atomically publishes exactly one next version and
// updates domains.graph_json as its active compatibility projection. A stale
// writer receives ErrCurriculumVersionConflict; it never overwrites the winner.
func (s *Store) CompareAndSwapCurriculum(ctx context.Context, learnerID, domainID string, expectedVersion int, candidate *models.CurriculumSnapshot) error {
	if candidate == nil {
		return fmt.Errorf("curriculum snapshot is required")
	}
	if expectedVersion < 1 {
		return fmt.Errorf("expected_version must be >= 1")
	}
	next := models.CloneCurriculumSnapshot(candidate)
	next.DomainID = domainID
	next.ParentVersion = expectedVersion
	next.Version = expectedVersion + 1
	next.CreatedBy = learnerID
	next.CreatedAt = time.Now().UTC()
	if err := validateCurriculumSnapshot(next); err != nil {
		return err
	}

	return s.inTx(ctx, nil, func(txs *Store) error {
		// The parent snapshot is a lineage guard in addition to domains'
		// graph_version CAS. It prevents publishing on an un-audited legacy
		// version; callers must EnsureCurriculumBaseline first.
		previous, err := txs.getCurriculumSnapshot(ctx, learnerID, domainID, expectedVersion)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storeport.ErrCurriculumVersionConflict
			}
			return err
		}

		graphJSON, err := json.Marshal(next.Graph)
		if err != nil {
			return fmt.Errorf("marshal curriculum graph: %w", err)
		}
		res, err := txs.exec(ctx, `UPDATE domains
			SET graph_json = ?, graph_version = ?
			WHERE id = ? AND learner_id = ? AND deleted_at IS NULL AND graph_version = ?`,
			string(graphJSON), next.Version, domainID, learnerID, expectedVersion)
		if err != nil {
			return fmt.Errorf("compare-and-swap curriculum: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("compare-and-swap curriculum rows: %w", err)
		}
		if n != 1 {
			return storeport.ErrCurriculumVersionConflict
		}
		if err := txs.reconcileCurriculumLearning(ctx, learnerID, previous, next); err != nil {
			return err
		}
		if _, err := txs.insertCurriculumSnapshot(ctx, learnerID, next, false); err != nil {
			return err
		}
		return nil
	})
}

// insertCurriculumSnapshot stores the complete immutable envelope and registers
// every (id,key) identity. allowExisting is used only by concurrent legacy
// baseline import; normal revisions reject a duplicate version.
func (s *Store) insertCurriculumSnapshot(ctx context.Context, learnerID string, snapshot *models.CurriculumSnapshot, allowExisting bool) (bool, error) {
	if err := validateCurriculumSnapshot(snapshot); err != nil {
		return false, err
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return false, fmt.Errorf("marshal curriculum snapshot: %w", err)
	}
	operationJSON, err := json.Marshal(snapshot.Operation)
	if err != nil {
		return false, fmt.Errorf("marshal curriculum operation: %w", err)
	}
	provenanceJSON, err := json.Marshal(snapshot.Provenance)
	if err != nil {
		return false, fmt.Errorf("marshal curriculum provenance: %w", err)
	}
	reviewJSON, err := json.Marshal(snapshot.Review)
	if err != nil {
		return false, fmt.Errorf("marshal curriculum review: %w", err)
	}

	query := `INSERT INTO curriculum_versions
		(domain_id, learner_id, version, parent_version, snapshot_json, operation_type,
		 operation_json, provenance_json, review_json, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if allowExisting {
		query += ` ON CONFLICT (domain_id, version) DO NOTHING`
	}
	var parent any
	if snapshot.ParentVersion > 0 {
		parent = snapshot.ParentVersion
	}
	res, err := s.exec(ctx, query,
		snapshot.DomainID, learnerID, snapshot.Version, parent, string(snapshotJSON),
		string(snapshot.Operation.Type), string(operationJSON), string(provenanceJSON),
		string(reviewJSON), snapshot.CreatedBy, snapshot.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("insert curriculum snapshot: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert curriculum snapshot rows: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	for _, concept := range snapshot.Concepts {
		if _, err := s.exec(ctx, `INSERT INTO curriculum_concepts
			(id, domain_id, learner_id, stable_key, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO NOTHING`,
			concept.ID, snapshot.DomainID, learnerID, concept.Key, snapshot.CreatedAt); err != nil {
			return false, fmt.Errorf("register curriculum concept %q: %w", concept.ID, err)
		}
		var gotDomain, gotLearner, gotKey string
		if err := s.queryRow(ctx, `SELECT domain_id, learner_id, stable_key
			FROM curriculum_concepts WHERE id = ?`, concept.ID).Scan(&gotDomain, &gotLearner, &gotKey); err != nil {
			return false, fmt.Errorf("verify curriculum concept %q: %w", concept.ID, err)
		}
		if gotDomain != snapshot.DomainID || gotLearner != learnerID || gotKey != concept.Key {
			return false, fmt.Errorf("curriculum concept identity %q cannot change", concept.ID)
		}
	}
	// Outcome and criterion IDs are stable identities too. Their prose may
	// evolve in later snapshots, but an ID can never move to another concept or
	// switch kind after an item was retired and later reintroduced.
	for _, concept := range snapshot.Concepts {
		for _, outcome := range concept.Outcomes {
			if err := s.registerCurriculumMetadataID(ctx, learnerID, snapshot, concept.ID, outcome.ID, "outcome"); err != nil {
				return false, err
			}
		}
		for _, criterion := range concept.Criteria {
			if err := s.registerCurriculumMetadataID(ctx, learnerID, snapshot, concept.ID, criterion.ID, "criterion"); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func (s *Store) registerCurriculumMetadataID(ctx context.Context, learnerID string, snapshot *models.CurriculumSnapshot, conceptID, metadataID, kind string) error {
	if _, err := s.exec(ctx, `INSERT INTO curriculum_metadata_ids
		(id, concept_id, domain_id, learner_id, kind, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		metadataID, conceptID, snapshot.DomainID, learnerID, kind, snapshot.CreatedAt); err != nil {
		return fmt.Errorf("register curriculum %s %q: %w", kind, metadataID, err)
	}
	var gotConcept, gotDomain, gotLearner, gotKind string
	if err := s.queryRow(ctx, `SELECT concept_id, domain_id, learner_id, kind
		FROM curriculum_metadata_ids WHERE id = ?`, metadataID).
		Scan(&gotConcept, &gotDomain, &gotLearner, &gotKind); err != nil {
		return fmt.Errorf("verify curriculum %s %q: %w", kind, metadataID, err)
	}
	if gotConcept != conceptID || gotDomain != snapshot.DomainID || gotLearner != learnerID || gotKind != kind {
		return fmt.Errorf("curriculum metadata identity %q cannot change owner or kind", metadataID)
	}
	return nil
}

func validateCurriculumSnapshot(snapshot *models.CurriculumSnapshot) error {
	if snapshot.DomainID == "" || snapshot.Version < 1 {
		return fmt.Errorf("curriculum domain_id and positive version are required")
	}
	baselineImport := snapshot.Operation.Type == models.CurriculumOperationBaselineImport && snapshot.ParentVersion == 0
	if snapshot.Version > 1 && !baselineImport && snapshot.ParentVersion != snapshot.Version-1 {
		return fmt.Errorf("curriculum parent_version must immediately precede version")
	}
	if snapshot.Operation.Type == "" || snapshot.Operation.Rationale == "" {
		return fmt.Errorf("curriculum operation type and rationale are required")
	}
	if snapshot.Provenance.SourceType == "" || snapshot.Provenance.Author == "" || snapshot.Provenance.Rationale == "" {
		return fmt.Errorf("curriculum provenance source_type, author, and rationale are required")
	}
	if snapshot.Review.Status == "" {
		return fmt.Errorf("curriculum review status is required")
	}
	switch snapshot.Review.Status {
	case models.CurriculumReviewUnreviewed, models.CurriculumReviewInReview,
		models.CurriculumReviewApproved, models.CurriculumReviewRejected:
	default:
		return fmt.Errorf("invalid curriculum review status %q", snapshot.Review.Status)
	}
	switch snapshot.Operation.Type {
	case models.CurriculumOperationCreate, models.CurriculumOperationBaselineImport,
		models.CurriculumOperationAdd, models.CurriculumOperationRename,
		models.CurriculumOperationUpdateMetadata, models.CurriculumOperationSplit,
		models.CurriculumOperationRepairPrerequisites,
		models.CurriculumOperationMerge, models.CurriculumOperationRemove,
		models.CurriculumOperationLegacyUpdate:
	default:
		return fmt.Errorf("invalid curriculum operation %q", snapshot.Operation.Type)
	}

	activeKeys := make(map[string]bool)
	ids := make(map[string]bool)
	allKeys := make(map[string]bool)
	metadataIDs := make(map[string]bool)
	for _, concept := range snapshot.Concepts {
		if concept.ID == "" || concept.Key == "" || concept.Label == "" {
			return fmt.Errorf("curriculum concepts require id, immutable key, and label")
		}
		if ids[concept.ID] || allKeys[concept.Key] {
			return fmt.Errorf("duplicate curriculum concept identity")
		}
		ids[concept.ID] = true
		allKeys[concept.Key] = true
		switch concept.Status {
		case models.CurriculumConceptActive:
			activeKeys[concept.Key] = true
		case models.CurriculumConceptRetired:
		default:
			return fmt.Errorf("invalid curriculum concept status %q", concept.Status)
		}
		if !validCurriculumLevel(concept.Level) {
			return fmt.Errorf("invalid curriculum level %q", concept.Level)
		}
		if err := validateCurriculumMetadataIDs(concept); err != nil {
			return err
		}
		for _, outcome := range concept.Outcomes {
			if metadataIDs[outcome.ID] {
				return fmt.Errorf("curriculum metadata identity %q is reused", outcome.ID)
			}
			metadataIDs[outcome.ID] = true
		}
		for _, criterion := range concept.Criteria {
			if metadataIDs[criterion.ID] {
				return fmt.Errorf("curriculum metadata identity %q is reused", criterion.ID)
			}
			metadataIDs[criterion.ID] = true
		}
	}

	graphKeys := make(map[string]bool, len(snapshot.Graph.Concepts))
	for _, key := range snapshot.Graph.Concepts {
		if graphKeys[key] || !activeKeys[key] {
			return fmt.Errorf("curriculum graph must contain each active concept key exactly once")
		}
		graphKeys[key] = true
	}
	if len(graphKeys) != len(activeKeys) {
		return fmt.Errorf("curriculum graph and active concepts differ")
	}
	for dependent, prerequisites := range snapshot.Graph.Prerequisites {
		if !graphKeys[dependent] {
			return fmt.Errorf("curriculum prerequisite key %q is not active", dependent)
		}
		seen := make(map[string]bool)
		for _, prerequisite := range prerequisites {
			if !graphKeys[prerequisite] || seen[prerequisite] {
				return fmt.Errorf("invalid curriculum prerequisite %q for %q", prerequisite, dependent)
			}
			seen[prerequisite] = true
		}
	}
	if curriculumGraphHasCycle(snapshot.Graph) {
		return fmt.Errorf("curriculum prerequisite graph contains a cycle")
	}
	return nil
}

func curriculumGraphHasCycle(graph models.KnowledgeSpace) bool {
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := make(map[string]int, len(graph.Concepts))
	var visit func(string) bool
	visit = func(concept string) bool {
		switch state[concept] {
		case visiting:
			return true
		case visited:
			return false
		}
		state[concept] = visiting
		for _, prerequisite := range graph.Prerequisites[concept] {
			if visit(prerequisite) {
				return true
			}
		}
		state[concept] = visited
		return false
	}
	for _, concept := range graph.Concepts {
		if visit(concept) {
			return true
		}
	}
	return false
}

func validCurriculumLevel(level models.CurriculumLevel) bool {
	switch level {
	case models.CurriculumLevelUnspecified, models.CurriculumLevelFoundation,
		models.CurriculumLevelIntermediate, models.CurriculumLevelAdvanced:
		return true
	default:
		return false
	}
}

func validateCurriculumMetadataIDs(concept models.CurriculumConcept) error {
	outcomes := make(map[string]bool)
	for _, outcome := range concept.Outcomes {
		if outcome.ID == "" || outcome.Statement == "" || outcomes[outcome.ID] {
			return fmt.Errorf("concept %q has invalid or duplicate outcome metadata", concept.ID)
		}
		outcomes[outcome.ID] = true
	}
	criteria := make(map[string]bool)
	for _, criterion := range concept.Criteria {
		if criterion.ID == "" || criterion.Description == "" || criteria[criterion.ID] {
			return fmt.Errorf("concept %q has invalid or duplicate criterion metadata", concept.ID)
		}
		criteria[criterion.ID] = true
	}
	return nil
}

// reconcileLegacyGraph converts the compatibility UpdateDomainGraph API into
// a fully audited CAS revision. New keys receive identities; missing active
// keys are retired rather than erased.
func reconcileLegacyGraph(current *models.CurriculumSnapshot, graph models.KnowledgeSpace, actor string) (*models.CurriculumSnapshot, error) {
	next := models.CloneCurriculumSnapshot(current)
	next.Graph = cloneKnowledgeSpace(graph)
	wanted := make(map[string]bool, len(graph.Concepts))
	for _, key := range graph.Concepts {
		wanted[key] = true
	}
	existing := make(map[string]bool, len(next.Concepts))
	var sources, targets []string
	for i := range next.Concepts {
		existing[next.Concepts[i].Key] = true
		if next.Concepts[i].Status == models.CurriculumConceptActive && !wanted[next.Concepts[i].Key] {
			next.Concepts[i].Status = models.CurriculumConceptRetired
			sources = append(sources, next.Concepts[i].ID)
		}
	}
	for _, key := range graph.Concepts {
		if existing[key] {
			for i := range next.Concepts {
				if next.Concepts[i].Key == key {
					next.Concepts[i].Status = models.CurriculumConceptActive
					next.Concepts[i].ReplacedBy = nil
				}
			}
			continue
		}
		concept, err := models.NewCurriculumConcept(key, key)
		if err != nil {
			return nil, err
		}
		next.Concepts = append(next.Concepts, concept)
		targets = append(targets, concept.ID)
	}
	sort.Strings(sources)
	sort.Strings(targets)
	next.Operation = models.CurriculumOperation{
		Type:             models.CurriculumOperationLegacyUpdate,
		SourceConceptIDs: sources,
		TargetConceptIDs: targets,
		Rationale:        "compatibility graph update captured as an immutable curriculum revision",
	}
	next.Provenance = models.CurriculumProvenance{
		SourceType: "legacy_store_api",
		Author:     actor,
		Rationale:  "preserve audit history for a legacy graph mutation",
	}
	next.Review = models.CurriculumReview{Status: models.CurriculumReviewUnreviewed}
	return next, nil
}
