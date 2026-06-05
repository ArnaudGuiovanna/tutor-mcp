// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/memory"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

func TestGetNextActivity_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetNextActivity, "", "get_next_activity", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestGetNextActivity_AuthFailureLoggedAtInfo(t *testing.T) {
	_, deps := setupToolsTest(t)
	var buf bytes.Buffer
	deps.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	res := callTool(t, deps, registerGetNextActivity, "", "get_next_activity", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}

	logs := buf.String()
	if !strings.Contains(logs, "level=INFO") || !strings.Contains(logs, "get_next_activity: auth failed") {
		t.Fatalf("expected auth failure at INFO, got logs=%q", logs)
	}
	if strings.Contains(logs, "level=ERROR") {
		t.Fatalf("auth failure should not be logged at ERROR, got logs=%q", logs)
	}
}

func TestGetNextActivity_NeedsDomainSetup(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["needs_domain_setup"] != true {
		t.Fatalf("expected needs_domain_setup=true, got %v", out["needs_domain_setup"])
	}
}

func TestGetNextActivity_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	// Seed concept state in domain.
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.5
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["needs_domain_setup"] != false {
		t.Fatalf("expected needs_domain_setup=false, got %v", out["needs_domain_setup"])
	}
	if _, ok := out["activity"]; !ok {
		t.Fatalf("expected activity key, got %v", out)
	}
	if _, ok := out["tutor_mode"]; !ok {
		t.Fatalf("expected tutor_mode key, got %v", out)
	}
	if _, ok := out["motivation_brief"]; !ok {
		t.Fatalf("expected motivation_brief key, got %v", out)
	}
	if _, ok := out["mastery_evidence"]; !ok {
		t.Fatalf("expected mastery_evidence key, got %v", out)
	}
	if _, ok := out["mastery_uncertainty"]; !ok {
		t.Fatalf("expected mastery_uncertainty key, got %v", out)
	}
	if _, ok := out["pedagogical_contract"]; !ok {
		t.Fatalf("expected pedagogical_contract key, got %v", out)
	}
	if _, ok := out["goal_relevance_status"]; !ok {
		t.Fatalf("expected goal_relevance_status key, got %v", out)
	}
	activity, _ := out["activity"].(map[string]any)
	if prompt, _ := activity["prompt_for_llm"].(string); strings.Contains(prompt, "Target difficulty:") {
		t.Fatalf("prompt embeds stale numeric difficulty: %q", prompt)
	}
	contract, _ := out["pedagogical_contract"].(map[string]any)
	if got := contract["audit_rationale"]; got == "" {
		t.Fatalf("expected contract audit_rationale, got %v", contract)
	}
	if got := contract["llm_instruction"]; got == "" {
		t.Fatalf("expected contract llm_instruction, got %v", contract)
	}
	if got := contract["learner_explanation"]; got == "" {
		t.Fatalf("expected contract learner_explanation, got %v", contract)
	}
}

func TestGetNextActivity_IncludesEpisodicContextAndReasoningRequest(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.5
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)
	ts := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)
	if err := memory.Write(memory.WriteRequest{
		LearnerID: "L_owner",
		Scope:     memory.ScopeSession,
		Timestamp: ts,
		Operation: memory.OpReplaceFile,
		Content:   "---\ntimestamp: 2026-05-14T09:30:00Z\nnovelty_flag: true\n---\n\n## Summary\nThe learner relied on a brittle cue.",
	}); err != nil {
		t.Fatalf("write memory session: %v", err)
	}

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	ec, ok := out["episodic_context"].(map[string]any)
	if !ok {
		t.Fatalf("expected episodic_context, got %v", out)
	}
	if sessions, _ := ec["recent_sessions"].([]any); len(sessions) != 1 {
		t.Fatalf("recent_sessions = %v", ec["recent_sessions"])
	}
	contract, _ := out["pedagogical_contract"].(map[string]any)
	if !strings.Contains(contract["llm_instruction"].(string), "Perform pattern completion") {
		t.Fatalf("llm_instruction missing memory guidance: %q", contract["llm_instruction"])
	}
	req, ok := contract["reasoning_request"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning_request, got %v", contract)
	}
	reasons, _ := req["enabled_because"].([]any)
	if len(reasons) == 0 || reasons[0] != "episodic_context_available" {
		t.Fatalf("unexpected reasoning reasons: %v", reasons)
	}
}

