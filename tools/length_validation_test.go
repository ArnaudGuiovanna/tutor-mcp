// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"strings"
	"testing"
)

// Issue #31 reproducers: chat-side tools must reject pathologically long
// free-text payloads at the boundary instead of silently bloating the
// interactions / learners / affects tables. Each test posts a payload
// that exceeds the field's cap and expects an errorResult.

func TestRecordInteraction_RejectsOversizedNotes(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.5,
		"notes":                 strings.Repeat("x", maxNoteLen+1),
	})
	if !res.IsError {
		t.Fatalf("expected length-cap rejection, got %q", resultText(res))
	}
}

func TestRecordInteraction_RejectsOversizedConcept(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               strings.Repeat("c", maxShortLabelLen+1),
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.5,
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected length-cap rejection, got %q", resultText(res))
	}
}

func TestRecordInteraction_RejectsOversizedRubric(t *testing.T) {
	cases := []struct {
		name  string
		field string
	}{
		{name: "rubric", field: "rubric"},
		{name: "rubric_score", field: "rubric_score"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, deps := setupToolsTest(t)
			makeOwnerDomain(t, store, "L_owner", "math")

			args := map[string]any{
				"concept":               "a",
				"activity_type":         "RECALL_EXERCISE",
				"success":               true,
				"response_time_seconds": 5.0,
				"confidence":            0.5,
				"notes":                 "",
			}
			huge := strings.Repeat("x", maxLongTextLen+1)
			args[tc.field] = map[string]any{"criteria": []any{map[string]any{"id": "x", "max_score": 1, "description": huge}}}
			if tc.field == "rubric_score" {
				args[tc.field] = map[string]any{"criteria_scores": []any{map[string]any{"id": "x", "score": 1, "evidence": huge}}}
			}

			res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", args)
			if !res.IsError {
				t.Fatalf("expected length-cap rejection, got %q", resultText(res))
			}
			if !strings.Contains(resultText(res), tc.field) {
				t.Fatalf("expected error to mention %s, got %q", tc.field, resultText(res))
			}
		})
	}
}

// TestUpdateLearnerProfile_RejectsOversizedObjective swapped in for the
// previous oversized-background coverage after issue #61 dropped the
// `background` / `level` / `learning_style` fields from the tool surface.
// `objective` shares the same maxNoteLen cap, keeping the boundary covered.
func TestUpdateLearnerProfile_RejectsOversizedObjective(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"objective": strings.Repeat("o", maxNoteLen+1),
	})
	if !res.IsError {
		t.Fatalf("expected length-cap rejection, got %q", resultText(res))
	}
}

func TestRecordAffect_RejectsOversizedSessionID(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordAffect, "L_owner", "record_affect", map[string]any{
		"session_id": strings.Repeat("s", maxShortLabelLen+1),
	})
	if !res.IsError {
		t.Fatalf("expected length-cap rejection, got %q", resultText(res))
	}
}

func TestValidateString_PureBoundary(t *testing.T) {
	if err := validateString("f", "abc", 3); err != nil {
		t.Errorf("equal-to-max should pass, got %v", err)
	}
	if err := validateString("f", "abcd", 3); err == nil {
		t.Error("over-max should fail")
	}
	if err := validateString("f", "", 3); err != nil {
		t.Errorf("empty must always pass, got %v", err)
	}
}

// Issue #82 reproducers: cycle-2 string length caps for the remaining
// chat-side tools that take user-controlled free-text fields persisted to
// SQLite. Each sub-test posts a payload that exceeds the field's cap and
// expects an errorResult.

func TestRecordSessionClose_RejectsOversizedIntention(t *testing.T) {
	cases := []struct {
		name  string
		field string // key inside implementation_intention
		value string
	}{
		{"trigger", "trigger", strings.Repeat("t", maxNoteLen+1)},
		{"action", "action", strings.Repeat("a", maxNoteLen+1)},
		{"scheduled_for", "scheduled_for", strings.Repeat("s", maxShortLabelLen+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, deps := setupToolsTest(t)
			_ = makeOwnerDomain(t, store, "L_owner", "math")
			intention := map[string]any{
				"trigger": "demain matin",
				"action":  "fais un exo",
			}
			intention[tc.field] = tc.value
			res := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{
				"implementation_intention": intention,
			})
			if !res.IsError {
				t.Fatalf("expected length-cap rejection on %s, got %q", tc.field, resultText(res))
			}
		})
	}
}

func TestTransferChallenge_RejectsOversizedFields(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			"concept_id",
			map[string]any{"concept": strings.Repeat("c", maxShortLabelLen+1)},
		},
		{
			"context_type",
			map[string]any{"concept": "a", "context_type": strings.Repeat("x", maxShortLabelLen+1)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, deps := setupToolsTest(t)
			_ = makeOwnerDomain(t, store, "L_owner", "math")
			res := callTool(t, deps, registerTransferChallenge, "L_owner", "transfer_challenge", tc.args)
			if !res.IsError {
				t.Fatalf("expected length-cap rejection, got %q", resultText(res))
			}
		})
	}
}

func TestFeynmanChallenge_RejectsOversizedConceptID(t *testing.T) {
	store, deps := setupToolsTest(t)
	_ = makeOwnerDomain(t, store, "L_owner", "math")
	res := callTool(t, deps, registerFeynmanChallenge, "L_owner", "feynman_challenge", map[string]any{
		"concept": strings.Repeat("c", maxShortLabelLen+1),
	})
	if !res.IsError {
		t.Fatalf("expected length-cap rejection, got %q", resultText(res))
	}
}

func TestCalibrationCheck_RejectsOversizedFields(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			"concept_id",
			map[string]any{
				"concept":           strings.Repeat("c", maxShortLabelLen+1),
				"predicted_mastery": 3.0,
			},
		},
		{
			"domain_id",
			map[string]any{
				"concept":           "a",
				"predicted_mastery": 3.0,
				"domain_id":         strings.Repeat("d", maxShortLabelLen+1),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, deps := setupToolsTest(t)
			res := callTool(t, deps, registerCalibrationCheck, "L_owner", "calibration_check", tc.args)
			if !res.IsError {
				t.Fatalf("expected length-cap rejection, got %q", resultText(res))
			}
		})
	}
}

func TestLearningNegotiation_RejectsOversizedFields(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			"session_id",
			map[string]any{"session_id": strings.Repeat("s", maxShortLabelLen+1)},
		},
		{
			"learner_concept",
			map[string]any{
				"session_id":      "sess1",
				"learner_concept": strings.Repeat("c", maxShortLabelLen+1),
			},
		},
		{
			"learner_rationale",
			map[string]any{
				"session_id":        "sess1",
				"learner_rationale": strings.Repeat("r", maxNoteLen+1),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, deps := setupToolsTest(t)
			res := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", tc.args)
			if !res.IsError {
				t.Fatalf("expected length-cap rejection, got %q", resultText(res))
			}
		})
	}
}
