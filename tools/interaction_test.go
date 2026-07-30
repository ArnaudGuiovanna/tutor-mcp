// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/db"
	"tutor-mcp/models"

	_ "modernc.org/sqlite"
)

// setupFileBackedToolsTest opens a real file-based SQLite DB with the
// production DSN (WAL + busy_timeout + foreign_keys). Required for
// concurrency tests because shared in-memory cache mode raises
// SQLITE_LOCKED on conflicting writers (instead of SQLITE_BUSY) and
// SQLITE_LOCKED is not retried by the busy handler — making the
// in-memory helper unfit for real contention scenarios.
func setupFileBackedToolsTest(t *testing.T) (*db.Store, *Deps) {
	t.Helper()
	tempDir := t.TempDir()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_txlock=immediate", filepath.Join(tempDir, "test.db"))
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"L_owner", "L_attacker"} {
		if _, err := raw.Exec(
			`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, 'hash', 'test', ?)`,
			id, id+"@test.com", now,
		); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { raw.Close() })
	store := db.NewStore(raw)
	deps := &Deps{
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return store, deps
}

// TestRecordInteraction_NoLostUpdateUnderConcurrency reproduces R008.
// N concurrent applyInteraction calls on the same (learner, concept) must
// all be reflected in the final concept_states row. Without a transaction
// guarding the read-modify-write cycle on PMastery / reps, concurrent
// upserts silently lose updates (verified empirically: 94.5% loss under
// 50 goroutines × 20 iterations on this same in-memory DB shape).
//
// Oracle: after N parallel successes from a fresh state, reps must equal
// the number of successful applyInteraction calls (FSRS bumps reps by 1
// per ReviewCard) and PMastery must have moved up from the default 0.1.
//
// N is chosen large enough to make the race observable at N=1 -count of
// the test: at N=50 the post-fix invariant fails reliably (well under 1%
// flake rate from manual repetition on shared in-memory SQLite).
func TestRecordInteraction_NoLostUpdateUnderConcurrency(t *testing.T) {
	store, deps := setupFileBackedToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math") // concepts: ["a","b"]

	const N = 50
	var wg sync.WaitGroup
	errs := make(chan error, N)
	now := time.Now().UTC()
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // release all goroutines together to maximise overlap
			input := interactionInput{
				Concept:             "a",
				ActivityType:        "RECALL_EXERCISE",
				Success:             true,
				ResponseTimeSeconds: 5.0,
				Confidence:          0.8,
				DomainID:            domain.ID,
			}
			if _, _, err := applyInteraction(context.Background(), deps, "L_owner", input, now); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("applyInteraction failed: %v", err)
	}

	// All N interactions must be persisted.
	recents, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", N+10)
	if err != nil {
		t.Fatalf("GetRecentInteractionsByLearner: %v", err)
	}
	count := 0
	for _, r := range recents {
		if r.Concept == "a" {
			count++
		}
	}
	if count != N {
		t.Fatalf("expected %d interactions on 'a', got %d", N, count)
	}

	// Concept state must reflect every update (lost-update detector).
	cs, err := store.GetConceptStateInDomain(context.Background(), "L_owner", domain.ID, "a")
	if err != nil {
		t.Fatalf("GetConceptState: %v", err)
	}
	if cs.Reps != N {
		t.Fatalf("lost update: expected reps=%d, got reps=%d (concept_states overwritten by concurrent record_interaction)", N, cs.Reps)
	}
	if cs.PMastery <= 0.1 {
		t.Fatalf("expected PMastery > 0.1 after %d successes, got %f", N, cs.PMastery)
	}
}

// Compile-time guard: make sure the BKT heuristic is exposed under its new
// renamed name. If a future refactor reverts the rename this file refuses
// to build (issue #51).
var _ = algorithms.BKTUpdateHeuristicSlipByErrorType

func TestRecordInteraction_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordInteraction, "", "record_interaction", map[string]any{
		"concept":               "x",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestRecordInteraction_MissingConcept(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
	})
	if !res.IsError || !strings.Contains(resultText(res), "concept is required") {
		t.Fatalf("got %q", resultText(res))
	}
}

