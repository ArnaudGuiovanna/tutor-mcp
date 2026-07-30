// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

// Issue #38: end-to-end regression guard for the BKT/FSRS/IRT update chain.
// The unit tests on each algorithm cover the math; this test exercises the
// full tool surface (`init_domain` → `get_next_activity` → `record_interaction`
// × N) so a re-ordering of the BKT/FSRS/IRT block in interaction_apply.go
// (which issue #8 explicitly warns is fragile) trips CI immediately.
//
// Acceptance: after 10 successful exercises chosen by the runtime, at least
// one concept's PMastery must move above the default 0.1 bootstrap. If this
// stalls at 0.1 the chain is broken — most commonly because IRT or FSRS
// stomps on cs.Reps/Difficulty before BKT had a chance to read them.
func TestEndToEnd_TenSuccessesMoveMastery(t *testing.T) {
	store, deps := setupToolsTest(t)

	if res := callTool(t, deps, registerInitDomain, "L", "init_domain", map[string]any{
		"name":          "e2e",
		"concepts":      []string{"a", "b", "c", "d", "e"},
		"prerequisites": map[string][]string{},
	}); res.IsError {
		t.Fatalf("init_domain failed: %s", resultText(res))
	}

	for i := 0; i < 10; i++ {
		actRes := callTool(t, deps, registerGetNextActivity, "L", "get_next_activity", map[string]any{})
		if actRes.IsError {
			t.Fatalf("iter %d: get_next_activity errored: %s", i, resultText(actRes))
		}
		out := decodeResult(t, actRes)
		activity, ok := out["activity"].(map[string]any)
		if !ok {
			t.Fatalf("iter %d: missing activity object in response: %v", i, out)
		}
		concept, _ := activity["concept"].(string)
		if concept == "" {
			t.Fatalf("iter %d: empty concept in activity: %v", i, activity)
		}
		recRes := callTool(t, deps, registerRecordInteraction, "L", "record_interaction", map[string]any{
			"concept":               concept,
			"activity_type":         "RECALL_EXERCISE",
			"success":               true,
			"response_time_seconds": 4.0,
			"confidence":            0.85,
			"notes":                 "",
		})
		if recRes.IsError {
			t.Fatalf("iter %d: record_interaction errored on concept=%s: %s", i, concept, resultText(recRes))
		}
	}

	movedConcepts := 0
	totalReps := 0
	for _, c := range []string{"a", "b", "c", "d", "e"} {
		cs, err := store.GetConceptState(context.Background(), "L", c)
		if err != nil || cs == nil {
			continue
		}
		totalReps += cs.Reps
		if cs.PMastery > 0.1 {
			movedConcepts++
		}
	}
	if movedConcepts == 0 {
		t.Fatal("no concept's PMastery moved above the 0.1 default after 10 successes — the BKT/FSRS/IRT chain is broken")
	}
	if totalReps != 10 {
		t.Errorf("expected sum(cs.Reps)=10 across all concepts (one per recorded success), got %d", totalReps)
	}
}

func TestEndToEnd_SharedConceptLabelsStayIsolatedByDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	ctx := context.Background()
	graph := models.KnowledgeSpace{
		Concepts:      []string{"functions"},
		Prerequisites: map[string][]string{},
	}
	programming, err := store.CreateDomain(ctx, "L_owner", "Programming", "", graph)
	if err != nil {
		t.Fatalf("create programming domain: %v", err)
	}
	mathematics, err := store.CreateDomain(ctx, "L_owner", "Mathematics", "", graph)
	if err != nil {
		t.Fatalf("create mathematics domain: %v", err)
	}

	now := time.Now().UTC()
	programmingState := models.NewConceptStateInDomain("L_owner", programming.ID, "functions")
	programmingState.CardState = "review"
	programmingState.Reps = 1
	programmingState.Stability = 30
	programmingState.LastReview = &now
	mathematicsState := models.NewConceptStateInDomain("L_owner", mathematics.ID, "functions")
	mathematicsState.CardState = "review"
	mathematicsState.Reps = 1
	mathematicsState.Stability = 0.2
	oldReview := now.Add(-90 * 24 * time.Hour)
	mathematicsState.LastReview = &oldReview
	for _, state := range []*models.ConceptState{programmingState, mathematicsState} {
		if err := store.UpsertConceptState(ctx, state); err != nil {
			t.Fatalf("seed scoped state: %v", err)
		}
	}

	recorded := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"domain_id":             programming.ID,
		"concept":               "functions",
		"activity_type":         "PRACTICE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
	})
	if recorded.IsError {
		t.Fatalf("record programming interaction: %s", resultText(recorded))
	}

	gotProgramming, err := store.GetConceptStateInDomain(ctx, "L_owner", programming.ID, "functions")
	if err != nil {
		t.Fatalf("read programming state: %v", err)
	}
	gotMathematics, err := store.GetConceptStateInDomain(ctx, "L_owner", mathematics.ID, "functions")
	if err != nil {
		t.Fatalf("read mathematics state: %v", err)
	}
	if gotProgramming.Reps != 2 || gotMathematics.Reps != 1 {
		t.Fatalf("cross-domain cognitive update: programming reps=%d mathematics reps=%d", gotProgramming.Reps, gotMathematics.Reps)
	}
	mathHistory, err := store.GetRecentInteractionsInDomain(ctx, "L_owner", mathematics.ID, "functions", 10)
	if err != nil {
		t.Fatalf("read mathematics history: %v", err)
	}
	if len(mathHistory) != 0 {
		t.Fatalf("programming evidence leaked into mathematics: %+v", mathHistory)
	}

	// With no explicit domain, the router must notice the critically forgotten
	// mathematics card even though the programming domain uses the same label.
	next := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{})
	if next.IsError {
		t.Fatalf("get next activity: %s", resultText(next))
	}
	out := decodeResult(t, next)
	if got := out["domain_id"]; got != mathematics.ID {
		t.Fatalf("critical-domain routing selected %v, want mathematics %q", got, mathematics.ID)
	}
}
