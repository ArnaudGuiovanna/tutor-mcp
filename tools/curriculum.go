// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxCurriculumDescriptionLen = 4000
	maxCurriculumOutcomeLen     = 2000
	maxCurriculumCriteriaLen    = 2000
	maxCurriculumMetadataItems  = 50
)

type CurriculumOutcomeInput struct {
	ID         string `json:"id,omitempty" jsonschema:"stable outcome ID when updating an existing outcome; omit to allocate one"`
	Statement  string `json:"statement" jsonschema:"observable learner capability"`
	Observable string `json:"observable,omitempty" jsonschema:"how the capability can be observed"`
}

type CurriculumCriterionInput struct {
	ID          string `json:"id,omitempty" jsonschema:"stable criterion ID when updating an existing criterion; omit to allocate one"`
	Description string `json:"description" jsonschema:"criterion for acceptable evidence"`
	Evidence    string `json:"evidence,omitempty" jsonschema:"expected evidence form"`
}

type CurriculumConceptDefinition struct {
	Label       string                     `json:"label" jsonschema:"learner-facing competency label"`
	Description string                     `json:"description,omitempty" jsonschema:"bounded competency description"`
	Level       string                     `json:"level,omitempty" jsonschema:"unspecified | foundation | intermediate | advanced"`
	Outcomes    []CurriculumOutcomeInput   `json:"outcomes,omitempty" jsonschema:"observable learning outcomes"`
	Criteria    []CurriculumCriterionInput `json:"criteria,omitempty" jsonschema:"evidence acceptance criteria"`
}

type CurriculumProvenanceInput struct {
	SourceType string `json:"source_type" jsonschema:"source category, for example learner, expert, standard, institution, or imported_material"`
	SourceRef  string `json:"source_ref,omitempty" jsonschema:"source URL, document ID, or citation"`
	Rationale  string `json:"rationale" jsonschema:"why this revision improves the learner curriculum"`
}

type CurriculumReviewInput struct {
	Status     string `json:"status,omitempty" jsonschema:"unreviewed | in_review; approval requires a trusted operator boundary"`
	ReviewerID string `json:"reviewer_id,omitempty" jsonschema:"reviewer identity when review has started"`
	Notes      string `json:"notes,omitempty" jsonschema:"review notes"`
}

type GetCurriculumSnapshotParams struct {
	DomainID       string `json:"domain_id" jsonschema:"target domain ID"`
	Version        int    `json:"version,omitempty" jsonschema:"immutable version to read; omit/zero for latest"`
	IncludeHistory bool   `json:"include_history,omitempty" jsonschema:"include every immutable version envelope"`
	HistoryLimit   int    `json:"history_limit,omitempty" jsonschema:"maximum recent versions returned with history (default 50, max 200)"`
}

func registerGetCurriculumSnapshot(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_curriculum_snapshot",
		Description: "Read the latest or a historical immutable curriculum snapshot, including stable concept IDs, outcomes, criteria, provenance, and review state.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetCurriculumSnapshotParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_curriculum_snapshot", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.DomainID == "" || params.Version < 0 || params.HistoryLimit < 0 || params.HistoryLimit > 200 {
			r, _ := errorResult("domain_id is required; version/history_limit cannot be negative and history_limit cannot exceed 200")
			return r, nil, nil
		}
		// The audit table retains learner ownership after a domain tombstone, so
		// an owner can still read preserved history. Active legacy domains with no
		// snapshot yet fall through to a one-time baseline import.
		snapshot, err := deps.Store.GetCurriculumSnapshot(ctx, learnerID, params.DomainID, params.Version)
		if err != nil {
			domain, resolveErr := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
			if resolveErr != nil {
				r, _ := errorResult("curriculum version not found")
				return r, nil, nil
			}
			if _, ensureErr := deps.Store.EnsureCurriculumBaseline(ctx, learnerID, domain.ID); ensureErr != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to initialize curriculum history", ensureErr)
				return r, nil, nil
			}
			snapshot, err = deps.Store.GetCurriculumSnapshot(ctx, learnerID, domain.ID, params.Version)
			if err != nil {
				r, _ := errorResult("curriculum version not found")
				return r, nil, nil
			}
		}
		payload := map[string]any{"snapshot": snapshot}
		if params.IncludeHistory {
			history, err := deps.Store.ListCurriculumSnapshots(ctx, learnerID, params.DomainID, params.HistoryLimit)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to list curriculum history", err)
				return r, nil, nil
			}
			payload["history"] = history
		}
		r, _ := jsonResult(payload)
		return r, nil, nil
	})
}