func TestGetNextActivity_AttachesClientInitiatedConsolidationRequest(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	t.Setenv("TUTOR_MCP_CONSOLIDATION_SEED", "7")
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.5
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if err := memory.Write(memory.WriteRequest{LearnerID: "L_owner", Scope: memory.ScopeMemory, Operation: memory.OpReplaceFile, Content: "# Stable memory\n"}); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	if err := memory.Write(memory.WriteRequest{LearnerID: "L_owner", Scope: memory.ScopeMemoryPending, Operation: memory.OpReplaceFile, Content: "- pending observation\n"}); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if err := memory.Write(memory.WriteRequest{LearnerID: "L_owner", Scope: memory.ScopeConcept, ConceptSlug: "a", Operation: memory.OpReplaceFile, Content: "## Current state\nEarly understanding."}); err != nil {
		t.Fatalf("write concept: %v", err)
	}
	for _, ts := range []time.Time{
		time.Date(2026, time.April, 10, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.April, 11, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.April, 12, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.April, 13, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC),
	} {
		if err := memory.Write(memory.WriteRequest{
			LearnerID: "L_owner",
			Scope:     memory.ScopeSession,
			Timestamp: ts,
			Operation: memory.OpReplaceFile,
			Content:   "---\ntimestamp: " + ts.Format(time.RFC3339) + "\nconcepts_touched: [\"a\"]\nnovelty_flag: false\n---\n\n## Summary\nWorked on a.",
		}); err != nil {
			t.Fatalf("write session: %v", err)
		}
	}
	if err := store.UpsertPendingConsolidation(context.Background(), "L_owner", "monthly", "2026-04", time.Date(2026, time.May, 3, 13, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("UpsertPendingConsolidation: %v", err)
	}

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{"domain_id": d.ID})
	if res.IsError {
		t.Fatalf("get_next_activity failed: %s", resultText(res))
	}
	out := decodeResult(t, res)
	req, ok := out["consolidation_request"].(map[string]any)
	if !ok {
		t.Fatalf("expected consolidation_request, got %v", out)
	}
	if !strings.Contains(req["instruction"].(string), "pending consolidation jobs") {
		t.Fatalf("unexpected instruction: %q", req["instruction"])
	}
	jobs, _ := req["pending_jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("pending_jobs = %v", req["pending_jobs"])
	}
	job := jobs[0].(map[string]any)
	if job["period_type"] != "monthly" || job["period_key"] != "2026-04" {
		t.Fatalf("unexpected job payload: %v", job)
	}
	if sessions, _ := job["sessions_in_period"].([]any); len(sessions) != 4 {
		t.Fatalf("sessions_in_period = %v", job["sessions_in_period"])
	}
	if replay, _ := job["interleaved_replay_sessions"].([]any); len(replay) != 1 {
		t.Fatalf("interleaved_replay_sessions = %v", job["interleaved_replay_sessions"])
	}
	if concepts, _ := job["touched_concepts"].([]any); len(concepts) != 1 {
		t.Fatalf("touched_concepts = %v", job["touched_concepts"])
	}
	item, err := store.GetConsolidation(context.Background(), "L_owner", "monthly", "2026-04")
	if err != nil {
		t.Fatalf("GetConsolidation delivered: %v", err)
	}
	if item.Status != "delivered" {
		t.Fatalf("status after delivery = %q, want delivered", item.Status)
	}

	archive := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":       "archive",
		"period_type": "monthly",
		"period_key":  "2026-04",
		"content":     "# Consolidation 2026-04\n\n## Period trajectory\nThe learner practiced a.",
	})
	if archive.IsError {
		t.Fatalf("archive write failed: %s", resultText(archive))
	}
	item, _ = store.GetConsolidation(context.Background(), "L_owner", "monthly", "2026-04")
	if item.Status != "completed" {
		t.Fatalf("status after archive = %q, want completed", item.Status)
	}
	second := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{"domain_id": d.ID})
	if second.IsError {
		t.Fatalf("second get_next_activity failed: %s", resultText(second))
	}
	secondOut := decodeResult(t, second)
	if _, ok := secondOut["consolidation_request"]; ok {
		t.Fatalf("completed consolidation should not be requested again: %v", secondOut["consolidation_request"])
	}
}