func TestRecordInteraction_HappyPath_Success(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math") // concepts: ["a","b"]

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 12.0,
		"confidence":            0.9,
		"notes":                 "great",
		"hints_requested":       1,
		"self_initiated":        true,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["updated"] != true {
		t.Fatalf("expected updated=true, got %v", out)
	}
	if _, ok := out["new_mastery"]; !ok {
		t.Fatalf("expected new_mastery key, got %v", out)
	}
	if out["engagement_signal"] != "positive" {
		t.Fatalf("expected positive engagement (success+conf>=0.8), got %v", out["engagement_signal"])
	}

	// DB: interaction created.
	recents, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recents) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(recents))
	}
	if !recents[0].Success || recents[0].Concept != "a" {
		t.Fatalf("unexpected interaction: %+v", recents[0])
	}

	// DB: concept state upserted.
	cs, err := store.GetConceptState(context.Background(), "L_owner", "a")
	if err != nil {
		t.Fatalf("expected concept state: %v", err)
	}
	if cs.Reps == 0 {
		t.Fatalf("expected reps to be incremented, got %d", cs.Reps)
	}
}

func TestRecordInteraction_ReturnsRubricObservation(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 12.0,
		"confidence":            0.9,
		"notes":                 "rubric scored",
		"rubric_json":           `{"scale":"0-1","criteria":[{"id":"correctness","weight":0.7}]}`,
		"rubric_score_json":     `{"overall":0.8,"criteria_scores":{"correctness":0.8}}`,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	out := decodeResult(t, res)
	obs, ok := out["observation"].(map[string]any)
	if !ok {
		t.Fatalf("expected observation map, got %v", out["observation"])
	}
	rubric, ok := obs["rubric"].(map[string]any)
	if !ok {
		t.Fatalf("expected parsed rubric object, got %v", obs["rubric"])
	}
	if _, ok := rubric["criteria"].([]any); !ok {
		t.Fatalf("expected canonical rubric criteria, got %v", rubric)
	}
	score, ok := obs["rubric_score"].(map[string]any)
	if !ok {
		t.Fatalf("expected parsed rubric_score object, got %v", obs["rubric_score"])
	}
	if got, _ := score["total"].(float64); got != 0.8 {
		t.Fatalf("rubric_score total = %v, want 0.8", score["total"])
	}
	if _, ok := score["criteria_scores"].([]any); !ok {
		t.Fatalf("expected canonical criteria_scores, got %v", score)
	}
	if _, ok := obs["rubric_schema_warnings"].([]any); !ok {
		t.Fatalf("expected rubric schema warnings, got %v", obs)
	}
	recents, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 1)
	if err != nil {
		t.Fatalf("get interactions: %v", err)
	}
	if len(recents) != 1 {
		t.Fatalf("got %d interactions, want 1", len(recents))
	}
	if recents[0].RubricJSON == "" || recents[0].RubricScoreJSON == "" {
		t.Fatalf("expected rubric JSON to be persisted on interaction: %+v", recents[0])
	}
	snapshots, err := store.GetPedagogicalSnapshots(context.Background(), "L_owner", recents[0].DomainID, "a", 5)
	if err != nil {
		t.Fatalf("get snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snapshots))
	}
	if !strings.Contains(snapshots[0].ObservationJSON, `"rubric_score"`) {
		t.Fatalf("snapshot observation should include rubric_score, got %s", snapshots[0].ObservationJSON)
	}
}

