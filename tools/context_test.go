// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestGetLearnerContext_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetLearnerContext, "", "get_learner_context", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestGetLearnerContext_ExposesCanonicalProfileAndObjective(t *testing.T) {
	_, deps := setupToolsTest(t)
	updated := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"objective": "speak Spanish at work",
		"language":  "es",
		"device":    "phone",
	})
	if updated.IsError {
		t.Fatalf("update profile: %q", resultText(updated))
	}

	res := callTool(t, deps, registerGetLearnerContext, "L_owner", "get_learner_context", map[string]any{})
	if res.IsError {
		t.Fatalf("get context: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["objective"] != "speak Spanish at work" {
		t.Fatalf("objective = %v", out["objective"])
	}
	profile, ok := out["profile"].(map[string]any)
	if !ok {
		t.Fatalf("profile type = %T, value=%v", out["profile"], out["profile"])
	}
	if profile["language"] != "es" || profile["device"] != "phone" {
		t.Fatalf("profile = %v", profile)
	}
}

func TestGetLearnerContext_InteractionsTodayUsesCalendarDay(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	now := time.Now().UTC()
	for _, createdAt := range []time.Time{now.Add(-48 * time.Hour), now.Add(-time.Minute)} {
		if _, err := store.RawDB().ExecContext(context.Background(), `
			INSERT INTO interactions
			(learner_id, domain_id, concept, activity_type, success, created_at)
			VALUES (?, ?, ?, 'PRACTICE', 1, ?)`, "L_owner", domain.ID, "a", createdAt); err != nil {
			t.Fatalf("insert interaction at %s: %v", createdAt, err)
		}
	}

	res := callTool(t, deps, registerGetLearnerContext, "L_owner", "get_learner_context", map[string]any{
		"domain_id": domain.ID,
	})
	if res.IsError {
		t.Fatalf("get context: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["interactions_today"] != float64(1) {
		encoded, _ := json.Marshal(out)
		t.Fatalf("interactions_today = %v, context=%s", out["interactions_today"], encoded)
	}
}

func TestGetLearnerContext_NeedsDomainSetup(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetLearnerContext, "L_owner", "get_learner_context", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["needs_domain_setup"] != true {
		t.Fatalf("expected needs_domain_setup=true, got %v", out["needs_domain_setup"])
	}
	if _, ok := out["priority_concept_domain_id"]; ok {
		t.Fatalf("expected no priority_concept_domain_id without priority, got %v", out["priority_concept_domain_id"])
	}
}

func TestGetLearnerContext_ActiveSessionBackendFailureIsVisible(t *testing.T) {
	store, deps := setupToolsTest(t)
	deps.Store = &failingLearningSessionLookupStore{Store: store, activeErr: context.DeadlineExceeded}

	res := callTool(t, deps, registerGetLearnerContext, "L_owner", "get_learner_context", map[string]any{})
	if !res.IsError || resultText(res) != "failed to load active learning session" {
		t.Fatalf("active-session backend failure was hidden: error=%v text=%q", res.IsError, resultText(res))
	}
}

func TestGetLearnerContext_WithDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	// Seed a non-new concept with low retention.
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.6
	cs.CardState = "review"
	cs.Stability = 1.0
	cs.ElapsedDays = 14
	lastReview := time.Now().UTC().Add(-14 * 24 * time.Hour)
	cs.LastReview = &lastReview
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	res := callTool(t, deps, registerGetLearnerContext, "L_owner", "get_learner_context", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["needs_domain_setup"] != false {
		t.Fatalf("expected needs_domain_setup=false, got %v", out["needs_domain_setup"])
	}
	if out["learner_id"] != "L_owner" {
		t.Fatalf("expected learner_id L_owner, got %v", out["learner_id"])
	}
	if out["opening_message"] == nil {
		t.Fatalf("expected opening_message, got %v", out)
	}
	if out["priority_concept_domain_id"] != d.ID {
		t.Fatalf("expected priority_concept_domain_id %q, got %v", d.ID, out["priority_concept_domain_id"])
	}
	domains, ok := out["domains"].([]any)
	if !ok || len(domains) != 1 {
		t.Fatalf("expected 1 active domain, got %v", out["domains"])
	}
}

func TestGetLearnerContext_OmitsPriorityConceptDomainIDWithoutPriority(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerGetLearnerContext, "L_owner", "get_learner_context", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["priority_concept"] != "" {
		t.Fatalf("expected no priority_concept, got %v", out["priority_concept"])
	}
	if _, ok := out["priority_concept_domain_id"]; ok {
		t.Fatalf("expected no priority_concept_domain_id without priority, got %v", out["priority_concept_domain_id"])
	}
}

func TestGetLearnerContext_PriorityConceptDomainIDUsesSourceDomain(t *testing.T) {
	store, deps := setupToolsTest(t)

	priorityDomain, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "math", "", models.KnowledgeSpace{
		Concepts:      []string{"slow_forgetting"},
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		t.Fatalf("create priority domain: %v", err)
	}
	time.Sleep(time.Millisecond)
	defaultDomain, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "physics", "", models.KnowledgeSpace{
		Concepts:      []string{"fresh_review"},
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		t.Fatalf("create default domain: %v", err)
	}
	gotDefault, err := store.GetDomainByLearner(context.Background(), "L_owner")
	if err != nil {
		t.Fatalf("get default domain: %v", err)
	}
	if gotDefault.ID != defaultDomain.ID {
		t.Fatalf("test setup expected default domain %q, got %q", defaultDomain.ID, gotDefault.ID)
	}

	priorityState := models.NewConceptState("L_owner", "slow_forgetting")
	priorityState.CardState = "review"
	priorityState.Stability = 1.0
	priorityState.ElapsedDays = 14
	priorityLastReview := time.Now().UTC().Add(-14 * 24 * time.Hour)
	priorityState.LastReview = &priorityLastReview
	if err := store.UpsertConceptState(context.Background(), priorityState); err != nil {
		t.Fatalf("upsert priority state: %v", err)
	}

	defaultState := models.NewConceptState("L_owner", "fresh_review")
	defaultState.CardState = "review"
	defaultState.Stability = 100.0
	defaultState.ElapsedDays = 1
	defaultLastReview := time.Now().UTC().Add(-24 * time.Hour)
	defaultState.LastReview = &defaultLastReview
	if err := store.UpsertConceptState(context.Background(), defaultState); err != nil {
		t.Fatalf("upsert default state: %v", err)
	}

	res := callTool(t, deps, registerGetLearnerContext, "L_owner", "get_learner_context", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["priority_concept"] != "slow_forgetting" {
		t.Fatalf("expected priority_concept slow_forgetting, got %v", out["priority_concept"])
	}
	if out["priority_concept_domain_id"] != priorityDomain.ID {
		t.Fatalf("expected priority_concept_domain_id %q, got %v", priorityDomain.ID, out["priority_concept_domain_id"])
	}
}

func TestGetLearnerContext_DomainsExposePriorityRank(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	if err := store.SetDomainPriority(context.Background(), d.ID, "L_owner", 1); err != nil {
		t.Fatalf("set domain priority: %v", err)
	}

	res := callTool(t, deps, registerGetLearnerContext, "L_owner", "get_learner_context", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	domains, ok := out["domains"].([]any)
	if !ok || len(domains) != 1 {
		t.Fatalf("expected one domain entry, got %v", out["domains"])
	}
	domain, ok := domains[0].(map[string]any)
	if !ok {
		t.Fatalf("expected domain object, got %T", domains[0])
	}
	if domain["priority_rank"] != float64(1) {
		t.Fatalf("expected priority_rank=1, got %v", domain["priority_rank"])
	}
}

func TestBuildProgressNarrative_ReturnsNilWhenNoData(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	learner, err := store.GetLearnerByID(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	got, err := buildProgressNarrative(context.Background(), deps, "L_owner", learner, d)
	if err != nil {
		t.Fatalf("build progress narrative: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil narrative when no signals, got %+v", got)
	}
}
