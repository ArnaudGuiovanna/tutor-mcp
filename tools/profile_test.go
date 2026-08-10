// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateLearnerProfile_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerUpdateLearnerProfile, "", "update_learner_profile", map[string]any{"device": "laptop"})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestUpdateLearnerProfile_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)
	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"device":          "laptop",
		"objective":       "deep math",
		"language":        "fr",
		"affect_baseline": "calm",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["updated"] != true {
		t.Fatalf("expected updated=true, got %v", out)
	}
	if out["fields_changed"].(float64) != 4 {
		t.Fatalf("expected 4 fields_changed, got %v", out["fields_changed"])
	}

	// DB state: profile is persisted.
	learner, err := store.GetLearnerByID(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(learner.ProfileJSON), &p); err != nil {
		t.Fatalf("bad profile JSON: %v", err)
	}
	if p["device"] != "laptop" || p["language"] != "fr" {
		t.Fatalf("unexpected profile: %v", p)
	}
	if _, duplicated := p["objective"]; duplicated {
		t.Fatalf("objective must have one canonical source, got profile %v", p)
	}
	if learner.Objective != "deep math" {
		t.Fatalf("canonical objective = %q, want deep math", learner.Objective)
	}
}

func TestUpdateLearnerProfile_PartialUpdatePreservesExisting(t *testing.T) {
	store, deps := setupToolsTest(t)

	// First call: sets device + language.
	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"device":   "phone",
		"language": "fr",
	})
	if res.IsError {
		t.Fatalf("first call: %q", resultText(res))
	}

	// Second call: only updates language — device should be preserved.
	res2 := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"language": "en",
	})
	if res2.IsError {
		t.Fatalf("second call: %q", resultText(res2))
	}

	learner, err := store.GetLearnerByID(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(learner.ProfileJSON), &p)
	if p["device"] != "phone" {
		t.Fatalf("device should be preserved, got %v", p["device"])
	}
	if p["language"] != "en" {
		t.Fatalf("language should be updated, got %v", p["language"])
	}
}

func TestUpdateLearnerProfile_NoFieldsProvided(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["fields_changed"].(float64) != 0 {
		t.Fatalf("expected 0 fields_changed, got %v", out["fields_changed"])
	}
}

func TestUpdateLearnerProfile_CorruptStoredProfileIsNotOverwritten(t *testing.T) {
	store, deps := setupToolsTest(t)
	if _, err := store.RawDB().ExecContext(context.Background(),
		`UPDATE learners SET profile_json = ? WHERE id = ?`, "{broken", "L_owner"); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"language": "fr",
	})
	if !res.IsError || !strings.Contains(resultText(res), "invalid") {
		t.Fatalf("expected stored-profile integrity error, got %q", resultText(res))
	}
	learner, err := store.GetLearnerByID(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	if learner.ProfileJSON != "{broken" {
		t.Fatalf("corrupt profile was overwritten: %q", learner.ProfileJSON)
	}
}

// TestUpdateLearnerProfile_DroppedFieldsRejected is the issue #61 regression
// guard. The deprecated fields `level`, `background` and `learning_style`
// were write-only with no consumer (no read site in motivation, concept
// selection, alerts or dashboard). After their removal from
// UpdateLearnerProfileParams, the SDK's JSON-Schema input validator rejects
// posts that include any of them with an `unexpected additional properties`
// transport error — so a stale client cannot smuggle them back into
// profile_json.
//
// We try each deprecated key in isolation to assert the rejection is
// per-field, not just a happy-path collision with one specific key.
func TestUpdateLearnerProfile_DroppedFieldsRejected(t *testing.T) {
	_, deps := setupToolsTest(t)
	for _, key := range []string{"level", "background", "learning_style", "calibration_bias", "autonomy_score"} {
		res, err := callToolRaw(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
			key: "x",
		})
		if err != nil {
			t.Fatalf("posting deprecated key %q returned transport error: %v", key, err)
		}
		if res == nil || !res.IsError || !strings.Contains(resultText(res), "additional properties") {
			t.Errorf("posting deprecated key %q: expected additional-properties tool error, got %+v", key, res)
		}
	}
}

// TestUpdateLearnerProfile_DroppedKeysAbsentFromInputSchema is a structural
// guard: even if the SDK relaxes additionalProperties enforcement in the
// future, the param struct itself must not carry these JSON tags. We
// reflect over UpdateLearnerProfileParams and assert no field maps to any
// of the three deprecated JSON keys.
func TestUpdateLearnerProfile_DroppedKeysAbsentFromInputSchema(t *testing.T) {
	dropped := map[string]struct{}{
		"level":            {},
		"background":       {},
		"learning_style":   {},
		"calibration_bias": {},
		"autonomy_score":   {},
	}
	rt := reflect.TypeOf(UpdateLearnerProfileParams{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		// strip ",omitempty" suffix
		if idx := strings.Index(tag, ","); idx >= 0 {
			tag = tag[:idx]
		}
		if _, bad := dropped[tag]; bad {
			t.Errorf("UpdateLearnerProfileParams.%s carries deprecated JSON tag %q (issue #61)", rt.Field(i).Name, tag)
		}
	}
}