func TestRecordInteraction_PersistsInterpretationBrief(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 12.0,
		"confidence":            0.9,
		"notes":                 "## Interpretation brief\nThe learner likely knows the formula but not the transfer cue.\n\n## Activity\nShort recall task.",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	snapshots, err := store.GetPedagogicalSnapshots(context.Background(), "L_owner", "", "a", 5)
	if err != nil {
		t.Fatalf("get snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snapshots))
	}
	if snapshots[0].InterpretationBrief != "The learner likely knows the formula but not the transfer cue." {
		t.Fatalf("brief = %q", snapshots[0].InterpretationBrief)
	}
}

func TestRecordInteraction_ReturnsSemanticObservation(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":                   "a",
		"activity_type":             "RECALL_EXERCISE",
		"success":                   true,
		"response_time_seconds":     12.0,
		"confidence":                0.9,
		"notes":                     "semantic audit",
		"semantic_observation_json": `{"reasoning_quality":"brittle","success_mode":"procedural_without_explanation","confidence_alignment":"overconfident"}`,
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	out := decodeResult(t, res)
	obs, ok := out["observation"].(map[string]any)
	if !ok {
		t.Fatalf("expected observation map, got %v", out["observation"])
	}
	semantic, ok := obs["semantic_observation"].(map[string]any)
	if !ok {
		t.Fatalf("expected parsed semantic_observation object, got %v", obs["semantic_observation"])
	}
	if got := semantic["reasoning_quality"]; got != "brittle" {
		t.Fatalf("reasoning_quality = %v, want brittle", got)
	}

	recents, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 1)
	if err != nil {
		t.Fatalf("get interactions: %v", err)
	}
	if len(recents) != 1 {
		t.Fatalf("got %d interactions, want 1", len(recents))
	}
	snapshots, err := store.GetPedagogicalSnapshots(context.Background(), "L_owner", recents[0].DomainID, "a", 5)
	if err != nil {
		t.Fatalf("get snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snapshots))
	}
	if !strings.Contains(snapshots[0].ObservationJSON, `"semantic_observation"`) ||
		!strings.Contains(snapshots[0].ObservationJSON, `"reasoning_quality":"brittle"`) {
		t.Fatalf("snapshot observation should include semantic_observation, got %s", snapshots[0].ObservationJSON)
	}
}

func TestRecordInteraction_ReturnsPedagogicalModelObservation(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 9.0,
		"confidence":            0.8,
		"notes":                 "model audit",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	out := decodeResult(t, res)
	obs, ok := out["observation"].(map[string]any)
	if !ok {
		t.Fatalf("expected observation map, got %v", out["observation"])
	}
	if _, ok := obs["bkt_individualized_profile"].(map[string]any); !ok {
		t.Fatalf("expected individualized BKT profile in observation, got %v", obs)
	}
	params, ok := obs["bkt_individualized_params"].(map[string]any)
	if !ok {
		t.Fatalf("expected individualized BKT params in observation, got %v", obs)
	}
	if _, ok := params["p_learn"].(float64); !ok {
		t.Fatalf("expected p_learn in individualized BKT params, got %v", params)
	}
	rasch, ok := obs["rasch_elo"].(map[string]any)
	if !ok {
		t.Fatalf("expected rasch_elo in observation, got %v", obs)
	}
	if _, ok := rasch["success_probability_before"].(float64); !ok {
		t.Fatalf("expected success_probability_before in rasch_elo, got %v", rasch)
	}
	if _, ok := obs["semantic_observation"]; ok {
		t.Fatalf("semantic_observation should be absent when semantic_observation_json is omitted: %v", obs)
	}

	recents, err := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 1)
	if err != nil || len(recents) != 1 {
		t.Fatalf("expected persisted interaction, got len=%d err=%v", len(recents), err)
	}
	snapshots, err := store.GetPedagogicalSnapshots(context.Background(), "L_owner", recents[0].DomainID, "a", 5)
	if err != nil {
		t.Fatalf("get snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snapshots))
	}
	if !strings.Contains(snapshots[0].ObservationJSON, `"rasch_elo"`) ||
		!strings.Contains(snapshots[0].ObservationJSON, `"bkt_learn"`) {
		t.Fatalf("snapshot observation should include model signals, got %s", snapshots[0].ObservationJSON)
	}
	if strings.Contains(snapshots[0].ObservationJSON, `"semantic_observation"`) {
		t.Fatalf("snapshot observation should omit semantic_observation when absent, got %s", snapshots[0].ObservationJSON)
	}
}

func TestRecordInteraction_RejectsInvalidRubricJSON(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
		"rubric_json":           `{"criteria":`,
	})
	if !res.IsError {
		t.Fatalf("expected rubric_json validation error, got %q", resultText(res))
	}
	msg := resultText(res)
	if !strings.Contains(msg, "rubric_json") || !strings.Contains(msg, "valid JSON") {
		t.Fatalf("expected rubric_json JSON error, got %q", msg)
	}

	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
	if len(recents) != 0 {
		t.Fatalf("expected no interactions persisted on bad rubric_json, got %d", len(recents))
	}
}