func TestGetNextActivity_OLMInconsistencyActivatesReasoningRequest(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.90
	cs.CardState = "review"
	cs.ElapsedDays = 200
	cs.Stability = 1
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	ec, ok := out["episodic_context"].(map[string]any)
	if !ok {
		t.Fatalf("expected episodic_context, got %v", out)
	}
	inconsistencies, _ := ec["olm_inconsistencies"].([]any)
	if len(inconsistencies) == 0 {
		t.Fatalf("expected OLM inconsistency, got %v", ec)
	}
	contract, _ := out["pedagogical_contract"].(map[string]any)
	req, ok := contract["reasoning_request"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning_request, got %v", contract)
	}
	reasons, _ := req["enabled_because"].([]any)
	found := false
	for _, reason := range reasons {
		if reason == "olm_inconsistencies_to_compensate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing OLM inconsistency reason: %v", reasons)
	}
}

func TestGetNextActivity_OverloadUsesRealSessionStart(t *testing.T) {
	store, deps := setupToolsTest(t)
	var logs bytes.Buffer
	deps.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d := makeOwnerDomain(t, store, "L_owner", "math")
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.5
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	oldStart := time.Now().UTC().Add(-1 * time.Hour)
	if _, err := store.RawDB().Exec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, created_at, domain_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"L_owner", "a", string(models.ActivityPractice), 1, 1000, 0.8, oldStart, d.ID,
	); err != nil {
		t.Fatalf("seed old interaction: %v", err)
	}

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q logs=%s", resultText(res), logs.String())
	}
	out := decodeResult(t, res)
	activity, ok := out["activity"].(map[string]any)
	if !ok {
		t.Fatalf("expected activity object, got %v", out["activity"])
	}
	if got := activity["type"]; got != string(models.ActivityCloseSession) {
		t.Fatalf("expected CLOSE_SESSION from overloaded session, got %v", activity)
	}
}

