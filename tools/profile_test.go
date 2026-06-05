// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"tutor-mcp/auth"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		"device":           "laptop",
		"objective":        "deep math",
		"language":         "fr",
		"calibration_bias": 0.15,
		"affect_baseline":  "calm",
		"autonomy_score":   0.6,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["updated"] != true {
		t.Fatalf("expected updated=true, got %v", out)
	}
	if out["fields_changed"].(float64) != 6 {
		t.Fatalf("expected 6 fields_changed, got %v", out["fields_changed"])
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
	for _, key := range []string{"level", "background", "learning_style"} {
		_, err := callToolRaw(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
			key: "x",
		})
		if err == nil {
			t.Fatalf("posting deprecated key %q should be rejected by JSON-schema validator, got nil error", key)
		}
		if !strings.Contains(err.Error(), "additional properties") {
			t.Errorf("posting deprecated key %q: expected 'additional properties' rejection, got %v", key, err)
		}
	}
}

// TestUpdateLearnerProfile_AllowsZeroCalibrationBias is the issue #89
// regression guard. `calibration_bias = 0` means *perfect calibration* — a
// legitimate value the system itself produces when predictions match
// outcomes. The previous `if params.CalibrationBias != 0` merge guard
// silently swallowed that value. With `*float64`, an explicit 0 must
// overwrite an existing non-zero value.
func TestUpdateLearnerProfile_AllowsZeroCalibrationBias(t *testing.T) {
	store, deps := setupToolsTest(t)
	// Seed an existing non-zero calibration_bias.
	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"calibration_bias": 0.5,
	})
	if res.IsError {
		t.Fatalf("seed call: %q", resultText(res))
	}

	// Now explicitly set to 0 — this must NOT be treated as "not provided".
	res2 := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"calibration_bias": 0,
	})
	if res2.IsError {
		t.Fatalf("zero call: %q", resultText(res2))
	}
	out := decodeResult(t, res2)
	if out["fields_changed"].(float64) < 1 {
		t.Fatalf("expected fields_changed >= 1, got %v", out["fields_changed"])
	}

	learner, err := store.GetLearnerByID(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(learner.ProfileJSON), &p)
	got, ok := p["calibration_bias"]
	if !ok {
		t.Fatalf("calibration_bias key missing from persisted profile: %v", p)
	}
	if v, _ := got.(float64); v != 0 {
		t.Fatalf("expected calibration_bias=0, got %v", got)
	}
}

// TestUpdateLearnerProfile_AllowsZeroAutonomyScore mirrors the above:
// `autonomy_score = 0` means *fully dependent*, legitimate at the start of
// a learning relationship.
func TestUpdateLearnerProfile_AllowsZeroAutonomyScore(t *testing.T) {
	store, deps := setupToolsTest(t)
	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"autonomy_score": 0.6,
	})
	if res.IsError {
		t.Fatalf("seed call: %q", resultText(res))
	}

	res2 := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"autonomy_score": 0,
	})
	if res2.IsError {
		t.Fatalf("zero call: %q", resultText(res2))
	}
	out := decodeResult(t, res2)
	if out["fields_changed"].(float64) < 1 {
		t.Fatalf("expected fields_changed >= 1, got %v", out["fields_changed"])
	}

	learner, err := store.GetLearnerByID(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(learner.ProfileJSON), &p)
	got, ok := p["autonomy_score"]
	if !ok {
		t.Fatalf("autonomy_score key missing from persisted profile: %v", p)
	}
	if v, _ := got.(float64); v != 0 {
		t.Fatalf("expected autonomy_score=0, got %v", got)
	}
}

// TestUpdateLearnerProfile_OmitsLeavesUnchanged guards against the
// dual-direction regression: a caller that does NOT supply
// calibration_bias/autonomy_score must leave existing values untouched.
func TestUpdateLearnerProfile_OmitsLeavesUnchanged(t *testing.T) {
	store, deps := setupToolsTest(t)
	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"calibration_bias": 0.42,
		"autonomy_score":   0.7,
	})
	if res.IsError {
		t.Fatalf("seed call: %q", resultText(res))
	}

	// Update an unrelated field; calibration_bias and autonomy_score must persist.
	res2 := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"device": "tablet",
	})
	if res2.IsError {
		t.Fatalf("unrelated update: %q", resultText(res2))
	}

	learner, err := store.GetLearnerByID(context.Background(), "L_owner")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(learner.ProfileJSON), &p)
	if v, _ := p["calibration_bias"].(float64); v != 0.42 {
		t.Fatalf("expected calibration_bias=0.42 preserved, got %v", p["calibration_bias"])
	}
	if v, _ := p["autonomy_score"].(float64); v != 0.7 {
		t.Fatalf("expected autonomy_score=0.7 preserved, got %v", p["autonomy_score"])
	}
}