type ReviseCurriculumParams struct {
	IdempotentMutationParams
	DomainID         string                        `json:"domain_id" jsonschema:"target domain ID"`
	ExpectedVersion  int                           `json:"expected_version" jsonschema:"current graph_version; stale revisions are rejected"`
	Operation        string                        `json:"operation" jsonschema:"rename | update_metadata | split | merge | remove"`
	SourceConceptIDs []string                      `json:"source_concept_ids" jsonschema:"stable concept IDs affected by the operation"`
	NewLabel         string                        `json:"new_label,omitempty" jsonschema:"new display label for rename"`
	Metadata         *CurriculumConceptDefinition  `json:"metadata,omitempty" jsonschema:"replacement metadata for update_metadata"`
	NewConcepts      []CurriculumConceptDefinition `json:"new_concepts,omitempty" jsonschema:"new competencies created by split or merge"`
	Provenance       CurriculumProvenanceInput     `json:"provenance" jsonschema:"source and rationale for this revision"`
	Review           *CurriculumReviewInput        `json:"review,omitempty" jsonschema:"optional unreviewed/in-review annotation"`
}

func registerReviseCurriculum(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "publish_curriculum_revision",
		Description: "Publish one immutable curriculum revision using optimistic concurrency. Supports rename, metadata update, split, merge, and safe leaf removal; stable IDs and all prior versions are preserved.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params ReviseCurriculumParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "publish_curriculum_revision", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.DomainID == "" || params.ExpectedVersion < 1 {
			r, _ := errorResult("domain_id and expected_version >= 1 are required")
			return r, nil, nil
		}
		if err := validateCurriculumProvenanceInput(params.Provenance); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil {
			r, _ := errorResult("domain not found")
			return r, nil, nil
		}
		current, err := deps.Store.EnsureCurriculumBaseline(ctx, learnerID, domain.ID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to initialize curriculum history", err)
			return r, nil, nil
		}
		if current.Version != params.ExpectedVersion || domain.GraphVersion != params.ExpectedVersion {
			r, _ := curriculumConflictResult(params.ExpectedVersion, domain.GraphVersion)
			return r, nil, nil
		}

		next, newKeys, err := buildCurriculumRevision(current, params, learnerID)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateConcepts(next.Graph.Concepts, next.Graph.Prerequisites); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		err = deps.Store.WithTx(ctx, func(tx storeport.Store) error {
			if err := tx.CompareAndSwapCurriculum(ctx, learnerID, domain.ID, params.ExpectedVersion, next); err != nil {
				return err
			}
			for _, key := range newKeys {
				if err := tx.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(learnerID, domain.ID, key)); err != nil {
					return fmt.Errorf("initialize concept %q: %w", key, err)
				}
			}
			return nil
		})
		if errors.Is(err, storeport.ErrCurriculumVersionConflict) {
			fresh, _ := deps.Store.GetDomainByID(ctx, domain.ID)
			currentVersion := domain.GraphVersion
			if fresh != nil {
				currentVersion = fresh.GraphVersion
			}
			r, _ := curriculumConflictResult(params.ExpectedVersion, currentVersion)
			return r, nil, nil
		}
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to publish curriculum revision", err)
			return r, nil, nil
		}
		published, err := deps.Store.GetCurriculumSnapshot(ctx, learnerID, domain.ID, params.ExpectedVersion+1)
		if err != nil {
			// The CAS and all new concept states committed in one transaction.
			// A readback failure cannot safely be represented as an MCP error:
			// aborting the idempotency reservation would make a retry collide with
			// the newly published version. Return the known commit metadata and
			// require an explicit get_curriculum_snapshot read to recover the
			// immutable envelope.
			deps.Logger.Warn("publish_curriculum_revision: committed snapshot readback degraded", "err", err, "learner", learnerID, "domain", domain.ID, "version", params.ExpectedVersion+1)
			r, _ := jsonResult(map[string]any{
				"domain_id":        domain.ID,
				"previous_version": params.ExpectedVersion,
				"version":          params.ExpectedVersion + 1,
				"operation":        next.Operation,
				"snapshot":         nil,
				"snapshot_status":  "committed_readback_unavailable",
				"degraded_components": []string{
					"curriculum_snapshot_readback",
				},
			})
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{
			"domain_id":        domain.ID,
			"previous_version": params.ExpectedVersion,
			"version":          published.Version,
			"operation":        published.Operation,
			"snapshot":         published,
		})
		return r, nil, nil
	})
}