func TestRecordInteraction_RejectsNegativeScalarRubricScoreJSON(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
		"rubric_score_json":     `-0.8`,
	})
	if !res.IsError {
		t.Fatalf("expected rubric_score_json validation error, got %q", resultText(res))
	}
	msg := resultText(res)
	if !strings.Contains(msg, "rubric_score_json") || !strings.Contains(msg, "non-negative") {
		t.Fatalf("expected structured rubric_score_json error, got %q", msg)
	}
}

func TestRecordInteraction_RejectsInvalidSemanticObservationJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "malformed", raw: `{"reasoning_quality":`, want: "valid JSON"},
		{name: "non_object", raw: `["brittle"]`, want: "JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, deps := setupToolsTest(t)
			makeOwnerDomain(t, store, "L_owner", "math")

			res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
				"concept":                   "a",
				"activity_type":             "RECALL_EXERCISE",
				"success":                   true,
				"response_time_seconds":     5.0,
				"confidence":                0.8,
				"notes":                     "",
				"semantic_observation_json": tc.raw,
			})
			if !res.IsError {
				t.Fatalf("expected semantic_observation_json validation error, got %q", resultText(res))
			}
			msg := resultText(res)
			if !strings.Contains(msg, "semantic_observation_json") || !strings.Contains(msg, tc.want) {
				t.Fatalf("expected semantic_observation_json %q error, got %q", tc.want, msg)
			}

			recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
			if len(recents) != 0 {
				t.Fatalf("expected no interactions persisted on bad semantic_observation_json, got %d", len(recents))
			}
		})
	}
}

func TestRecordInteraction_FailureDecliningSignal(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               false,
		"response_time_seconds": 30.0,
		"confidence":            0.1,
		"notes":                 "",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["engagement_signal"] != "declining" {
		t.Fatalf("expected declining engagement, got %v", out["engagement_signal"])
	}
}

func TestRecordInteraction_StoresMisconceptionOnFailure(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               false,
		"response_time_seconds": 20.0,
		"confidence":            0.4,
		"notes":                 "",
		"misconception_type":    "off_by_one",
		"misconception_detail":  "uses < instead of <=",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
	if len(recents) == 0 {
		t.Fatal("no interactions recorded")
	}
	got := recents[0]
	if got.MisconceptionType != "off_by_one" {
		t.Fatalf("misconception not stored: %+v", got)
	}
}

func TestRecordInteraction_MisconceptionIgnoredOnSuccess(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.7,
		"notes":                 "",
		"misconception_type":    "off_by_one",
		"misconception_detail":  "ignored",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
	if len(recents) == 0 {
		t.Fatal("no interactions")
	}
	if recents[0].MisconceptionType != "" {
		t.Fatalf("misconception should NOT be stored on success: %+v", recents[0])
	}
}

// Issue #24: domain_id parameter must be honored — i.e. resolved (not silently
// ignored) and persisted on the interaction row so audits can tell apart
// progress on the same concept name across two domains.
func TestRecordInteraction_DomainIDIsPersistedOnRow(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"domain_id":             d.ID,
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 4.0,
		"confidence":            0.7,
		"notes":                 "",
	})
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(res))
	}

	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 1)
	if len(recents) != 1 {
		t.Fatalf("expected 1 interaction row, got %d", len(recents))
	}
	if got := recents[0].DomainID; got != d.ID {
		t.Errorf("DomainID = %q, want %q", got, d.ID)
	}
}

