// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"tutor-mcp/models"
)

const goldsetConcept = "a"

type staticGoldsetFile struct {
	Version   int                     `json:"version"`
	Scenarios []staticGoldsetScenario `json:"scenarios"`
}

type staticGoldsetScenario struct {
	Name               string                     `json:"name"`
	SetupPMastery      *float64                   `json:"setup_p_mastery,omitempty"`
	Records            []staticGoldsetRecord      `json:"records,omitempty"`
	ExpectCheckMastery *goldsetMasteryExpectation `json:"expect_check_mastery,omitempty"`
	ExpectReplay       *goldsetReplayExpectation  `json:"expect_replay,omitempty"`
}

type staticGoldsetRecord struct {
	ActivityType string `json:"activity_type"`
	Success      bool   `json:"success"`
	WithRubric   bool   `json:"with_rubric"`
	AgeHours     int    `json:"age_hours,omitempty"`
}

type goldsetMasteryExpectation struct {
	MasteryReady     bool   `json:"mastery_ready"`
	BKTMasteryReady  bool   `json:"bkt_mastery_ready"`
	EvidenceQuality  string `json:"evidence_quality"`
	UncertaintyLabel string `json:"uncertainty_label"`
}

type goldsetReplayExpectation struct {
	TotalSnapshots               int `json:"total_snapshots"`
	MissingRubricEvidenceCount   int `json:"missing_rubric_evidence_count"`
	TransferAfterMasteryGapCount int `json:"transfer_after_mastery_gap_count"`
	SnapshotJSONIssueCount       int `json:"snapshot_json_issue_count"`
}

func TestRuntimeGoldsetStatic(t *testing.T) {
	goldset := loadStaticGoldset(t)
	if goldset.Version != 1 {
		t.Fatalf("goldset version = %d, want 1", goldset.Version)
	}
	if len(goldset.Scenarios) == 0 {
		t.Fatal("goldset must contain at least one scenario")
	}

	for _, scenario := range goldset.Scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			store, deps := setupToolsTest(t)
			domain := makeOwnerDomain(t, store, "L_owner", "goldset")
			if scenario.SetupPMastery != nil {
				cs := models.NewConceptState("L_owner", goldsetConcept)
				cs.PMastery = *scenario.SetupPMastery
				if err := store.UpsertConceptState(context.Background(), cs); err != nil {
					t.Fatalf("%s: setup concept state: %v", scenario.Name, err)
				}
			}

			for i, record := range scenario.Records {
				args := map[string]any{
					"concept":               goldsetConcept,
					"activity_type":         record.ActivityType,
					"success":               record.Success,
					"response_time_seconds": 30.0,
					"confidence":            goldsetConfidence(record.Success),
					"notes":                 "static goldset runtime replay",
					"domain_id":             domain.ID,
				}
				if record.WithRubric {
					prepared := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
						"domain_id": domain.ID, "concept": goldsetConcept,
						"activity_type": record.ActivityType, "observable": "Goldset observable response",
						"task_text": "Frozen goldset task", "rubric_json": goldsetRubricJSON(),
					})
					if prepared.IsError {
						t.Fatalf("%s: prepare record %d failed: %s", scenario.Name, i, resultText(prepared))
					}
					preparedOut := decodeResult(t, prepared)
					attemptID, _ := preparedOut["attempt_id"].(string)
					sessionID, _ := preparedOut["session_id"].(string)
					submitted := callTool(t, deps, registerSubmitAssessmentAttempt, "L_owner", "submit_assessment_attempt", map[string]any{
						"attempt_id": attemptID, "learner_response": "Committed goldset response",
					})
					if submitted.IsError {
						t.Fatalf("%s: submit record %d failed: %s", scenario.Name, i, resultText(submitted))
					}
					args["attempt_id"] = attemptID
					args["session_id"] = sessionID
					args["evaluator_id"] = "goldset-host-v1"
					args["evaluation_method"] = "host_llm"
					args["rubric_score_json"] = goldsetRubricScoreJSON(record.Success)
				}
				if record.ActivityType == string(models.ActivityTransferProbe) {
					args["transfer_dimension"] = "far"
					args["transfer_score"] = goldsetTransferScore(record.Success)
				}
				res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", args)
				if res.IsError {
					t.Fatalf("%s: record %d failed: %s", scenario.Name, i, resultText(res))
				}
				if record.AgeHours > 0 {
					at := time.Now().UTC().Add(-time.Duration(record.AgeHours) * time.Hour)
					if _, err := store.RawDB().Exec(`UPDATE interactions SET created_at = ?
						WHERE learner_id = ? AND domain_id = ? AND concept = ? AND activity_type = ?`,
						at,
						"L_owner", domain.ID, goldsetConcept, record.ActivityType); err != nil {
						t.Fatalf("%s: backdate record %d: %v", scenario.Name, i, err)
					}
					if record.WithRubric {
						// A historical interaction also needs a historical committed
						// response. Grading time alone cannot establish a recall gap.
						if _, err := store.RawDB().Exec(`UPDATE assessment_attempts
							SET created_at = ?, submitted_at = ?, evaluated_at = ? WHERE id = ?`,
							at.Add(-2*time.Minute), at.Add(-time.Minute), at, args["attempt_id"]); err != nil {
							t.Fatalf("%s: backdate assessment %d: %v", scenario.Name, i, err)
						}
					}
				}
			}

			if scenario.ExpectCheckMastery != nil {
				assertGoldsetCheckMastery(t, scenario.Name, deps, domain.ID, *scenario.ExpectCheckMastery)
			}
			if scenario.ExpectReplay != nil {
				assertGoldsetReplay(t, scenario.Name, deps, domain.ID, *scenario.ExpectReplay)
			}
		})
	}
}

