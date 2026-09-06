// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func curriculumRevisionArgs(domainID string, version int, operation string, ids []string) map[string]any {
	return map[string]any{"domain_id": domainID, "expected_version": version, "operation": operation,
		"source_concept_ids": ids, "provenance": map[string]any{"source_type": "test", "rationale": "Clarify the curriculum contract"}}
}

func TestCurriculumReconciliationMCPRejectsStaleEvaluationAndStartsFresh(t *testing.T) {
	store, deps := setupToolsTest(t)
	ctx := context.Background()
	domain := makeOwnerDomain(t, store, "L_owner", "MCP reconciliation")
	baseline, err := store.GetCurriculumSnapshot(ctx, "L_owner", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	first, session := prepareAndSubmitAssessment(t, deps, domain.ID)
	graded := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", assessmentEvaluationArgs(domain.ID, session, first, true))
	if graded.IsError {
		t.Fatal(resultText(graded))
	}
	pending, pendingSession := prepareAndSubmitAssessment(t, deps, domain.ID)
	args := curriculumRevisionArgs(domain.ID, 1, "update_metadata", []string{baseline.Concepts[0].ID})
	args["metadata"] = map[string]any{"label": "a", "description": "A changed observable competency"}
	published := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", args)
	if published.IsError {
		t.Fatal(resultText(published))
	}
	state, err := store.GetConceptStateInDomain(ctx, "L_owner", domain.ID, "a")
	if err != nil || state.PMastery != 0.1 || state.Reps != 0 {
		t.Fatalf("reset: %+v %v", state, err)
	}
	stale := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", assessmentEvaluationArgs(domain.ID, pendingSession, pending, true))
	if !stale.IsError || !strings.Contains(resultText(stale), "curriculum definition changed") {
		t.Fatalf("stale evaluation result: %s", resultText(stale))
	}
	snapshots, err := store.GetPedagogicalSnapshots(ctx, "L_owner", domain.ID, "a", 10)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("historical audit removed or stale evaluation counted: %v %v", snapshots, err)
	}
	if snapshots[0].CurriculumInvalidatedVersion != 2 {
		t.Fatal("historical snapshot missing its superseded annotation")
	}
	old, err := store.GetAssessmentAttempt(ctx, "L_owner", first)
	if err != nil || !old.Passed || old.CurriculumInvalidatedVersion != 2 {
		t.Fatalf("historical assessment was overwritten: %+v %v", old, err)
	}
	newAttempt, newSession := prepareAndSubmitAssessment(t, deps, domain.ID)
	fresh := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", assessmentEvaluationArgs(domain.ID, newSession, newAttempt, true))
	if fresh.IsError {
		t.Fatal(resultText(fresh))
	}
	obs := decodeResult(t, fresh)["observation"].(map[string]any)
	profile := obs["bkt_individualized_profile"].(map[string]any)
	if profile["observations"] != float64(0) {
		t.Fatalf("new BKT profile reused superseded history: %v", profile)
	}
	current, err := store.GetAssessmentAttempt(ctx, "L_owner", newAttempt)
	if err != nil || current.CurriculumVersion != 2 || current.CurriculumInvalidatedVersion != 0 {
		t.Fatalf("fresh assessment not bound to new definition: %+v %v", current, err)
	}
	// Curriculum history contains affected IDs, never a second retention-
	// exempt copy of the learner's model values or response text.
	if strings.Contains(resultText(published), "PMastery") || strings.Contains(resultText(published), "A complete learner-owned response") {
		t.Fatal("learner data copied into immutable curriculum metadata")
	}
}

func TestCurriculumPrerequisiteRepairIsExplicitValidatedAndPreservesEvidence(t *testing.T) {
	store, deps := setupToolsTest(t)
	ctx := context.Background()
	domain := makeOwnerDomain(t, store, "L_owner", "prerequisite repair")
	base, err := store.GetCurriculumSnapshot(ctx, "L_owner", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, c := range base.Concepts {
		ids[c.Key] = c.ID
	}
	seed := models.NewConceptStateInDomain("L_owner", domain.ID, "a")
	seed.PMastery, seed.Reps = 0.93, 7
	if err := store.UpsertConceptState(ctx, seed); err != nil {
		t.Fatal(err)
	}
	attempt, session := prepareAndSubmitAssessment(t, deps, domain.ID)
	for name, replacements := range map[string]map[string][]string{
		"cycle":     {ids["a"]: {ids["b"]}},
		"self":      {ids["a"]: {ids["a"]}},
		"unknown":   {ids["a"]: {"unknown-competency-id"}},
		"duplicate": {ids["a"]: {ids["b"], ids["b"]}},
		"wrong key": {ids["b"]: {}},
	} {
		t.Run(name, func(t *testing.T) {
			args := curriculumRevisionArgs(domain.ID, 1, "repair_prerequisites", []string{ids["a"]})
			args["prerequisites"] = replacements
			if res := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", args); !res.IsError {
				t.Fatalf("invalid graph accepted: %s", resultText(res))
			}
		})
	}
	args := curriculumRevisionArgs(domain.ID, 1, "repair_prerequisites", []string{ids["b"]})
	args["prerequisites"] = map[string][]string{ids["b"]: {}}
	if res := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", args); res.IsError {
		t.Fatal(resultText(res))
	}
	current, err := store.GetCurriculumSnapshot(ctx, "L_owner", domain.ID, 2)
	if err != nil || len(current.Graph.Prerequisites["b"]) != 0 || len(current.Reconciliation.InvalidatedConceptIDs) != 0 {
		t.Fatalf("repair changed definition/evidence or kept edge: %+v %v", current, err)
	}
	state, err := store.GetConceptStateInDomain(ctx, "L_owner", domain.ID, "a")
	if err != nil || state.PMastery != 0.93 || state.Reps != 7 {
		t.Fatalf("graph-only repair reset estimates: %+v %v", state, err)
	}
	if res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", assessmentEvaluationArgs(domain.ID, session, attempt, true)); res.IsError {
		t.Fatalf("unchanged competency evidence rejected: %s", resultText(res))
	}
	// Repairing the edge makes removal possible; it does not silently remove
	// the prerequisite competency on the author's behalf.
	args = curriculumRevisionArgs(domain.ID, 2, "remove", []string{ids["a"]})
	if res := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", args); res.IsError {
		t.Fatal(resultText(res))
	}
	old, err := store.GetAssessmentAttempt(ctx, "L_owner", attempt)
	if err != nil || old.CurriculumInvalidatedVersion != 3 {
		t.Fatalf("removal did not supersede the old evidence: %+v %v", old, err)
	}
}