func curriculumConflictResult(expected, current int) (*mcp.CallToolResult, any) {
	return errorResult(fmt.Sprintf("curriculum version conflict: expected_version=%d current_version=%d; reload get_curriculum_snapshot and rebase", expected, current))
}

func validateCurriculumProvenanceInput(in CurriculumProvenanceInput) error {
	if strings.TrimSpace(in.SourceType) == "" || strings.TrimSpace(in.Rationale) == "" {
		return fmt.Errorf("provenance.source_type and provenance.rationale are required")
	}
	if len(in.SourceType) > maxShortLabelLen || len(in.SourceRef) > maxCurriculumDescriptionLen || len(in.Rationale) > maxCurriculumDescriptionLen {
		return fmt.Errorf("curriculum provenance exceeds length limits")
	}
	return nil
}

func buildCurriculumRevision(current *models.CurriculumSnapshot, params ReviseCurriculumParams, actor string) (*models.CurriculumSnapshot, []string, error) {
	if len(params.SourceConceptIDs) > maxConceptsPerCall || len(params.NewConcepts) > maxConceptsPerCall {
		return nil, nil, fmt.Errorf("curriculum revision exceeds the %d-concept limit", maxConceptsPerCall)
	}
	next := models.CloneCurriculumSnapshot(current)
	next.Provenance = models.CurriculumProvenance{
		SourceType: strings.TrimSpace(params.Provenance.SourceType),
		SourceRef:  strings.TrimSpace(params.Provenance.SourceRef),
		Author:     actor,
		Rationale:  strings.TrimSpace(params.Provenance.Rationale),
	}
	review, err := curriculumReviewFromInput(params.Review)
	if err != nil {
		return nil, nil, err
	}
	next.Review = review

	sources, err := activeCurriculumSources(next, params.SourceConceptIDs)
	if err != nil {
		return nil, nil, err
	}
	var newKeys []string
	switch models.CurriculumOperationType(params.Operation) {
	case models.CurriculumOperationRename:
		if len(sources) != 1 || strings.TrimSpace(params.NewLabel) == "" {
			return nil, nil, fmt.Errorf("rename requires exactly one source_concept_id and new_label")
		}
		if len(params.NewLabel) > maxConceptNameLen {
			return nil, nil, fmt.Errorf("new_label too long")
		}
		if curriculumLabelInUse(next, params.NewLabel, sources[0].ID) {
			return nil, nil, fmt.Errorf("curriculum label %q is already active", params.NewLabel)
		}
		for i := range next.Concepts {
			if next.Concepts[i].ID == sources[0].ID {
				next.Concepts[i].Label = strings.TrimSpace(params.NewLabel)
			}
		}
	case models.CurriculumOperationUpdateMetadata:
		if len(sources) != 1 || params.Metadata == nil {
			return nil, nil, fmt.Errorf("update_metadata requires exactly one source_concept_id and metadata")
		}
		if params.Metadata.Label != "" && strings.TrimSpace(params.Metadata.Label) != sources[0].Label {
			return nil, nil, fmt.Errorf("update_metadata cannot rename a concept; use the rename operation")
		}
		for i := range next.Concepts {
			if next.Concepts[i].ID == sources[0].ID {
				if err := applyCurriculumDefinition(&next.Concepts[i], *params.Metadata, false); err != nil {
					return nil, nil, err
				}
			}
		}
	case models.CurriculumOperationSplit:
		if len(sources) != 1 || len(params.NewConcepts) < 2 {
			return nil, nil, fmt.Errorf("split requires exactly one source_concept_id and at least two new_concepts")
		}
		targets, err := newCurriculumConcepts(next, params.NewConcepts)
		if err != nil {
			return nil, nil, err
		}
		next.Concepts = append(next.Concepts, targets...)
		ids := curriculumConceptIDs(targets)
		retireCurriculumConcepts(next, []models.CurriculumConcept{sources[0]}, ids)
		next.Graph = splitCurriculumGraph(next.Graph, sources[0].Key, curriculumConceptKeys(targets))
		newKeys = curriculumConceptKeys(targets)
	case models.CurriculumOperationMerge:
		if len(sources) < 2 || len(params.NewConcepts) != 1 {
			return nil, nil, fmt.Errorf("merge requires at least two source_concept_ids and exactly one new_concept")
		}
		targets, err := newCurriculumConcepts(next, params.NewConcepts)
		if err != nil {
			return nil, nil, err
		}
		next.Concepts = append(next.Concepts, targets[0])
		retireCurriculumConcepts(next, sources, []string{targets[0].ID})
		next.Graph = mergeCurriculumGraph(next.Graph, curriculumConceptKeys(sources), targets[0].Key)
		newKeys = []string{targets[0].Key}
	case models.CurriculumOperationRemove:
		if len(sources) == 0 {
			return nil, nil, fmt.Errorf("remove requires at least one source_concept_id")
		}
		if len(next.Graph.Concepts) == len(sources) {
			return nil, nil, fmt.Errorf("cannot remove every active curriculum concept")
		}
		keys := curriculumConceptKeys(sources)
		if dependent, source := activeDependentOutsideSet(next.Graph, keys); dependent != "" {
			return nil, nil, fmt.Errorf("cannot remove concept %q while active concept %q depends on it; split or merge it first", source, dependent)
		}
		retireCurriculumConcepts(next, sources, nil)
		next.Graph = removeCurriculumGraphKeys(next.Graph, keys)
	default:
		return nil, nil, fmt.Errorf("operation must be rename, update_metadata, split, merge, or remove")
	}

	targetIDs := make([]string, 0)
	for _, key := range newKeys {
		for _, concept := range next.Concepts {
			if concept.Key == key {
				targetIDs = append(targetIDs, concept.ID)
			}
		}
	}
	next.Operation = models.CurriculumOperation{
		Type:             models.CurriculumOperationType(params.Operation),
		SourceConceptIDs: curriculumConceptIDs(sources),
		TargetConceptIDs: targetIDs,
		Rationale:        strings.TrimSpace(params.Provenance.Rationale),
	}
	return next, newKeys, nil
}

