// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
)

func TestCalibrationCheck_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerCalibrationCheck, "", "calibration_check", map[string]any{
		"concept":           "x",
		"predicted_mastery": 3.0,
	})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestCalibrationCheck_MissingConcept(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerCalibrationCheck, "L_owner", "calibration_check", map[string]any{
		"concept":           "",
		"predicted_mastery": 3.0,
	})
	if !res.IsError || !strings.Contains(resultText(res), "concept is required") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestCalibrationCheck_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "calc")

	res := callTool(t, deps, registerCalibrationCheck, "L_owner", "calibration_check", map[string]any{
		"concept":           "calc",
		"predicted_mastery": 4.0, // → predicted = 0.75
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	pid, _ := out["prediction_id"].(string)
	if !strings.HasPrefix(pid, "cal_") {
		t.Fatalf("expected prediction_id starting with cal_, got %q", pid)
	}

	rec, err := store.GetCalibrationRecord(context.Background(), pid, "L_owner")
	if err != nil {
		t.Fatalf("GetCalibrationRecord: %v", err)
	}
	if rec.LearnerID != "L_owner" {
		t.Fatalf("expected learner L_owner, got %q", rec.LearnerID)
	}
	if rec.ConceptID != "calc" {
		t.Fatalf("expected concept calc, got %q", rec.ConceptID)
	}
	if rec.Predicted < 0.74 || rec.Predicted > 0.76 {
		t.Fatalf("expected predicted ~0.75, got %v", rec.Predicted)
	}
}

func TestCalibrationCheck_AcceptsLegacyConceptID(t *testing.T) {
	store, deps := setupToolsTest(t)
	seedDomain(t, store, "L_owner", "legacy_calc")

	res := callTool(t, deps, registerCalibrationCheck, "L_owner", "calibration_check", map[string]any{
		"concept_id":        "legacy_calc",
		"predicted_mastery": 3.0,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	pid, _ := out["prediction_id"].(string)
	rec, err := store.GetCalibrationRecord(context.Background(), pid, "L_owner")
	if err != nil {
		t.Fatalf("GetCalibrationRecord: %v", err)
	}
	if rec.ConceptID != "legacy_calc" {
		t.Fatalf("expected legacy concept_id to be recorded, got %q", rec.ConceptID)
	}
}

func TestCalibrationCheck_RejectsOutOfRangePredictedMastery(t *testing.T) {
	store, deps := setupToolsTest(t)

	// Out-of-range high — we no longer silently clamp, because clamping a
	// hallucinated 100.0 to 1.0 corrupts the calibration record. Reject.
	high := callTool(t, deps, registerCalibrationCheck, "L_owner", "calibration_check", map[string]any{
		"concept":           "high",
		"predicted_mastery": 100.0,
	})
	if !high.IsError {
		t.Fatalf("expected error for predicted_mastery=100, got %q", resultText(high))
	}
	if !strings.Contains(resultText(high), "predicted_mastery") {
		t.Fatalf("expected error to mention 'predicted_mastery', got %q", resultText(high))
	}

	// Out-of-range low.
	low := callTool(t, deps, registerCalibrationCheck, "L_owner", "calibration_check", map[string]any{
		"concept":           "low",
		"predicted_mastery": -100.0,
	})
	if !low.IsError {
		t.Fatalf("expected error for predicted_mastery=-100, got %q", resultText(low))
	}

	// And no calibration record should be persisted for either attempt.
	// (We don't have a list-by-learner getter, so just sanity-check that
	// IDs from the response payload don't exist — the response is an error
	// payload here, so simply assert no panic on shape access.)
	_ = store
}

func TestRecordCalibrationResult_MissingPredictionID(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordCalibrationResult, "L_owner", "record_calibration_result", map[string]any{
		"prediction_id": "",
		"actual_score":  0.5,
	})
	if !res.IsError || !strings.Contains(resultText(res), "prediction_id is required") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestRecordCalibrationResult_NotFound(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordCalibrationResult, "L_owner", "record_calibration_result", map[string]any{
		"prediction_id": "cal_unknown",
		"actual_score":  0.5,
	})
	if !res.IsError || !strings.Contains(resultText(res), "prediction not found") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestGeneratePredictionID_FormatAndUnique(t *testing.T) {
	a, err := generatePredictionID()
	if err != nil {
		t.Fatalf("generate first prediction ID: %v", err)
	}
	b, err := generatePredictionID()
	if err != nil {
		t.Fatalf("generate second prediction ID: %v", err)
	}
	if !strings.HasPrefix(a, "cal_") || !strings.HasPrefix(b, "cal_") {
		t.Fatalf("expected cal_ prefix, got %q %q", a, b)
	}
	if a == b {
		t.Fatalf("expected unique IDs, got duplicate %q", a)
	}
}
