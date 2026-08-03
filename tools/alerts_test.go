// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestGetPendingAlerts_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetPendingAlerts, "", "get_pending_alerts", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestGetPendingAlerts_NoDataReturnsEmpty(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetPendingAlerts, "L_owner", "get_pending_alerts", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	alerts, ok := out["alerts"].([]any)
	if !ok {
		t.Fatalf("expected alerts array, got %v", out["alerts"])
	}
	if len(alerts) != 0 {
		t.Fatalf("expected empty alerts list, got %v", alerts)
	}
	if out["has_critical"] != false {
		t.Fatalf("expected has_critical=false, got %v", out["has_critical"])
	}
}

func TestGetPendingAlerts_FilterByDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerGetPendingAlerts, "L_owner", "get_pending_alerts", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
}

// orphanMasteryState builds the high-estimate part of a MASTERY_READY fixture.
// Readiness additionally requires retained, diverse evidence; tests that need
// a positive alert seed that evidence separately. Orphan filtering must happen
// before either component can influence the learner-facing alert stream.
func orphanMasteryState(learnerID, concept string) *models.ConceptState {
	now := time.Now().UTC()
	cs := models.NewConceptState(learnerID, concept)
	cs.CardState = "review"
	cs.PMastery = 0.95
	cs.LastReview = &now
	cs.Stability = 30
	cs.ElapsedDays = 0
	return cs
}

// Reproducer for issue #29: when the learner has no active domain at
// all, get_pending_alerts must NOT surface alerts on orphan concept
// states (the README contract). It must also signal needs_domain_setup
// so the LLM can self-correct.
func TestGetPendingAlerts_NoActiveDomain_ReturnsCleanEmpty(t *testing.T) {
	store, deps := setupToolsTest(t)
	// Insert a concept_state that *would* trigger MASTERY_READY for a
	// concept that is not in any domain. No init_domain call — learner
	// has zero active domains.
	if err := store.InsertConceptStateIfNotExists(context.Background(), orphanMasteryState("L_owner", "ghost")); err != nil {
		t.Fatalf("seed orphan state: %v", err)
	}

	res := callTool(t, deps, registerGetPendingAlerts, "L_owner", "get_pending_alerts", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)

	alerts, _ := out["alerts"].([]any)
	if len(alerts) > 0 {
		t.Fatalf("expected no alerts on orphan concept (no active domain), got %v", alerts)
	}
	if got, _ := out["needs_domain_setup"].(bool); !got {
		t.Errorf("expected needs_domain_setup=true when learner has no domain, got %v", out["needs_domain_setup"])
	}
	if out["has_critical"] != false {
		t.Errorf("expected has_critical=false, got %v", out["has_critical"])
	}
}

