// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"tutor-mcp/models"
)

func TestRecordInteraction_BKTObservationPolicyAndAudit(t *testing.T) {
	for _, activity := range []models.ActivityType{
		models.ActivityDiagnosticAssessment, models.ActivityMasteryChallenge,
		models.ActivityTransferProbe, models.ActivityPractice,
	} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", activity, success), func(t *testing.T) {
				store, deps := setupToolsTest(t)
				domain := makeOwnerDomain(t, store, "L_owner", "bkt-policy")
				args := map[string]any{
					"domain_id": domain.ID, "concept": "a", "activity_type": string(activity),
					"success": success, "response_time_seconds": 20.0, "confidence": 0.7,
					"notes": "BKT observation policy regression",
					// A host's semantic claim cannot override the mechanical policy.
					"semantic_observation_json": `{"bkt_update_mode":"learning_opportunity","bkt_transition_applied":true}`,
				}
				if activity == models.ActivityTransferProbe {
					args["transfer_dimension"], args["transfer_score"] = "near", 0.0
					if success {
						args["transfer_score"] = 1.0
					}
				}
				res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", args)
				if res.IsError {
					t.Fatal(resultText(res))
				}
				obs := decodeResult(t, res)["observation"].(map[string]any)
				learning := activity == models.ActivityPractice
				wantMode := models.BKTObservationOnly
				wantLearn, wantForget := 0.0, 0.0
				if learning {
					wantMode, wantLearn, wantForget = models.BKTLearningOpportunity, 0.15, 0.05
				}
				wantPosterior := 1.0 / 73
				if success {
					wantPosterior = 1.0 / 3
				}
				wantAfter := wantPosterior*(1-wantForget) + (1-wantPosterior)*wantLearn
				if obs["bkt_update_mode"] != string(wantMode) || obs["bkt_transition_applied"] != learning {
					t.Fatalf("incorrect runtime policy: %v", obs)
				}
				params := obs["bkt_individualized_params"].(map[string]any)
				if params["p_learn"] != wantLearn || params["p_forget"] != wantForget {
					t.Fatalf("incorrect effective transition parameters: %v", params)
				}
				if math.Abs(obs["bkt_posterior"].(float64)-wantPosterior) > 1e-12 {
					t.Fatalf("incorrect observation posterior: %v", obs)
				}
				cs, err := store.GetConceptStateInDomain(context.Background(), "L_owner", domain.ID, "a")
				if err != nil {
					t.Fatal(err)
				}
				if math.Abs(cs.PMastery-wantAfter) > 1e-12 || cs.PLearn != 0.15 || cs.PForget != 0.05 {
					t.Fatalf("wrong persisted mastery or erased base parameters: %+v; want %.15f", cs, wantAfter)
				}
				// This policy changes BKT only; FSRS still records response exposure.
				if cs.Reps != 1 {
					t.Fatalf("FSRS exposure behavior changed: reps=%d", cs.Reps)
				}
				snapshots, err := store.GetPedagogicalSnapshots(context.Background(), "L_owner", domain.ID, "a", 5)
				if err != nil || len(snapshots) != 1 {
					t.Fatalf("snapshot count=%d err=%v", len(snapshots), err)
				}
				var persisted, decision, after map[string]any
				for _, pair := range []struct {
					raw string
					out *map[string]any
				}{{snapshots[0].ObservationJSON, &persisted}, {snapshots[0].DecisionJSON, &decision}, {snapshots[0].AfterJSON, &after}} {
					if err := json.Unmarshal([]byte(pair.raw), pair.out); err != nil {
						t.Fatal(err)
					}
				}
				for _, key := range []string{"bkt_update_mode", "bkt_transition_applied", "bkt_posterior", "bkt_transition_delta", "bkt_forget"} {
					if persisted[key] != obs[key] {
						t.Fatalf("%s missing or changed in audit: %v vs %v", key, persisted[key], obs[key])
					}
				}
				if persisted["bkt_learn"] != wantLearn || decision["policy_version"] != models.PedagogicalPolicyVersion {
					t.Fatalf("audit missing effective learning or policy version: %v / %v", persisted, decision)
				}
				reconstructed := persisted["bkt_posterior"].(float64) + persisted["bkt_transition_delta"].(float64)
				if math.Abs(reconstructed-after["p_mastery"].(float64)) > 1e-12 {
					t.Fatal("posterior + transition delta does not reconstruct persisted mastery")
				}
			})
		}
	}
}
