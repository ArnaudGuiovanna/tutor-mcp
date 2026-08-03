// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestPublishCurriculumRevision_RenamePreservesIdentityAndRejectsStaleWriter(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "Mathematics")
	baseline, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 1)
	if err != nil {
		t.Fatalf("get baseline: %v", err)
	}
	concept := baseline.Concepts[0]

	args := map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   1,
		"operation":          "rename",
		"source_concept_ids": []string{concept.ID},
		"new_label":          "Foundational reasoning",
		"provenance": map[string]any{
			"source_type": "expert",
			"source_ref":  "curriculum-board-2026",
			"rationale":   "use an observable, domain-appropriate competency label",
		},
		"review": map[string]any{
			"status":      "in_review",
			"reviewer_id": "curriculum-board",
			"notes":       "terminology review opened; approval is not self-asserted",
		},
	}
	res := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", args)
	if res.IsError {
		t.Fatalf("publish rename: %q", resultText(res))
	}
	published, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 2)
	if err != nil {
		t.Fatalf("get published snapshot: %v", err)
	}
	if published.Concepts[0].ID != concept.ID || published.Concepts[0].Key != concept.Key {
		t.Fatalf("rename changed stable identity: before=%+v after=%+v", concept, published.Concepts[0])
	}
	if published.Concepts[0].Label != "Foundational reasoning" || published.Graph.Concepts[0] != concept.Key {
		t.Fatalf("rename did not preserve key/update label: %+v graph=%v", published.Concepts[0], published.Graph.Concepts)
	}
	if published.Review.Status != models.CurriculumReviewInReview || published.Review.ReviewerID != "curriculum-board" || published.Review.ReviewedAt == nil {
		t.Fatalf("review provenance was not persisted: %+v", published.Review)
	}

	stale := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", args)
	if !stale.IsError || !strings.Contains(resultText(stale), "version conflict") || !strings.Contains(resultText(stale), "current_version=2") {
		t.Fatalf("stale writer was not given an actionable conflict: %q", resultText(stale))
	}
	history, err := store.ListCurriculumSnapshots(context.Background(), "L_owner", domain.ID, 50)
	if err != nil || len(history) != 2 {
		t.Fatalf("stale writer altered history: len=%d err=%v", len(history), err)
	}
}