func curriculumReviewFromInput(in *CurriculumReviewInput) (models.CurriculumReview, error) {
	if in == nil || in.Status == "" || in.Status == string(models.CurriculumReviewUnreviewed) {
		return models.CurriculumReview{Status: models.CurriculumReviewUnreviewed}, nil
	}
	if in.Status != string(models.CurriculumReviewInReview) {
		return models.CurriculumReview{}, fmt.Errorf("public curriculum revisions may only be unreviewed or in_review; approval requires a trusted operator")
	}
	if strings.TrimSpace(in.ReviewerID) == "" {
		return models.CurriculumReview{}, fmt.Errorf("reviewer_id is required for in_review")
	}
	if len(in.ReviewerID) > maxShortLabelLen || len(in.Notes) > maxCurriculumDescriptionLen {
		return models.CurriculumReview{}, fmt.Errorf("curriculum review exceeds length limits")
	}
	now := time.Now().UTC()
	return models.CurriculumReview{
		Status:     models.CurriculumReviewInReview,
		ReviewerID: strings.TrimSpace(in.ReviewerID),
		Notes:      strings.TrimSpace(in.Notes),
		ReviewedAt: &now,
	}, nil
}

func activeCurriculumSources(snapshot *models.CurriculumSnapshot, ids []string) ([]models.CurriculumConcept, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || wanted[id] {
			return nil, fmt.Errorf("source_concept_ids must be non-empty and unique")
		}
		wanted[id] = true
	}
	result := make([]models.CurriculumConcept, 0, len(ids))
	for _, id := range ids {
		found := false
		for _, concept := range snapshot.Concepts {
			if concept.ID == id && concept.Status == models.CurriculumConceptActive {
				result = append(result, concept)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("active source concept %q not found", id)
		}
	}
	return result, nil
}