// Issue #24: an explicit domain_id pointing at someone else's domain must be
// rejected — concept membership validation runs against the resolved domain
// and a foreign domain has no overlap with the learner's concept set.
func TestRecordInteraction_DomainIDRejectsForeignDomain(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")
	foreign, err := store.CreateDomain(context.Background(), "L_other", "shared", "", models.KnowledgeSpace{
		Concepts: []string{"a"},
	})
	if err != nil {
		t.Fatalf("create foreign domain: %v", err)
	}

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"domain_id":             foreign.ID,
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 4.0,
		"confidence":            0.7,
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected errorResult on foreign domain_id, got %q", resultText(res))
	}
}

func TestRecordInteraction_RejectsUnknownConcept(t *testing.T) {
	store, deps := setupToolsTest(t)
	// makeOwnerDomain creates a domain with concepts ["a","b"].
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "ghost",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected error for unknown concept, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "ghost") {
		t.Fatalf("expected error to mention the unknown concept name, got %q", resultText(res))
	}

	// And no orphan ConceptState row should have been created.
	if cs, err := store.GetConceptState(context.Background(), "L_owner", "ghost"); err == nil && cs != nil {
		t.Fatalf("orphan concept_state row created for unknown concept: %+v", cs)
	}
}

func TestRecordInteraction_NoActiveDomain(t *testing.T) {
	_, deps := setupToolsTest(t)
	// L_owner has no domain at all — record_interaction must signal
	// needs_domain_setup (issue #33: uniform shape across chat-side tools).
	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "anything",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
	})
	out := decodeResult(t, res)
	if got, _ := out["needs_domain_setup"].(bool); !got {
		t.Fatalf("expected needs_domain_setup=true, got %v (raw %q)", out, resultText(res))
	}
}

func TestRecordInteraction_RejectsOutOfRangeConfidence(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            2.5, // out-of-range; legal interval is [0,1]
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected error for confidence=2.5, got %q", resultText(res))
	}
	msg := resultText(res)
	if !strings.Contains(msg, "confidence") {
		t.Fatalf("expected error message to mention 'confidence', got %q", msg)
	}

	// And nothing should have been written to the cognitive store.
	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
	if len(recents) != 0 {
		t.Fatalf("expected no interactions persisted, got %d", len(recents))
	}
}

func TestRecordInteraction_RejectsNegativeResponseTime(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": -30.0,
		"confidence":            0.5,
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected error for response_time_seconds=-30, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "response_time_seconds") {
		t.Fatalf("expected error to mention 'response_time_seconds', got %q", resultText(res))
	}
}

func TestRecordInteraction_RejectsOutOfRangeHints(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.5,
		"hints_requested":       9999,
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected error for hints_requested=9999, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "hints_requested") {
		t.Fatalf("expected error to mention 'hints_requested', got %q", resultText(res))
	}
}

// Issue #51: each interaction must persist the slip/guess values the
// non-canonical error-type heuristic fed into the BKT update so the run
// can be replayed deterministically. SYNTAX_ERROR ramps slip up by 0.15;
// the row's bkt_slip column must reflect that.
func TestRecordInteraction_AuditRowCarriesHeuristicSlipGuess_SyntaxError(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               false,
		"response_time_seconds": 8.0,
		"confidence":            0.4,
		"error_type":            "SYNTAX_ERROR",
		"notes":                 "",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 1)
	if len(recents) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(recents))
	}
	got := recents[0]
	if got.BKTSlip == nil || got.BKTGuess == nil {
		t.Fatalf("expected bkt_slip and bkt_guess to be persisted, got slip=%v guess=%v", got.BKTSlip, got.BKTGuess)
	}
	// Default ConceptState has PSlip=0.1; SYNTAX_ERROR ramp is +0.15 → 0.25.
	if *got.BKTSlip < 0.249 || *got.BKTSlip > 0.251 {
		t.Errorf("bkt_slip = %f, want ~0.25 (base 0.1 + SYNTAX_ERROR ramp 0.15)", *got.BKTSlip)
	}
	// Default ConceptState has PGuess=0.2 — SYNTAX_ERROR doesn't ramp guess.
	if *got.BKTGuess < 0.199 || *got.BKTGuess > 0.201 {
		t.Errorf("bkt_guess = %f, want ~0.20 (unchanged by SYNTAX_ERROR)", *got.BKTGuess)
	}
}