func TestPublishCurriculumRevision_SplitMergeAndSafeRemovalAreAuditable(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "Science") // a -> b
	baseline, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	var sourceB models.CurriculumConcept
	for _, concept := range baseline.Concepts {
		if concept.Key == "b" {
			sourceB = concept
		}
	}

	split := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   1,
		"operation":          "split",
		"source_concept_ids": []string{sourceB.ID},
		"new_concepts": []map[string]any{
			{
				"label":       "b-model",
				"description": "Build a model.",
				"level":       "intermediate",
				"outcomes": []map[string]any{{
					"statement":  "Construct a valid domain model",
					"observable": "an independently produced model",
				}},
				"criteria": []map[string]any{{
					"description": "The model explains the observed relationships",
					"evidence":    "model plus justification",
				}},
			},
			{
				"label":    "b-test",
				"level":    "intermediate",
				"outcomes": []map[string]any{{"statement": "Test the model against evidence"}},
				"criteria": []map[string]any{{"description": "Test covers a falsifiable prediction"}},
			},
		},
		"provenance": map[string]any{
			"source_type": "standard",
			"source_ref":  "science-practice-standard",
			"rationale":   "separate model construction from empirical testing",
		},
	})
	if split.IsError {
		t.Fatalf("split: %q", resultText(split))
	}
	v2, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(v2.Operation.TargetConceptIDs) != 2 || v2.Operation.SourceConceptIDs[0] != sourceB.ID {
		t.Fatalf("split audit envelope incomplete: %+v", v2.Operation)
	}
	for _, targetID := range v2.Operation.TargetConceptIDs {
		concept := curriculumConceptByIDForTest(t, v2, targetID)
		if len(v2.Graph.Prerequisites[concept.Key]) != 1 || v2.Graph.Prerequisites[concept.Key][0] != "a" {
			t.Fatalf("split target did not inherit prerequisite a: %s -> %v", concept.Key, v2.Graph.Prerequisites[concept.Key])
		}
		if len(concept.Outcomes) == 0 || len(concept.Criteria) == 0 || concept.Level != models.CurriculumLevelIntermediate {
			t.Fatalf("target metadata lost: %+v", concept)
		}
	}

	merge := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   2,
		"operation":          "merge",
		"source_concept_ids": v2.Operation.TargetConceptIDs,
		"new_concepts": []map[string]any{{
			"label":    "b-inquiry",
			"level":    "advanced",
			"outcomes": []map[string]any{{"statement": "Run a complete model-test inquiry"}},
			"criteria": []map[string]any{{"description": "Conclusion follows from the evidence"}},
		}},
		"provenance": map[string]any{
			"source_type": "expert_review",
			"rationale":   "restore an integrated capstone after targeted practice",
		},
	})
	if merge.IsError {
		t.Fatalf("merge: %q", resultText(merge))
	}
	v3, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	merged := curriculumConceptByIDForTest(t, v3, v3.Operation.TargetConceptIDs[0])
	if got := v3.Graph.Prerequisites[merged.Key]; len(got) != 1 || got[0] != "a" {
		t.Fatalf("merge did not retain external prerequisite: %v", got)
	}

	var conceptA models.CurriculumConcept
	for _, concept := range v3.Concepts {
		if concept.Key == "a" {
			conceptA = concept
		}
	}
	blocked := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   3,
		"operation":          "remove",
		"source_concept_ids": []string{conceptA.ID},
		"provenance": map[string]any{
			"source_type": "learner",
			"rationale":   "attempt to remove an obsolete prerequisite",
		},
	})
	if !blocked.IsError || !strings.Contains(resultText(blocked), "depends on it") {
		t.Fatalf("unsafe prerequisite removal was not blocked: %q", resultText(blocked))
	}

	removed := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   3,
		"operation":          "remove",
		"source_concept_ids": []string{merged.ID},
		"provenance": map[string]any{
			"source_type": "learner",
			"rationale":   "retire the completed leaf competency without erasing history",
		},
	})
	if removed.IsError {
		t.Fatalf("safe leaf removal: %q", resultText(removed))
	}
	v4, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	retired := curriculumConceptByIDForTest(t, v4, merged.ID)
	if retired.Status != models.CurriculumConceptRetired || len(v4.Graph.Concepts) != 1 || v4.Graph.Concepts[0] != "a" {
		t.Fatalf("leaf removal erased history or left active graph inconsistent: retired=%+v graph=%v", retired, v4.Graph.Concepts)
	}
	history, err := store.ListCurriculumSnapshots(context.Background(), "L_owner", domain.ID, 50)
	if err != nil || len(history) != 4 {
		t.Fatalf("expected four immutable snapshots, got %d err=%v", len(history), err)
	}
}

func TestGetCurriculumSnapshot_ReadsHistoricalVersion(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "History")
	res := callTool(t, deps, registerGetCurriculumSnapshot, "L_owner", "get_curriculum_snapshot", map[string]any{
		"domain_id":       domain.ID,
		"version":         1,
		"include_history": true,
	})
	if res.IsError {
		t.Fatalf("get curriculum: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["snapshot"] == nil || out["history"] == nil {
		t.Fatalf("missing snapshot/history: %v", out)
	}
}

func TestGetCurriculumSnapshot_ReadsHistoryAfterDomainTombstone(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "Archived history")
	if err := store.DeleteDomain(context.Background(), domain.ID, "L_owner"); err != nil {
		t.Fatalf("tombstone domain: %v", err)
	}

	res := callTool(t, deps, registerGetCurriculumSnapshot, "L_owner", "get_curriculum_snapshot", map[string]any{
		"domain_id":       domain.ID,
		"version":         1,
		"include_history": true,
	})
	if res.IsError {
		t.Fatalf("read preserved curriculum history: %q", resultText(res))
	}
	out := decodeResult(t, res)
	history, ok := out["history"].([]any)
	if !ok || len(history) != 1 {
		t.Fatalf("tombstoned domain lost curriculum history: %v", out)
	}
}

