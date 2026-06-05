// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"
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