func loadStaticGoldset(t *testing.T) staticGoldsetFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/goldset_static.json")
	if err != nil {
		t.Fatalf("read static goldset: %v", err)
	}
	var goldset staticGoldsetFile
	if err := json.Unmarshal(raw, &goldset); err != nil {
		t.Fatalf("parse static goldset: %v", err)
	}
	return goldset
}

func assertGoldsetCheckMastery(t *testing.T, name string, deps *Deps, domainID string, want goldsetMasteryExpectation) {
	t.Helper()
	res := callTool(t, deps, registerCheckMastery, "L_owner", "check_mastery", map[string]any{
		"concept":   goldsetConcept,
		"domain_id": domainID,
	})
	if res.IsError {
		t.Fatalf("%s: check_mastery failed: %s", name, resultText(res))
	}
	out := decodeResult(t, res)
	assertGoldsetBool(t, name, "mastery_ready", out["mastery_ready"], want.MasteryReady)
	assertGoldsetBool(t, name, "bkt_mastery_ready", out["bkt_mastery_ready"], want.BKTMasteryReady)

	if want.EvidenceQuality != "" {
		evidenceQuality, ok := out["evidence_quality"].(map[string]any)
		if !ok {
			t.Fatalf("%s: evidence_quality = %T, want object", name, out["evidence_quality"])
		}
		if got := evidenceQuality["quality"]; got != want.EvidenceQuality {
			t.Fatalf("%s: evidence_quality.quality = %v, want %s", name, got, want.EvidenceQuality)
		}
	}
	if want.UncertaintyLabel != "" {
		uncertainty, ok := out["mastery_uncertainty"].(map[string]any)
		if !ok {
			t.Fatalf("%s: mastery_uncertainty = %T, want object", name, out["mastery_uncertainty"])
		}
		if got := uncertainty["confidence_label"]; got != want.UncertaintyLabel {
			t.Fatalf("%s: mastery_uncertainty.confidence_label = %v, want %s", name, got, want.UncertaintyLabel)
		}
	}
	t.Logf("check_mastery: mastery_ready=%v bkt_mastery_ready=%v evidence_quality=%v uncertainty_label=%v",
		out["mastery_ready"],
		out["bkt_mastery_ready"],
		nestedString(out, "evidence_quality", "quality"),
		nestedString(out, "mastery_uncertainty", "confidence_label"),
	)
}

func assertGoldsetReplay(t *testing.T, name string, deps *Deps, domainID string, want goldsetReplayExpectation) {
	t.Helper()
	res := callTool(t, deps, registerGetDecisionReplaySummary, "L_owner", "get_decision_replay_summary", map[string]any{
		"domain_id": domainID,
		"concept":   goldsetConcept,
		"limit":     100,
	})
	if res.IsError {
		t.Fatalf("%s: get_decision_replay_summary failed: %s", name, resultText(res))
	}
	out := decodeResult(t, res)
	summary, ok := out["summary"].(map[string]any)
	if !ok {
		t.Fatalf("%s: summary = %T, want object", name, out["summary"])
	}
	assertGoldsetNumber(t, name, "total_snapshots", summary["total_snapshots"], want.TotalSnapshots)
	assertGoldsetNumber(t, name, "missing_rubric_evidence_count", summary["missing_rubric_evidence_count"], want.MissingRubricEvidenceCount)
	assertGoldsetNumber(t, name, "transfer_after_mastery_gap_count", summary["transfer_after_mastery_gap_count"], want.TransferAfterMasteryGapCount)
	assertGoldsetNumber(t, name, "snapshot_json_issue_count", summary["snapshot_json_issue_count"], want.SnapshotJSONIssueCount)
	t.Logf("replay: total_snapshots=%v missing_rubric=%v transfer_gap=%v json_issues=%v",
		summary["total_snapshots"],
		summary["missing_rubric_evidence_count"],
		summary["transfer_after_mastery_gap_count"],
		summary["snapshot_json_issue_count"],
	)
}

func assertGoldsetBool(t *testing.T, scenario, field string, got any, want bool) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: %s = %v, want %v", scenario, field, got, want)
	}
}

func assertGoldsetNumber(t *testing.T, scenario, field string, got any, want int) {
	t.Helper()
	if got != float64(want) {
		t.Fatalf("%s: %s = %v, want %d", scenario, field, got, want)
	}
}

func nestedString(root map[string]any, objectKey, valueKey string) any {
	obj, ok := root[objectKey].(map[string]any)
	if !ok {
		return nil
	}
	return obj[valueKey]
}

func goldsetConfidence(success bool) float64 {
	if success {
		return 0.9
	}
	return 0.2
}

func goldsetTransferScore(success bool) float64 {
	if success {
		return 0.85
	}
	return 0.2
}

func goldsetRubricJSON() string {
	return `{"criteria":[{"id":"correctness","max_score":1},{"id":"reasoning","max_score":1}],"passing_score":1.5}`
}

func goldsetRubricScoreJSON(success bool) string {
	if success {
		return `{"criteria_scores":[{"id":"correctness","score":1,"evidence":"expected result"},{"id":"reasoning","score":1,"evidence":"clear reasoning"}],"summary":"goldset pass","confidence":0.95}`
	}
	return `{"criteria_scores":[{"id":"correctness","score":0,"evidence":"incorrect result"},{"id":"reasoning","score":0,"evidence":"missing reasoning"}],"summary":"goldset fail","confidence":0.95}`
}