func TestPublishCurriculumRevision_CannotSelfApproveReview(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "Safety")
	baseline, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   1,
		"operation":          "rename",
		"source_concept_ids": []string{baseline.Concepts[0].ID},
		"new_label":          "Reviewed safety",
		"provenance": map[string]any{
			"source_type": "learner",
			"rationale":   "request a review",
		},
		"review": map[string]any{
			"status":      "approved",
			"reviewer_id": "self-declared-reviewer",
		},
	})
	if !res.IsError || !strings.Contains(resultText(res), "trusted operator") {
		t.Fatalf("public caller self-approved curriculum: %q", resultText(res))
	}
}

func TestPublishCurriculumRevision_MetadataIDsStayOwnedAndStable(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "Metadata")
	baseline, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := baseline.Concepts[0]
	provenance := map[string]any{
		"source_type": "expert",
		"rationale":   "define observable evidence",
	}

	created := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   1,
		"operation":          "update_metadata",
		"source_concept_ids": []string{source.ID},
		"metadata": map[string]any{
			"label":       source.Label,
			"description": "Initial description",
			"level":       "foundation",
			"outcomes":    []map[string]any{{"statement": "Demonstrate the competency"}},
			"criteria":    []map[string]any{{"description": "Evidence is independently produced"}},
		},
		"provenance": provenance,
	})
	if created.IsError {
		t.Fatalf("create metadata: %q", resultText(created))
	}
	v2, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	withMetadata := curriculumConceptByIDForTest(t, v2, source.ID)
	if len(withMetadata.Outcomes) != 1 || len(withMetadata.Criteria) != 1 {
		t.Fatalf("metadata was not created: %+v", withMetadata)
	}
	outcomeID := withMetadata.Outcomes[0].ID
	criterionID := withMetadata.Criteria[0].ID

	partial := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   2,
		"operation":          "update_metadata",
		"source_concept_ids": []string{source.ID},
		"metadata": map[string]any{
			"label":       source.Label,
			"description": "Refined description",
		},
		"provenance": provenance,
	})
	if partial.IsError {
		t.Fatalf("partial metadata update: %q", resultText(partial))
	}
	v3, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	refined := curriculumConceptByIDForTest(t, v3, source.ID)
	if refined.Description != "Refined description" || refined.Level != models.CurriculumLevelFoundation ||
		len(refined.Outcomes) != 1 || refined.Outcomes[0].ID != outcomeID ||
		len(refined.Criteria) != 1 || refined.Criteria[0].ID != criterionID {
		t.Fatalf("partial update replaced stable metadata: %+v", refined)
	}

	forged := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   3,
		"operation":          "update_metadata",
		"source_concept_ids": []string{source.ID},
		"metadata": map[string]any{
			"label":    source.Label,
			"outcomes": []map[string]any{{"id": "outcome_forged", "statement": "Forged identity"}},
		},
		"provenance": provenance,
	})
	if !forged.IsError || !strings.Contains(resultText(forged), "does not belong") {
		t.Fatalf("caller supplied an unowned metadata ID: %q", resultText(forged))
	}

	updated := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   3,
		"operation":          "update_metadata",
		"source_concept_ids": []string{source.ID},
		"metadata": map[string]any{
			"label": source.Label,
			"outcomes": []map[string]any{{
				"id":         outcomeID,
				"statement":  "Demonstrate the refined competency",
				"observable": "independent production",
			}},
			"criteria": []map[string]any{{
				"id":          criterionID,
				"description": "Evidence is correct and independently produced",
			}},
		},
		"provenance": provenance,
	})
	if updated.IsError {
		t.Fatalf("update owned metadata IDs: %q", resultText(updated))
	}
	v4, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	stable := curriculumConceptByIDForTest(t, v4, source.ID)
	if stable.Outcomes[0].ID != outcomeID || stable.Criteria[0].ID != criterionID {
		t.Fatalf("owned metadata IDs changed: %+v", stable)
	}
}

func curriculumConceptByIDForTest(t *testing.T, snapshot *models.CurriculumSnapshot, id string) models.CurriculumConcept {
	t.Helper()
	for _, concept := range snapshot.Concepts {
		if concept.ID == id {
			return concept
		}
	}
	t.Fatalf("concept %q not found in snapshot", id)
	return models.CurriculumConcept{}
}