// TestGetPendingAlerts_WiresMetacognitiveAlerts is the regression for sub-issue
// #58: ComputeMetacognitiveAlerts must be called from the sync get_pending_alerts
// tool, not just from tests. Seeds the AFFECT_NEGATIVE precondition (two
// consecutive sessions with Satisfaction <= 2) and asserts the alert surfaces.
func TestGetPendingAlerts_WiresMetacognitiveAlerts(t *testing.T) {
	store, deps := setupToolsTest(t)
	// At least one active domain so the no-domain_id branch doesn't
	// short-circuit on needs_domain_setup.
	if _, err := store.CreateDomain(context.Background(), "L_owner", "math", "", models.KnowledgeSpace{
		Concepts: []string{"a"},
	}); err != nil {
		t.Fatalf("create domain: %v", err)
	}

	// Two consecutive low-satisfaction affect rows trigger AFFECT_NEGATIVE.
	now := time.Now().UTC()
	if err := store.UpsertAffectState(context.Background(), &models.AffectState{
		LearnerID:    "L_owner",
		SessionID:    "s1",
		Satisfaction: 2,
	}); err != nil {
		t.Fatalf("upsert affect s1: %v", err)
	}
	// Force a small ordering gap so newest-first ordering is deterministic.
	_ = now
	if err := store.UpsertAffectState(context.Background(), &models.AffectState{
		LearnerID:    "L_owner",
		SessionID:    "s2",
		Satisfaction: 1,
	}); err != nil {
		t.Fatalf("upsert affect s2: %v", err)
	}

	res := callTool(t, deps, registerGetPendingAlerts, "L_owner", "get_pending_alerts", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	alerts, _ := out["alerts"].([]any)
	sawAffect := false
	for _, a := range alerts {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == string(models.AlertAffectNegative) {
			sawAffect = true
		}
	}
	if !sawAffect {
		t.Fatalf("expected AFFECT_NEGATIVE metacognitive alert in payload, got %+v", alerts)
	}
}

// TestGetPendingAlerts_DedupsMetacognitiveAlerts asserts mergeMetacognitiveAlerts
// does not double-emit when an activity-level alert and a metacognitive alert
// share the same (Type, Concept). We synthesize the collision by reaching into
// the merge helper directly — going through the DB would require an
// AFFECT_NEGATIVE-equivalent on the activity side, which doesn't exist.
func TestMergeMetacognitiveAlerts_Dedupes(t *testing.T) {
	base := []models.Alert{
		{Type: models.AlertAffectNegative, Concept: ""},
	}
	extra := []models.Alert{
		{Type: models.AlertAffectNegative, Concept: ""},
		{Type: models.AlertCalibrationDiverging, Concept: ""},
	}
	merged := mergeMetacognitiveAlerts(base, extra)
	if len(merged) != 2 {
		t.Fatalf("expected dedup to drop the duplicate AFFECT_NEGATIVE, got %d alerts: %+v", len(merged), merged)
	}
	// Ensure CALIBRATION_DIVERGING was kept (i.e. dedup is by type+concept,
	// not a blanket de-overlap).
	sawCalib := false
	for _, a := range merged {
		if a.Type == models.AlertCalibrationDiverging {
			sawCalib = true
		}
	}
	if !sawCalib {
		t.Errorf("dedup must not drop unrelated kinds, got %+v", merged)
	}
}

// Reproducer for issue #29: when the learner has multiple non-archived
// domains and no domain_id filter is given, alerts must be computed only
// over the union of concepts across active domains — both legacy unowned rows
// and evidence preserved under a domain tombstone must be filtered out. Also:
// alerts on concepts belonging to *any* active domain must surface (i.e. the
// handler shouldn't pick a single arbitrary domain in this case).
func TestGetPendingAlerts_MultipleActiveDomains_FiltersOutOrphanAndTombstone(t *testing.T) {
	store, deps := setupToolsTest(t)
	// Two active domains with disjoint concept sets. D2 is created last,
	// so a single-domain fallback in resolveDomain would only see {x,y}
	// and silently drop a legitimate alert on "a".
	d1, err := store.CreateDomain(context.Background(), "L_owner", "d1", "", models.KnowledgeSpace{
		Concepts:      []string{"a", "b"},
		Prerequisites: map[string][]string{"b": {"a"}},
	})
	if err != nil {
		t.Fatalf("create d1: %v", err)
	}
	if _, err := store.CreateDomain(context.Background(), "L_owner", "d2", "", models.KnowledgeSpace{
		Concepts:      []string{"x", "y"},
		Prerequisites: map[string][]string{"y": {"x"}},
	}); err != nil {
		t.Fatalf("create d2: %v", err)
	}
	retired, err := store.CreateDomain(context.Background(), "L_owner", "retired-domain", "", models.KnowledgeSpace{
		Concepts: []string{"retired"},
	})
	if err != nil {
		t.Fatalf("create retired domain: %v", err)
	}

	// Seed readiness states with explicit ownership for active D1 and the future
	// tombstone, plus a legacy unowned "ghost" row. Only D1 may surface.
	aState := orphanMasteryState("L_owner", "a")
	aState.DomainID = d1.ID
	if err := store.InsertConceptStateIfNotExists(context.Background(), aState); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	retiredState := orphanMasteryState("L_owner", "retired")
	retiredState.DomainID = retired.ID
	if err := store.InsertConceptStateIfNotExists(context.Background(), retiredState); err != nil {
		t.Fatalf("seed retired: %v", err)
	}
	if err := store.InsertConceptStateIfNotExists(context.Background(), orphanMasteryState("L_owner", "ghost")); err != nil {
		t.Fatalf("seed ghost: %v", err)
	}
	// MASTERY_READY is challenge readiness, not a BKT threshold alias. Seed
	// retained, temporally separated and activity-diverse evidence for both
	// owned concepts. Tombstoning must preserve the retired evidence for audit
	// while removing it from active routing.
	now := time.Now().UTC()
	aRetentionAttempt := seedEvaluatedAssessmentFixture(t, store, "L_owner", d1.ID, "a", models.ActivityRecall, true, now.Add(-time.Hour), "")
	retiredRetentionAttempt := seedEvaluatedAssessmentFixture(t, store, "L_owner", retired.ID, "retired", models.ActivityRecall, true, now.Add(-time.Hour), "")
	for _, interaction := range []*models.Interaction{
		{LearnerID: "L_owner", DomainID: d1.ID, Concept: "a", ActivityType: string(models.ActivityPractice), Success: true, CreatedAt: now.Add(-48 * time.Hour)},
		{LearnerID: "L_owner", DomainID: d1.ID, Concept: "a", ActivityType: string(models.ActivityFeynmanPrompt), Success: true, CreatedAt: now.Add(-2 * time.Hour)},
		{LearnerID: "L_owner", DomainID: d1.ID, AssessmentAttemptID: aRetentionAttempt, Concept: "a", ActivityType: string(models.ActivityRecall), Success: true, CreatedAt: now.Add(-time.Hour)},
		{LearnerID: "L_owner", DomainID: retired.ID, Concept: "retired", ActivityType: string(models.ActivityPractice), Success: true, CreatedAt: now.Add(-48 * time.Hour)},
		{LearnerID: "L_owner", DomainID: retired.ID, Concept: "retired", ActivityType: string(models.ActivityFeynmanPrompt), Success: true, CreatedAt: now.Add(-2 * time.Hour)},
		{LearnerID: "L_owner", DomainID: retired.ID, AssessmentAttemptID: retiredRetentionAttempt, Concept: "retired", ActivityType: string(models.ActivityRecall), Success: true, CreatedAt: now.Add(-time.Hour)},
	} {
		createdAt := interaction.CreatedAt
		if err := store.CreateInteraction(context.Background(), interaction); err != nil {
			t.Fatalf("seed mastery-ready evidence: %v", err)
		}
		// CreateInteraction assigns the production clock. Restore the fixture's
		// explicit timestamps so this test actually contains a delayed recall.
		if _, err := store.RawDB().ExecContext(context.Background(),
			`UPDATE interactions SET created_at = ? WHERE id = ?`, createdAt, interaction.ID,
		); err != nil {
			t.Fatalf("date mastery-ready evidence: %v", err)
		}
	}
	if err := store.DeleteDomain(context.Background(), retired.ID, "L_owner"); err != nil {
		t.Fatalf("tombstone retired domain: %v", err)
	}

	// Call with no domain_id — handler should aggregate concepts from
	// ALL active domains and ignore "ghost".
	res := callTool(t, deps, registerGetPendingAlerts, "L_owner", "get_pending_alerts", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)

	alerts, _ := out["alerts"].([]any)
	sawA := false
	for _, a := range alerts {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if m["concept"] == "ghost" {
			t.Fatalf("orphan concept 'ghost' surfaced in alerts: %v", alerts)
		}
		if m["concept"] == "retired" {
			t.Fatalf("tombstoned concept 'retired' surfaced in alerts: %v", alerts)
		}
		if m["concept"] == "a" {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("expected alert on 'a' (in active domain D1) to surface across multiple domains, got %v", alerts)
	}
	// needs_domain_setup must be false when active domains exist.
	if got, _ := out["needs_domain_setup"].(bool); got {
		t.Errorf("expected needs_domain_setup=false when active domains exist, got true")
	}
}
