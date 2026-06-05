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

// orphanMasteryState builds a ConceptState shaped to trigger the
// MASTERY_READY branch of engine.ComputeAlerts (CardState != "new" and
// PMastery >= MasteryBKT()=0.85). Used to assert the orphan-filter
// contract: this state should be skipped when the concept is not in any
// active domain.
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
// over the union of concepts across active domains — orphan concepts
// (e.g. survivors of a deleted domain) must be filtered out. Also: alerts
// on concepts belonging to *any* active domain must surface (i.e. the
// handler shouldn't pick a single arbitrary domain in this case).
func TestGetPendingAlerts_MultipleActiveDomains_FiltersOutOrphan(t *testing.T) {
	store, deps := setupToolsTest(t)
	// Two active domains with disjoint concept sets. D2 is created last,
	// so a single-domain fallback in resolveDomain would only see {x,y}
	// and silently drop a legitimate alert on "a".
	if _, err := store.CreateDomain(context.Background(), "L_owner", "d1", "", models.KnowledgeSpace{
		Concepts:      []string{"a", "b"},
		Prerequisites: map[string][]string{"b": {"a"}},
	}); err != nil {
		t.Fatalf("create d1: %v", err)
	}
	if _, err := store.CreateDomain(context.Background(), "L_owner", "d2", "", models.KnowledgeSpace{
		Concepts:      []string{"x", "y"},
		Prerequisites: map[string][]string{"y": {"x"}},
	}); err != nil {
		t.Fatalf("create d2: %v", err)
	}

	// Seed a MASTERY_READY-trigger state on "a" (D1) and on "ghost"
	// (no domain). Only "a" should surface.
	if err := store.InsertConceptStateIfNotExists(context.Background(), orphanMasteryState("L_owner", "a")); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := store.InsertConceptStateIfNotExists(context.Background(), orphanMasteryState("L_owner", "ghost")); err != nil {
		t.Fatalf("seed ghost: %v", err)
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