// Issue #51: KNOWLEDGE_GAP ramps guess down by 0.10 — the audit row must
// carry the resulting value (0.20 - 0.10 = 0.10).
func TestRecordInteraction_AuditRowCarriesHeuristicSlipGuess_KnowledgeGap(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               false,
		"response_time_seconds": 12.0,
		"confidence":            0.3,
		"error_type":            "KNOWLEDGE_GAP",
		"notes":                 "",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 1)
	if len(recents) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(recents))
	}
	got := recents[0]
	if got.BKTSlip == nil || got.BKTGuess == nil {
		t.Fatalf("expected bkt_slip and bkt_guess to be persisted, got slip=%v guess=%v", got.BKTSlip, got.BKTGuess)
	}
	if *got.BKTSlip < 0.099 || *got.BKTSlip > 0.101 {
		t.Errorf("bkt_slip = %f, want ~0.10 (unchanged by KNOWLEDGE_GAP)", *got.BKTSlip)
	}
	if *got.BKTGuess < 0.099 || *got.BKTGuess > 0.101 {
		t.Errorf("bkt_guess = %f, want ~0.10 (base 0.20 - KNOWLEDGE_GAP ramp 0.10)", *got.BKTGuess)
	}
}

// Issue #51: even a successful interaction (no heuristic ramp applied)
// records the slip/guess values that were in effect, so replay does not
// have to special-case missing rows.
func TestRecordInteraction_AuditRowCarriesHeuristicSlipGuess_Success(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
	})
	if res.IsError {
		t.Fatalf("got %q", resultText(res))
	}

	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 1)
	if len(recents) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(recents))
	}
	got := recents[0]
	if got.BKTSlip == nil || got.BKTGuess == nil {
		t.Fatalf("expected bkt_slip and bkt_guess to be persisted on successes too, got slip=%v guess=%v", got.BKTSlip, got.BKTGuess)
	}
}

// Issue #88: activity_type must be constrained to a known enum so the LLM
// stops guessing values from prose like "RECALL_EXERCISE, NEW_CONCEPT, etc.".
// A free-form value like "GARBAGE" must be rejected with a clear error
// listing the accepted values.
func TestRecordInteraction_RejectsUnknownActivityType(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "GARBAGE",
		"success":               true,
		"response_time_seconds": 5.0,
		"confidence":            0.8,
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected error for activity_type=GARBAGE, got %q", resultText(res))
	}
	msg := resultText(res)
	if !strings.Contains(msg, "activity_type") {
		t.Fatalf("expected error to mention 'activity_type', got %q", msg)
	}
	if !strings.Contains(msg, "must be one of") {
		t.Fatalf("expected error to list accepted values via 'must be one of', got %q", msg)
	}
	// Spot-check a few canonical values are mentioned.
	if !strings.Contains(msg, "RECALL_EXERCISE") || !strings.Contains(msg, "NEW_CONCEPT") {
		t.Fatalf("expected error to list canonical activity types, got %q", msg)
	}

	// And nothing should have been persisted.
	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
	if len(recents) != 0 {
		t.Fatalf("expected no interactions persisted on bad activity_type, got %d", len(recents))
	}
}