func newCurriculumConcepts(snapshot *models.CurriculumSnapshot, defs []CurriculumConceptDefinition) ([]models.CurriculumConcept, error) {
	keys := make(map[string]bool, len(snapshot.Concepts)+len(defs))
	labels := make(map[string]bool, len(snapshot.Concepts)+len(defs))
	for _, concept := range snapshot.Concepts {
		keys[concept.Key] = true
		if concept.Status == models.CurriculumConceptActive {
			labels[strings.ToLower(strings.TrimSpace(concept.Label))] = true
		}
	}
	result := make([]models.CurriculumConcept, 0, len(defs))
	for _, def := range defs {
		key := strings.TrimSpace(def.Label)
		if key == "" || len(key) > maxConceptNameLen || keys[key] {
			return nil, fmt.Errorf("each new concept needs a unique label/key of at most %d characters", maxConceptNameLen)
		}
		labelKey := strings.ToLower(key)
		if labels[labelKey] {
			return nil, fmt.Errorf("curriculum label %q is already active", key)
		}
		concept, err := models.NewCurriculumConcept(key, key)
		if err != nil {
			return nil, err
		}
		if err := applyCurriculumDefinition(&concept, def, true); err != nil {
			return nil, err
		}
		keys[key] = true
		labels[labelKey] = true
		result = append(result, concept)
	}
	return result, nil
}