// Issue #85 regression suite: the chat-side LLM was free to push NaN/Inf
// or wildly out-of-range floats into `calibration_bias` and `autonomy_score`.
// NaN cannot survive JSON marshalling but +Inf marshals to the literal `+Inf`
// which is invalid JSON and breaks every downstream consumer that reads
// profile_json (motivation brief, dashboard, get_olm_snapshot).
//
// Defense in depth runs at two layers:
//   1. JSON transport — Go's encoding/json refuses to marshal NaN/Inf and
//      refuses to unmarshal numeric literals that overflow float64. So a
//      well-formed JSON-RPC request *cannot* place NaN/Inf into the
//      params struct. Asserted via the malformedJSON tests below.
//   2. Handler validators — added in this commit. Catch out-of-range
//      finite values and any future code path that sets the params via
//      a non-JSON binding (gob, struct literal in a refactor, etc.).
//      Asserted via the OutOfRange tests below.
//
// Each guard test posts ONE bad value at a time so the rejection is
// per-field, not a happy-path collision.

// callUpdateProfileWithRawJSON is the only way to feed the handler payloads
// that encoding/json refuses to round-trip via map[string]any (NaN, +Inf,
// -Inf cannot be Marshal'd at all). We hand-build the request body so the
// transport layer sees the exact bytes the test asserts against.
func callUpdateProfileWithRawJSON(t *testing.T, deps *Deps, learnerID, rawArgs string) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	registerUpdateLearnerProfile(server, deps)
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
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	return session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "update_learner_profile",
		Arguments: json.RawMessage(rawArgs),
	})
}

// _ keeps math import live even when the integration tests use string
// payloads for NaN/Inf — the validators are still exercised with real
// math.NaN/math.Inf at the unit level (validate_test.go).
var _ = math.NaN

func TestUpdateLearnerProfile_RejectsNaNAutonomy(t *testing.T) {
	// JSON has no syntax for NaN; if a misbehaving client emits the
	// literal `NaN`, the transport must reject it before the handler runs.
	_, deps := setupToolsTest(t)
	_, err := callUpdateProfileWithRawJSON(t, deps, "L_owner", `{"autonomy_score": NaN}`)
	if err == nil {
		t.Fatal("expected transport error for NaN autonomy_score literal")
	}
}

func TestUpdateLearnerProfile_RejectsInfAutonomy(t *testing.T) {
	// Same for +Inf — not valid JSON, must not reach the merge.
	_, deps := setupToolsTest(t)
	_, err := callUpdateProfileWithRawJSON(t, deps, "L_owner", `{"autonomy_score": Infinity}`)
	if err == nil {
		t.Fatal("expected transport error for Infinity autonomy_score literal")
	}
}

func TestUpdateLearnerProfile_RejectsOutOfRangeAutonomy(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
		"autonomy_score": 1.5,
	})
	if !res.IsError {
		t.Fatalf("expected error for autonomy_score=1.5, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "autonomy_score") {
		t.Fatalf("error should mention autonomy_score, got %q", resultText(res))
	}
}

func TestUpdateLearnerProfile_RejectsNaNCalibrationBias(t *testing.T) {
	_, deps := setupToolsTest(t)
	_, err := callUpdateProfileWithRawJSON(t, deps, "L_owner", `{"calibration_bias": NaN}`)
	if err == nil {
		t.Fatal("expected transport error for NaN calibration_bias literal")
	}
}

func TestUpdateLearnerProfile_RejectsInfCalibrationBias(t *testing.T) {
	_, deps := setupToolsTest(t)
	_, err := callUpdateProfileWithRawJSON(t, deps, "L_owner", `{"calibration_bias": -Infinity}`)
	if err == nil {
		t.Fatal("expected transport error for -Infinity calibration_bias literal")
	}
}

func TestUpdateLearnerProfile_RejectsOutOfRangeCalibrationBias(t *testing.T) {
	_, deps := setupToolsTest(t)
	for _, v := range []float64{-2.0, 2.0} {
		res := callTool(t, deps, registerUpdateLearnerProfile, "L_owner", "update_learner_profile", map[string]any{
			"calibration_bias": v,
		})
		if !res.IsError {
			t.Fatalf("expected error for calibration_bias=%v, got %q", v, resultText(res))
		}
		if !strings.Contains(resultText(res), "calibration_bias") {
			t.Fatalf("error should mention calibration_bias, got %q", resultText(res))
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
		"level":          {},
		"background":     {},
		"learning_style": {},
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