// Issue #88: error_type is also a free-form string today and must be
// constrained to the BKT heuristic's vocabulary (SYNTAX_ERROR, LOGIC_ERROR,
// KNOWLEDGE_GAP). The empty value remains allowed (omitted).
func TestRecordInteraction_RejectsUnknownErrorType(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               false,
		"response_time_seconds": 10.0,
		"confidence":            0.4,
		"error_type":            "GARBAGE",
		"notes":                 "",
	})
	if !res.IsError {
		t.Fatalf("expected error for error_type=GARBAGE, got %q", resultText(res))
	}
	msg := resultText(res)
	if !strings.Contains(msg, "error_type") {
		t.Fatalf("expected error to mention 'error_type', got %q", msg)
	}
	if !strings.Contains(msg, "must be one of") {
		t.Fatalf("expected error to list accepted values via 'must be one of', got %q", msg)
	}
	if !strings.Contains(msg, "SYNTAX_ERROR") || !strings.Contains(msg, "KNOWLEDGE_GAP") {
		t.Fatalf("expected error to list canonical error types, got %q", msg)
	}

	// And nothing should have been persisted.
	recents, _ := store.GetRecentInteractionsByLearner(context.Background(), "L_owner", 5)
	if len(recents) != 0 {
		t.Fatalf("expected no interactions persisted on bad error_type, got %d", len(recents))
	}
}

// Issue #88: error_type is optional — empty string must be allowed (omitted).
func TestRecordInteraction_AllowsEmptyErrorType(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", map[string]any{
		"concept":               "a",
		"activity_type":         "RECALL_EXERCISE",
		"success":               false,
		"response_time_seconds": 10.0,
		"confidence":            0.4,
		"error_type":            "", // explicitly empty — must pass
		"notes":                 "",
	})
	if res.IsError {
		t.Fatalf("expected empty error_type to pass, got %q", resultText(res))
	}
}

func TestComputeCognitiveSignals(t *testing.T) {
	// Less than 3 interactions → no signals.
	fatigue, frust := computeCognitiveSignals([]*models.Interaction{
		{Success: true, Confidence: 0.8, ResponseTime: 10},
	})
	if fatigue != "none" || frust != "none" {
		t.Fatalf("expected none/none for tiny session, got %s/%s", fatigue, frust)
	}

	// Build a frustration scenario: consecutive failures + low confidence.
	bad := []*models.Interaction{
		{Success: false, Confidence: 0.1, ResponseTime: 20},
		{Success: false, Confidence: 0.2, ResponseTime: 22},
		{Success: false, Confidence: 0.1, ResponseTime: 25},
		{Success: true, Confidence: 0.5, ResponseTime: 10},
	}
	_, frust2 := computeCognitiveSignals(bad)
	if frust2 == "none" {
		t.Fatalf("expected non-none frustration, got %q", frust2)
	}

	// Build fatigue: poor recent vs solid earlier window.
	long := []*models.Interaction{
		// recent (newest first) — poor
		{Success: false, Confidence: 0.3, ResponseTime: 60},
		{Success: false, Confidence: 0.3, ResponseTime: 50},
		{Success: false, Confidence: 0.3, ResponseTime: 40},
		{Success: true, Confidence: 0.4, ResponseTime: 30},
		{Success: false, Confidence: 0.3, ResponseTime: 30},
		// earlier window — strong
		{Success: true, Confidence: 0.9, ResponseTime: 5},
		{Success: true, Confidence: 0.9, ResponseTime: 5},
		{Success: true, Confidence: 0.9, ResponseTime: 5},
		{Success: true, Confidence: 0.9, ResponseTime: 5},
		{Success: true, Confidence: 0.9, ResponseTime: 5},
	}
	fatigue3, _ := computeCognitiveSignals(long)
	if fatigue3 == "none" {
		t.Fatalf("expected fatigue signal, got %q", fatigue3)
	}
}