func applyCurriculumDefinition(concept *models.CurriculumConcept, def CurriculumConceptDefinition, requireLabel bool) error {
	existingOutcomeIDs := make(map[string]bool, len(concept.Outcomes))
	for _, outcome := range concept.Outcomes {
		existingOutcomeIDs[outcome.ID] = true
	}
	existingCriterionIDs := make(map[string]bool, len(concept.Criteria))
	for _, criterion := range concept.Criteria {
		existingCriterionIDs[criterion.ID] = true
	}
	if requireLabel && strings.TrimSpace(def.Label) == "" {
		return fmt.Errorf("new curriculum concept label is required")
	}
	if def.Label != "" {
		if len(def.Label) > maxConceptNameLen {
			return fmt.Errorf("curriculum concept label too long")
		}
		concept.Label = strings.TrimSpace(def.Label)
	}
	if len(def.Description) > maxCurriculumDescriptionLen || len(def.Outcomes) > maxCurriculumMetadataItems || len(def.Criteria) > maxCurriculumMetadataItems {
		return fmt.Errorf("curriculum concept metadata exceeds limits")
	}
	if requireLabel || def.Description != "" {
		concept.Description = strings.TrimSpace(def.Description)
	}
	if requireLabel || def.Level != "" {
		level := models.CurriculumLevel(def.Level)
		if level == "" {
			level = models.CurriculumLevelUnspecified
		}
		switch level {
		case models.CurriculumLevelUnspecified, models.CurriculumLevelFoundation,
			models.CurriculumLevelIntermediate, models.CurriculumLevelAdvanced:
			concept.Level = level
		default:
			return fmt.Errorf("invalid curriculum level %q", def.Level)
		}
	}

	if requireLabel || def.Outcomes != nil {
		concept.Outcomes = make([]models.CurriculumOutcome, 0, len(def.Outcomes))
		seenOutcome := make(map[string]bool)
		for _, in := range def.Outcomes {
			if strings.TrimSpace(in.Statement) == "" || len(in.Statement) > maxCurriculumOutcomeLen || len(in.Observable) > maxCurriculumOutcomeLen {
				return fmt.Errorf("each curriculum outcome needs a bounded statement")
			}
			id := strings.TrimSpace(in.ID)
			if id == "" {
				var err error
				id, err = models.NewCurriculumStableID("outcome")
				if err != nil {
					return err
				}
			} else if requireLabel || !existingOutcomeIDs[id] {
				return fmt.Errorf("outcome ID %q does not belong to this existing concept; omit id to create a new outcome", id)
			}
			if len(id) > maxShortLabelLen || seenOutcome[id] {
				return fmt.Errorf("curriculum outcome IDs must be unique and bounded")
			}
			seenOutcome[id] = true
			concept.Outcomes = append(concept.Outcomes, models.CurriculumOutcome{ID: id, Statement: strings.TrimSpace(in.Statement), Observable: strings.TrimSpace(in.Observable)})
		}
	}

	if requireLabel || def.Criteria != nil {
		concept.Criteria = make([]models.CurriculumCriterion, 0, len(def.Criteria))
		seenCriterion := make(map[string]bool)
		for _, in := range def.Criteria {
			if strings.TrimSpace(in.Description) == "" || len(in.Description) > maxCurriculumCriteriaLen || len(in.Evidence) > maxCurriculumCriteriaLen {
				return fmt.Errorf("each curriculum criterion needs a bounded description")
			}
			id := strings.TrimSpace(in.ID)
			if id == "" {
				var err error
				id, err = models.NewCurriculumStableID("criterion")
				if err != nil {
					return err
				}
			} else if requireLabel || !existingCriterionIDs[id] {
				return fmt.Errorf("criterion ID %q does not belong to this existing concept; omit id to create a new criterion", id)
			}
			if len(id) > maxShortLabelLen || seenCriterion[id] {
				return fmt.Errorf("curriculum criterion IDs must be unique and bounded")
			}
			seenCriterion[id] = true
			concept.Criteria = append(concept.Criteria, models.CurriculumCriterion{ID: id, Description: strings.TrimSpace(in.Description), Evidence: strings.TrimSpace(in.Evidence)})
		}
	}
	return nil
}

func retireCurriculumConcepts(snapshot *models.CurriculumSnapshot, sources []models.CurriculumConcept, replacementIDs []string) {
	sourceSet := make(map[string]bool, len(sources))
	for _, source := range sources {
		sourceSet[source.ID] = true
	}
	for i := range snapshot.Concepts {
		if sourceSet[snapshot.Concepts[i].ID] {
			snapshot.Concepts[i].Status = models.CurriculumConceptRetired
			snapshot.Concepts[i].ReplacedBy = append([]string(nil), replacementIDs...)
		}
	}
}