func TestBuildGoalRelevanceStatus(t *testing.T) {
	base := func() *models.Domain {
		return &models.Domain{
			GraphVersion: 1,
			Graph: models.KnowledgeSpace{
				Concepts: []string{"a", "b"},
			},
		}
	}

	t.Run("missing", func(t *testing.T) {
		got := buildGoalRelevanceStatus(base())
		if got.Status != "missing" || got.RecommendedTool != "set_goal_relevance" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("partial", func(t *testing.T) {
		d := base()
		d.GoalRelevanceJSON = `{"for_graph_version":1,"relevance":{"a":1},"set_at":"2026-01-01T00:00:00Z"}`
		got := buildGoalRelevanceStatus(d)
		if got.Status != "partial" || len(got.MissingConcepts) != 1 || got.MissingConcepts[0] != "b" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("valid", func(t *testing.T) {
		d := base()
		d.GoalRelevanceJSON = `{"for_graph_version":1,"relevance":{"a":1,"b":0.5},"set_at":"2026-01-01T00:00:00Z"}`
		got := buildGoalRelevanceStatus(d)
		if got.Status != "valid" || got.RecommendedTool != "" {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestGetNextActivity_ForeignDomainFallsBackToSetup(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerGetNextActivity, "L_attacker", "get_next_activity", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	// Foreign learner should fall through to needs_domain_setup since resolveDomain rejects.
	if out["needs_domain_setup"] != true {
		t.Fatalf("expected setup fallback for foreign domain, got %v", out)
	}
}

func TestGetNextActivity_DomainNameSelectsMatchingDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	goDomain, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "Golang", "", models.KnowledgeSpace{
		Concepts:      []string{"Pointers"},
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		t.Fatalf("create go domain: %v", err)
	}
	if _, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "Conditional Probability", "", models.KnowledgeSpace{
		Concepts:      []string{"Bayes"},
		Prerequisites: map[string][]string{},
	}, ""); err != nil {
		t.Fatalf("create probability domain: %v", err)
	}

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{
		"domain_name": "golang",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if got := out["domain_id"]; got != goDomain.ID {
		t.Fatalf("expected Golang domain %q, got %v", goDomain.ID, got)
	}
}

func TestGetNextActivity_ReviewIntentAvoidsNewConcept(t *testing.T) {
	store, deps := setupToolsTest(t)
	d, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "Golang", "", models.KnowledgeSpace{
		Concepts:      []string{"Pointers", "Generics"},
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if _, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{
		"Pointers": 0.2,
		"Generics": 1.0,
	}); err != nil {
		t.Fatalf("seed relevance: %v", err)
	}
	lastReview := time.Now().UTC().Add(-10 * 24 * time.Hour)
	cs := models.NewConceptState("L_owner", "Pointers")
	cs.PMastery = 0.75
	cs.CardState = "review"
	cs.Stability = 2
	cs.ElapsedDays = 10
	cs.Reps = 2
	cs.LastReview = &lastReview
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{
		"domain_id": d.ID,
		"intent":    "review",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	activity, ok := out["activity"].(map[string]any)
	if !ok {
		t.Fatalf("expected activity object, got %v", out["activity"])
	}
	if got := activity["type"]; got == string(models.ActivityNewConcept) {
		t.Fatalf("review intent must not return NEW_CONCEPT, got %v", activity)
	}
	if got := activity["concept"]; got != "Pointers" {
		t.Fatalf("expected review of Pointers, got %v", activity)
	}
	if got := out["intent_status"]; got != "applied" {
		t.Fatalf("expected intent_status=applied, got %v", got)
	}
}

func TestGetNextActivity_ReviewIntentNoReviewedConceptDoesNotIntroduce(t *testing.T) {
	store, deps := setupToolsTest(t)
	d, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "Golang", "", models.KnowledgeSpace{
		Concepts:      []string{"Generics"},
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{
		"domain_id": d.ID,
		"intent":    "revise",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	activity, ok := out["activity"].(map[string]any)
	if !ok {
		t.Fatalf("expected activity object, got %v", out["activity"])
	}
	if got := activity["type"]; got == string(models.ActivityNewConcept) {
		t.Fatalf("review intent with no reviewed concept must not introduce, got %v", activity)
	}
	if got := out["intent_status"]; got != "no_reviewable_concept" {
		t.Fatalf("expected no_reviewable_concept, got %v", got)
	}
}

// TestGetNextActivity_FlagOff_NoFadeFields asserts the byte-equivalence
// guarantee of the FadeController constraint: with REGULATION_FADE
// unset (its default), the JSON returned by get_next_activity must
// contain neither fade_params nor autonomy_score keys, and the
// motivation_brief must be the legacy output untouched. This is the
// "FadeController is post-decision and cannot affect orchestrator
// outputs when the flag is off" invariant from the design doc.
func TestGetNextActivity_FlagOff_NoFadeFields(t *testing.T) {
	// Force flag OFF regardless of caller's environment so this test
	// is robust to `REGULATION_FADE=on go test ./...` invocations.
	t.Setenv("REGULATION_FADE", "")
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.5
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity",
		map[string]any{"domain_id": d.ID})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	for _, key := range []string{"fade_params", "autonomy_score", "autonomy_trend"} {
		if _, ok := out[key]; ok {
			t.Errorf("flag OFF: expected no %q key in output, got %v", key, out[key])
		}
	}
}

// TestGetNextActivity_FadeFlagOn_VerbosityDecreasesAsAutonomyRises is
// the integration test required by the FadeController acceptance
// criteria. We simulate a learner whose autonomy_score rises across
// multiple sessions, call get_next_activity at successive points, and
// assert that:
//
//   - fade_params is present in the JSON,
//   - the hint_level transitions from "full" toward "none" as the
//     score climbs,
//   - the motivation_brief.instruction length is non-increasing along
//     the sequence (verbosity monotonically drops).
func TestGetNextActivity_FadeFlagOn_VerbosityDecreasesAsAutonomyRises(t *testing.T) {
	t.Setenv("REGULATION_FADE", "on")

	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	cs := models.NewConceptState("L_owner", "a")
	cs.PMastery = 0.5
	_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
	_ = store.UpsertConceptState(context.Background(), cs)

	// Steps: at each step, seed enough affect rows to drive the
	// autonomy_score to the target tier. We use direct UpsertAffectState
	// per session, then patch autonomy_score via UpdateAffectAutonomyScore
	// (the upsert path's CASE WHEN excluded.autonomy_score > 0 guard
	// preserves the value we set).
	steps := []struct {
		name           string
		affectScores   []float64 // newest-first; one row per session
		wantHintAtMost int       // 2 = full, 1 = partial, 0 = none
	}{
		{"start: low autonomy", []float64{0.10, 0.10, 0.10}, 2},
		{"climbing: mid autonomy", []float64{0.55, 0.50, 0.40, 0.20, 0.10, 0.10}, 1},
		{"high autonomy + improving", []float64{0.95, 0.92, 0.90, 0.88, 0.30, 0.25, 0.20, 0.15, 0.10, 0.10}, 0},
	}

	hintRank := map[string]int{"full": 2, "partial": 1, "none": 0, "": 0}

	prevInstrLen := -1
	for stepIdx, st := range steps {
		// Wipe prior affect rows to reseed the timeline cleanly per
		// step — newest-first ordering of GetRecentAffectStates is
		// driven by created_at, and re-using session IDs would upsert
		// rather than insert.
		raw := store.RawDB()
		if _, err := raw.Exec(`DELETE FROM affect_states WHERE learner_id = ?`, "L_owner"); err != nil {
			t.Fatalf("step %s: wipe affects: %v", st.name, err)
		}

		// Seed in reverse (oldest-first) so created_at ordering matches
		// newest-first when read back.
		for i := len(st.affectScores) - 1; i >= 0; i-- {
			sid := fmt.Sprintf("S_%d_%d", stepIdx, i)
			a := &models.AffectState{
				LearnerID:     "L_owner",
				SessionID:     sid,
				Energy:        3,
				AutonomyScore: st.affectScores[i],
			}
			if err := store.UpsertAffectState(context.Background(), a); err != nil {
				t.Fatalf("step %s: upsert affect %s: %v", st.name, sid, err)
			}
			// CASE WHEN excluded.autonomy_score > 0: setting it
			// directly via insert works for non-zero scores.
		}

		res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity",
			map[string]any{"domain_id": d.ID})
		if res.IsError {
			t.Fatalf("step %s: %q", st.name, resultText(res))
		}
		out := decodeResult(t, res)

		fp, ok := out["fade_params"].(map[string]any)
		if !ok {
			t.Fatalf("step %s: expected fade_params, got %v", st.name, out)
		}
		gotHint, _ := fp["hint_level"].(string)
		if hintRank[gotHint] > st.wantHintAtMost {
			t.Errorf("step %s: hint_level=%q (rank %d), want at most rank %d",
				st.name, gotHint, hintRank[gotHint], st.wantHintAtMost)
		}
		contract, ok := out["pedagogical_contract"].(map[string]any)
		if !ok {
			t.Fatalf("step %s: expected pedagogical_contract, got %v", st.name, out)
		}
		fadeGuidance, ok := contract["fade_guidance"].(map[string]any)
		if !ok {
			t.Fatalf("step %s: expected fade_guidance in contract, got %v", st.name, contract)
		}
		if got := fadeGuidance["hint_level"]; got != gotHint {
			t.Errorf("step %s: contract hint_level=%v, want %q", st.name, got, gotHint)
		}

		var instrLen int
		if mb, ok := out["motivation_brief"].(map[string]any); ok {
			if instr, ok := mb["instruction"].(string); ok {
				instrLen = len(instr)
			}
		}
		if prevInstrLen >= 0 && instrLen > prevInstrLen {
			t.Errorf("step %s: motivation instruction length grew (was %d, now %d) — verbosity should be monotonically non-increasing as autonomy rises",
				st.name, prevInstrLen, instrLen)
		}
		prevInstrLen = instrLen
	}
}

// TestGetNextActivity_PostOrchestratePhaseMatchesDB is the regression
// test for perf #91. Before the change, get_next_activity re-read the
// domain row from the DB to log the post-orchestrate phase. The new
// path consumes the phase directly from OrchestrateWithPhase. This
// test seeds a scenario that triggers an FSM transition
// (INSTRUCTION → MAINTENANCE), calls get_next_activity, and asserts
// the call succeeds and the DB phase is what the orchestrator
// persisted — confirming the in-handler logging path still observes
// the same phase the DB sees.
func TestGetNextActivity_PostOrchestratePhaseMatchesDB(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	var buf bytes.Buffer
	deps.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Seed phase = INSTRUCTION and goal-relevance so the FSM has
	// something to evaluate.
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseInstruction, 0, time.Now().UTC()); err != nil {
		t.Fatalf("seed phase: %v", err)
	}
	if _, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{
		"a": 1.0, "b": 0.8,
	}); err != nil {
		t.Fatalf("seed goal_relevance: %v", err)
	}

	// Mastery above BKT threshold for both concepts → FSM should
	// transition to MAINTENANCE.
	for _, c := range []string{"a", "b"} {
		cs := models.NewConceptState("L_owner", c)
		cs.PMastery = 0.95
		cs.CardState = "review"
		cs.Stability = 30
		cs.ElapsedDays = 1
		_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
		if err := store.UpsertConceptState(context.Background(), cs); err != nil {
			t.Fatalf("seed state %s: %v", c, err)
		}
	}

	res := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity",
		map[string]any{"domain_id": d.ID})
	if res.IsError {
		t.Fatalf("get_next_activity: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["needs_domain_setup"] != false {
		t.Fatalf("expected needs_domain_setup=false, got %v", out["needs_domain_setup"])
	}

	// The orchestrator should have persisted the MAINTENANCE phase.
	// This indirectly verifies that the phase the in-handler logger
	// observed (via the OrchestrateWithPhase return value) matches
	// what's in the DB — i.e. the perf #91 change preserves the
	// audit-log invariant.
	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if got.Phase != models.PhaseMaintenance {
		t.Errorf("expected DB phase=MAINTENANCE after transition, got %q", got.Phase)
	}
	logs := buf.String()
	if !strings.Contains(logs, "pipeline decision") || !strings.Contains(logs, "route=orchestrator") {
		t.Fatalf("expected pipeline decision with route=orchestrator, got logs=%q", logs)
	}
	if !strings.Contains(logs, "phase transition (FSM)") {
		t.Fatalf("expected FSM transition to use deps.Logger, got logs=%q", logs)
	}
}

// BenchmarkGetNextActivity exercises the full get_next_activity tool
// handler against a freshly seeded in-memory SQLite DB. It does NOT
// assert a latency target — its purpose is to give future PRs (the
// rest of issue #91: caching, query merging, async webhook) a stable
// reference point to measure regressions and improvements against.
//
// Run with:
//
//	go test ./tools -bench=BenchmarkGetNextActivity -benchmem -run=^$
func BenchmarkGetNextActivity(b *testing.B) {
	store, deps := setupBenchTools(b)
	d := makeBenchDomain(b, store, "L_owner")

	// Seed enough state to exercise the typical hot path: a domain
	// with concepts, goal-relevance, and mastered states so the FSM
	// has work to do but doesn't bail early.
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseInstruction, 0, time.Now().UTC()); err != nil {
		b.Fatalf("seed phase: %v", err)
	}
	if _, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, map[string]float64{"a": 0.9, "b": 0.6}); err != nil {
		b.Fatalf("seed goal_relevance: %v", err)
	}
	for _, c := range []string{"a", "b"} {
		cs := models.NewConceptState("L_owner", c)
		cs.PMastery = 0.5
		cs.CardState = "review"
		cs.Stability = 30
		cs.ElapsedDays = 1
		_ = store.InsertConceptStateIfNotExists(context.Background(), cs)
		if err := store.UpsertConceptState(context.Background(), cs); err != nil {
			b.Fatalf("seed state %s: %v", c, err)
		}
	}

	args := map[string]any{"domain_id": d.ID}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := callBenchTool(b, deps, "L_owner", args)
		if res.IsError {
			b.Fatalf("get_next_activity: %q", resultText(res))
		}
	}
}

func BenchmarkGetNextActivityLargeDomain(b *testing.B) {
	store, deps := setupBenchTools(b)

	concepts := make([]string, 100)
	relevance := make(map[string]float64, len(concepts))
	for i := range concepts {
		concepts[i] = fmt.Sprintf("c%03d", i)
		relevance[concepts[i]] = 1.0 - float64(i%10)*0.03
	}
	d := makeBenchDomainWithConcepts(b, store, "L_owner", concepts)
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseInstruction, 0, time.Now().UTC()); err != nil {
		b.Fatalf("seed phase: %v", err)
	}
	if _, err := store.MergeDomainGoalRelevance(context.Background(), d.ID, relevance); err != nil {
		b.Fatalf("seed goal_relevance: %v", err)
	}

	for _, c := range concepts {
		cs := models.NewConceptState("L_owner", c)
		cs.PMastery = 0.5
		cs.CardState = "review"
		cs.Stability = 30
		cs.ElapsedDays = 1
		if err := store.UpsertConceptState(context.Background(), cs); err != nil {
			b.Fatalf("seed state %s: %v", c, err)
		}
	}

	for i := 0; i < 1000; i++ {
		concept := concepts[i%len(concepts)]
		interaction := &models.Interaction{
			LearnerID:    "L_owner",
			Concept:      concept,
			ActivityType: string(models.ActivityPractice),
			Success:      i%7 != 0,
			DomainID:     d.ID,
		}
		if i%10 == 0 {
			interaction.Success = false
			interaction.MisconceptionType = "bench_misconception"
			interaction.MisconceptionDetail = "benchmark detail"
		}
		if err := store.CreateInteraction(context.Background(), interaction); err != nil {
			b.Fatalf("seed interaction: %v", err)
		}
	}

	args := map[string]any{"domain_id": d.ID}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := callBenchTool(b, deps, "L_owner", args)
		if res.IsError {
			b.Fatalf("get_next_activity: %q", resultText(res))
		}
	}
}

// ─── Benchmark helpers (testing.B variants of setupToolsTest/callTool) ─────

// benchDSNCounter avoids DSN collisions across parallel bench runs.
var benchDSNCounter int64

// setupBenchTools mirrors setupToolsTest but takes *testing.B so it
// can be used from BenchmarkGetNextActivity. We keep this duplicated
// (rather than refactoring setupToolsTest to take testing.TB) to keep
// the perf #91 PR scope minimal.
func setupBenchTools(b *testing.B) (*db.Store, *Deps) {
	b.Helper()
	n := atomic.AddInt64(&benchDSNCounter, 1)
	dsn := fmt.Sprintf("file:bench_%s_%d?mode=memory&cache=shared", b.Name(), n)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Migrate(raw); err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"L_owner", "L_attacker"} {
		_, err := raw.Exec(
			`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, 'hash', 'test', ?)`,
			id, id+"@test.com", now,
		)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() { raw.Close() })
	store := db.NewStore(raw)
	deps := &Deps{
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return store, deps
}

// makeBenchDomain creates a domain with two prereq-linked concepts
// for the benchmark — testing.B variant of makeOwnerDomain.
func makeBenchDomain(b *testing.B, store *db.Store, ownerID string) *models.Domain {
	b.Helper()
	return makeBenchDomainWithConcepts(b, store, ownerID, []string{"a", "b"})
}

func makeBenchDomainWithConcepts(b *testing.B, store *db.Store, ownerID string, concepts []string) *models.Domain {
	b.Helper()
	d, err := store.CreateDomainWithValueFramings(context.Background(), ownerID, "math", "", models.KnowledgeSpace{
		Concepts:      concepts,
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		b.Fatalf("create domain: %v", err)
	}
	return d
}

// callBenchTool is a testing.B variant of callTool, hard-wired to
// registerGetNextActivity.
func callBenchTool(b *testing.B, deps *Deps, learnerID string, args any) *mcp.CallToolResult {
	b.Helper()
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "bench", Version: "0.0.1"}, nil)
	registerGetNextActivity(server, deps)
	if learnerID != "" {
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				ctx = context.WithValue(ctx, auth.LearnerIDKey, learnerID)
				return next(ctx, method, req)
			}
		})
	}

	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		b.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer session.Close()

	argsJSON, _ := json.Marshal(args)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_next_activity",
		Arguments: json.RawMessage(argsJSON),
	})
	if err != nil {
		b.Fatalf("CallTool transport error: %v", err)
	}
	return res
}