// Issue #53: applyInteraction's BKT → FSRS → IRT chain must be commutative
// in the sense that the IRT step reads the *prior* (pre-FSRS) Difficulty,
// not the value FSRS just overwrote. The non-commutative read is on
// cs.Difficulty: FSRS rewrites it, then IRT consumes it via
// FSRSDifficultyToIRT to compute the θ update. Reading post-FSRS difficulty
// silently shifts θ along the difficulty curve, biasing the learner's IRT
// estimate by the size of the difficulty step. Snapshot the read-only
// inputs at the top, run the three updates against the snapshot, write
// once at the end.
func TestApplyInteraction_IRTReadsPreFSRSDifficulty(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")

	// Seed a concept state in the Review FSRS state with a non-default
	// Difficulty and Reps. Difficulty=9.0 sits at the high end of the
	// FSRS [1,10] range; on an Again rating FSRS's nextDifficulty pulls
	// it sharply down (toward ~6.9), so post-FSRS difficulty differs
	// from pre-FSRS by ~2 difficulty points. With a failure response
	// IRT pushes θ negative, and the two starting difficulties produce
	// distinguishable θ values inside the [-4, 4] clamp window.
	now := time.Now().UTC()
	last := now.Add(-48 * time.Hour)
	seed := &models.ConceptState{
		LearnerID:     "L_owner",
		DomainID:      domain.ID,
		Concept:       "a",
		Stability:     5.0,
		Difficulty:    9.0,
		ElapsedDays:   2,
		ScheduledDays: 5,
		Reps:          5,
		Lapses:        0,
		CardState:     "review",
		LastReview:    &last,
		PMastery:      0.4,
		PLearn:        0.15,
		PForget:       0.05,
		PSlip:         0.1,
		PGuess:        0.2,
		// θ seed is non-zero and close to the pre-FSRS IRT difficulty so
		// the Newton iterations in IRTUpdateTheta land *inside* the
		// [-4, 4] clamp window, leaving room to distinguish the pre-FSRS
		// vs post-FSRS difficulty cases.
		Theta: 2.0,
	}
	if err := store.UpsertConceptState(context.Background(), seed); err != nil {
		t.Fatalf("seed concept state: %v", err)
	}

	// Compute the expected θ update by running IRT against the *pre-FSRS*
	// difficulty (9.0). success=false maps to FSRS rating Again.
	expectedItem := algorithms.IRTItem{
		Difficulty:     algorithms.FSRSDifficultyToIRT(9.0),
		Discrimination: 1.0,
	}
	expectedTheta := algorithms.IRTUpdateTheta(seed.Theta, []algorithms.IRTItem{expectedItem}, []bool{false})

	// And the *post-FSRS* difficulty θ — what today's buggy code computes.
	// We capture this so the assertion can also confirm the two values are
	// in fact distinguishable on this scenario (otherwise the test is
	// vacuous).
	postFSRSDiff := algorithms.ReviewCard(algorithms.FSRSCard{
		Stability:     5.0,
		Difficulty:    9.0,
		ElapsedDays:   2,
		ScheduledDays: 5,
		Reps:          5,
		Lapses:        0,
		State:         algorithms.Review,
		LastReview:    last,
	}, algorithms.Again, now).Difficulty
	buggyItem := algorithms.IRTItem{
		Difficulty:     algorithms.FSRSDifficultyToIRT(postFSRSDiff),
		Discrimination: 1.0,
	}
	buggyTheta := algorithms.IRTUpdateTheta(seed.Theta, []algorithms.IRTItem{buggyItem}, []bool{false})
	if math.Abs(expectedTheta-buggyTheta) < 1e-6 {
		t.Fatalf("test setup is vacuous: pre/post FSRS thetas are identical (%v)", expectedTheta)
	}

	cs, _, err := applyInteraction(context.Background(), deps, "L_owner", interactionInput{
		Concept:             "a",
		ActivityType:        "RECALL_EXERCISE",
		Success:             false, // → FSRS rating Again
		ResponseTimeSeconds: 10,
		Confidence:          0.4,
		DomainID:            domain.ID,
	}, now)
	if err != nil {
		t.Fatalf("applyInteraction: %v", err)
	}

	if math.Abs(cs.Theta-expectedTheta) > 1e-9 {
		t.Fatalf("IRT consumed post-FSRS difficulty: got θ=%v, want θ=%v (post-FSRS-buggy θ=%v)",
			cs.Theta, expectedTheta, buggyTheta)
	}

	// Sanity: FSRS *did* rewrite Difficulty and Reps, so the read-then-
	// update concern is real on this path.
	if cs.Difficulty == 9.0 {
		t.Fatalf("FSRS did not rewrite Difficulty as expected (still 9.0)")
	}
	if cs.Reps != 6 {
		t.Fatalf("FSRS did not increment Reps as expected: got %d, want 6", cs.Reps)
	}
}