func splitCurriculumGraph(graph models.KnowledgeSpace, source string, targets []string) models.KnowledgeSpace {
	out := models.KnowledgeSpace{Prerequisites: make(map[string][]string)}
	for _, key := range graph.Concepts {
		if key == source {
			out.Concepts = append(out.Concepts, targets...)
		} else {
			out.Concepts = append(out.Concepts, key)
		}
	}
	inherited := append([]string(nil), graph.Prerequisites[source]...)
	for dependent, prerequisites := range graph.Prerequisites {
		if dependent == source {
			continue
		}
		out.Prerequisites[dependent] = replacePrerequisites(prerequisites, map[string]bool{source: true}, targets)
	}
	for _, target := range targets {
		out.Prerequisites[target] = append([]string(nil), inherited...)
	}
	return out
}

func mergeCurriculumGraph(graph models.KnowledgeSpace, sources []string, target string) models.KnowledgeSpace {
	sourceSet := stringSet(sources)
	out := models.KnowledgeSpace{Prerequisites: make(map[string][]string)}
	inserted := false
	for _, key := range graph.Concepts {
		if sourceSet[key] {
			if !inserted {
				out.Concepts = append(out.Concepts, target)
				inserted = true
			}
			continue
		}
		out.Concepts = append(out.Concepts, key)
	}
	var inherited []string
	for _, source := range sources {
		for _, prerequisite := range graph.Prerequisites[source] {
			if !sourceSet[prerequisite] {
				inherited = append(inherited, prerequisite)
			}
		}
	}
	inherited = uniqueStrings(inherited)
	for dependent, prerequisites := range graph.Prerequisites {
		if sourceSet[dependent] {
			continue
		}
		out.Prerequisites[dependent] = replacePrerequisites(prerequisites, sourceSet, []string{target})
	}
	out.Prerequisites[target] = inherited
	return out
}

func removeCurriculumGraphKeys(graph models.KnowledgeSpace, keys []string) models.KnowledgeSpace {
	removed := stringSet(keys)
	out := models.KnowledgeSpace{Prerequisites: make(map[string][]string)}
	for _, key := range graph.Concepts {
		if !removed[key] {
			out.Concepts = append(out.Concepts, key)
		}
	}
	for dependent, prerequisites := range graph.Prerequisites {
		if !removed[dependent] {
			out.Prerequisites[dependent] = replacePrerequisites(prerequisites, removed, nil)
		}
	}
	return out
}

func activeDependentOutsideSet(graph models.KnowledgeSpace, keys []string) (dependent, source string) {
	removed := stringSet(keys)
	for _, candidate := range graph.Concepts {
		if removed[candidate] {
			continue
		}
		for _, prerequisite := range graph.Prerequisites[candidate] {
			if removed[prerequisite] {
				return candidate, prerequisite
			}
		}
	}
	return "", ""
}

func replacePrerequisites(prerequisites []string, removed map[string]bool, replacements []string) []string {
	result := make([]string, 0, len(prerequisites)+len(replacements))
	replaced := false
	for _, prerequisite := range prerequisites {
		if removed[prerequisite] {
			replaced = true
			continue
		}
		result = append(result, prerequisite)
	}
	if replaced {
		result = append(result, replacements...)
	}
	return uniqueStrings(result)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func curriculumConceptIDs(concepts []models.CurriculumConcept) []string {
	result := make([]string, 0, len(concepts))
	for _, concept := range concepts {
		result = append(result, concept.ID)
	}
	return result
}

func curriculumConceptKeys(concepts []models.CurriculumConcept) []string {
	result := make([]string, 0, len(concepts))
	for _, concept := range concepts {
		result = append(result, concept.Key)
	}
	return result
}

func curriculumLabelInUse(snapshot *models.CurriculumSnapshot, label, exceptID string) bool {
	needle := strings.ToLower(strings.TrimSpace(label))
	for _, concept := range snapshot.Concepts {
		if concept.Status == models.CurriculumConceptActive && concept.ID != exceptID && strings.ToLower(strings.TrimSpace(concept.Label)) == needle {
			return true
		}
	}
	return false
}
