// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

// ─── Affect States ──────────────────────────────────────────────────────────

func TestUpsertAffectState_InsertThenMergeNonZero(t *testing.T) {
	store := setupTestDB(t)

	// Initial insert with several non-zero fields.
	a1 := &models.AffectState{
		LearnerID:           "L1",
		SessionID:           "S1",
		Energy:              3,
		SubjectConfidence:   2,
		Satisfaction:        4,
		PerceivedDifficulty: 1,
		NextSessionIntent:   2,
		AutonomyScore:       0.5,
	}
	if err := store.UpsertAffectState(context.Background(), a1); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Second upsert with some zero values: zero values must NOT overwrite
	// previously-stored non-zero fields (per the CASE WHEN > 0 logic).
	a2 := &models.AffectState{
		LearnerID:           "L1",
		SessionID:           "S1",
		Energy:              0,    // should keep 3
		SubjectConfidence:   4,    // should overwrite 2 -> 4
		Satisfaction:        0,    // should keep 4
		PerceivedDifficulty: 0,    // should keep 1
		NextSessionIntent:   0,    // should keep 2
		AutonomyScore:       0.75, // should overwrite 0.5 -> 0.75
	}
	if err := store.UpsertAffectState(context.Background(), a2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := store.GetAffectBySession(context.Background(), "L1", "S1")
	if err != nil {
		t.Fatalf("GetAffectBySession: %v", err)
	}
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"Energy", got.Energy, 3},
		{"SubjectConfidence", got.SubjectConfidence, 4},
		{"Satisfaction", got.Satisfaction, 4},
		{"PerceivedDifficulty", got.PerceivedDifficulty, 1},
		{"NextSessionIntent", got.NextSessionIntent, 2},
		{"AutonomyScore", got.AutonomyScore, 0.75},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestGetAffectBySession_NotFound(t *testing.T) {
	store := setupTestDB(t)
	if _, err := store.GetAffectBySession(context.Background(), "L1", "missing"); err == nil {
		t.Fatal("expected error for missing affect state")
	}
}

func TestGetRecentAffectStates_OrderingAndLimit(t *testing.T) {
	store := setupTestDB(t)

	// Insert affect states for 3 different sessions, then back-date them so we
	// can assert DESC ordering by created_at.
	sessions := []string{"S1", "S2", "S3"}
	for _, sid := range sessions {
		a := &models.AffectState{LearnerID: "L1", SessionID: sid, Energy: 3}
		if err := store.UpsertAffectState(context.Background(), a); err != nil {
			t.Fatalf("insert %s: %v", sid, err)
		}
	}
	now := time.Now().UTC()
	rewrite := func(sid string, ts time.Time) {
		t.Helper()
		if _, err := store.db.Exec(
			`UPDATE affect_states SET created_at = ? WHERE session_id = ?`, ts, sid,
		); err != nil {
			t.Fatalf("rewrite %s: %v", sid, err)
		}
	}
	rewrite("S1", now.Add(-3*time.Hour))
	rewrite("S2", now.Add(-2*time.Hour))
	rewrite("S3", now.Add(-1*time.Hour))

	got, err := store.GetRecentAffectStates(context.Background(), "L1", 10)
	if err != nil {
		t.Fatalf("GetRecentAffectStates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	if got[0].SessionID != "S3" || got[1].SessionID != "S2" || got[2].SessionID != "S1" {
		t.Errorf("expected DESC order [S3,S2,S1], got [%s,%s,%s]",
			got[0].SessionID, got[1].SessionID, got[2].SessionID)
	}

	// limit caps result count.
	got, err = store.GetRecentAffectStates(context.Background(), "L1", 2)
	if err != nil {
		t.Fatalf("limit query: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("limit=2: got %d rows", len(got))
	}

	// Other learner gets nothing.
	got, _ = store.GetRecentAffectStates(context.Background(), "L2", 10)
	if len(got) != 0 {
		t.Errorf("expected 0 rows for L2, got %d", len(got))
	}
}

func TestUpdateAffectAutonomyScore(t *testing.T) {
	store := setupTestDB(t)
	a := &models.AffectState{LearnerID: "L1", SessionID: "S1", Energy: 1, AutonomyScore: 0.1}
	if err := store.UpsertAffectState(context.Background(), a); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := store.UpdateAffectAutonomyScore(context.Background(), "L1", "S1", 0.42); err != nil {
		t.Fatalf("UpdateAffectAutonomyScore: %v", err)
	}
	got, err := store.GetAffectBySession(context.Background(), "L1", "S1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AutonomyScore != 0.42 {
		t.Errorf("AutonomyScore = %v want 0.42", got.AutonomyScore)
	}

	// No-op against missing row should not error (UPDATE 0 rows is fine).
	if err := store.UpdateAffectAutonomyScore(context.Background(), "L1", "missing", 0.99); err != nil {
		t.Errorf("unexpected error for missing session: %v", err)
	}
}

// ─── Calibration Records ────────────────────────────────────────────────────

func TestCalibrationLifecycle(t *testing.T) {
	store := setupTestDB(t)
	rec := &models.CalibrationRecord{
		PredictionID: "P1",
		LearnerID:    "L1",
		ConceptID:    "C1",
		Predicted:    0.6,
	}
	if err := store.CreateCalibrationPrediction(context.Background(), rec); err != nil {
		t.Fatalf("create prediction: %v", err)
	}

	got, err := store.GetCalibrationRecord(context.Background(), "P1", "L1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LearnerID != "L1" || got.ConceptID != "C1" || got.Predicted != 0.6 {
		t.Errorf("unexpected: %+v", got)
	}
	if got.Actual != nil {
		t.Errorf("expected Actual nil before completion, got %v", *got.Actual)
	}
	if got.Delta != nil {
		t.Errorf("expected Delta nil before completion, got %v", *got.Delta)
	}

	// Complete the prediction.
	if err := store.CompleteCalibrationRecord(context.Background(), "P1", "L1", 0.8, -0.2); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err = store.GetCalibrationRecord(context.Background(), "P1", "L1")
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if got.Actual == nil || *got.Actual != 0.8 {
		t.Errorf("Actual = %v want 0.8", got.Actual)
	}
	if got.Delta == nil || *got.Delta != -0.2 {
		t.Errorf("Delta = %v want -0.2", got.Delta)
	}

	// Completing missing prediction returns the "not found" error.
	if err := store.CompleteCalibrationRecord(context.Background(), "does-not-exist", "L1", 0, 0); err == nil {
		t.Fatal("expected error completing missing record")
	}

	// GetCalibrationRecord on missing prediction also errors.
	if _, err := store.GetCalibrationRecord(context.Background(), "does-not-exist", "L1"); err == nil {
		t.Fatal("expected error getting missing record")
	}
}

// TestGetCalibrationRecord_FiltersByLearnerID asserts the DB query refuses to
// return a calibration record when the supplied learner_id does not match the
// owner — defence-in-depth for issue #87.
func TestGetCalibrationRecord_FiltersByLearnerID(t *testing.T) {
	store := setupTestDB(t)
	rec := &models.CalibrationRecord{
		PredictionID: "P_owner",
		LearnerID:    "learner_A",
		ConceptID:    "C1",
		Predicted:    0.5,
	}
	if err := store.CreateCalibrationPrediction(context.Background(), rec); err != nil {
		t.Fatalf("create prediction: %v", err)
	}

	// Fetching with the wrong learner must error with "calibration record not found".
	if _, err := store.GetCalibrationRecord(context.Background(), "P_owner", "learner_B"); err == nil {
		t.Fatal("expected error when fetching another learner's calibration record")
	}

	// Sanity: rightful owner still resolves.
	got, err := store.GetCalibrationRecord(context.Background(), "P_owner", "learner_A")
	if err != nil {
		t.Fatalf("owner fetch failed: %v", err)
	}
	if got.LearnerID != "learner_A" {
		t.Errorf("LearnerID = %q want learner_A", got.LearnerID)
	}
}

// TestCompleteCalibrationRecord_FiltersByLearnerID asserts the UPDATE refuses
// to mutate a calibration row owned by another learner — defence-in-depth for
// issue #87.
func TestCompleteCalibrationRecord_FiltersByLearnerID(t *testing.T) {
	store := setupTestDB(t)
	rec := &models.CalibrationRecord{
		PredictionID: "P_owner",
		LearnerID:    "learner_A",
		ConceptID:    "C1",
		Predicted:    0.5,
	}
	if err := store.CreateCalibrationPrediction(context.Background(), rec); err != nil {
		t.Fatalf("create prediction: %v", err)
	}

	// Completing as the wrong learner must error "calibration record not found".
	if err := store.CompleteCalibrationRecord(context.Background(), "P_owner", "learner_B", 0.5, 0.0); err == nil {
		t.Fatal("expected error when completing another learner's calibration record")
	}

	// And the row must remain unmodified (Actual still nil).
	got, err := store.GetCalibrationRecord(context.Background(), "P_owner", "learner_A")
	if err != nil {
		t.Fatalf("owner fetch failed: %v", err)
	}
	if got.Actual != nil {
		t.Errorf("expected Actual nil after rejected foreign update, got %v", *got.Actual)
	}

	// Sanity: rightful owner still completes.
	if err := store.CompleteCalibrationRecord(context.Background(), "P_owner", "learner_A", 0.7, -0.2); err != nil {
		t.Fatalf("owner complete failed: %v", err)
	}
}

func TestGetCalibrationBiasAndHistory(t *testing.T) {
	store := setupTestDB(t)

	// Empty case: bias is 0, history is empty.
	bias, err := store.GetCalibrationBias(context.Background(), "L1", 5)
	if err != nil {
		t.Fatalf("bias empty: %v", err)
	}
	if bias != 0 {
		t.Errorf("expected 0 bias on empty, got %v", bias)
	}
	hist, err := store.GetCalibrationBiasHistory(context.Background(), "L1", 5)
	if err != nil {
		t.Fatalf("history empty: %v", err)
	}
	if len(hist) != 0 {
		t.Errorf("expected empty history, got %v", hist)
	}

	// Insert three completed predictions; mean delta should be average.
	deltas := []float64{0.1, 0.3, -0.2}
	for i, d := range deltas {
		rec := &models.CalibrationRecord{
			PredictionID: "P" + string(rune('1'+i)),
			LearnerID:    "L1",
			ConceptID:    "C1",
			Predicted:    0.5,
		}
		if err := store.CreateCalibrationPrediction(context.Background(), rec); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := store.CompleteCalibrationRecord(context.Background(), rec.PredictionID, "L1", 0.5-d, d); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}

	bias, err = store.GetCalibrationBias(context.Background(), "L1", 10)
	if err != nil {
		t.Fatalf("bias: %v", err)
	}
	const want = (0.1 + 0.3 - 0.2) / 3.0
	if bias < want-1e-9 || bias > want+1e-9 {
		t.Errorf("bias = %v want %v", bias, want)
	}

	hist, err = store.GetCalibrationBiasHistory(context.Background(), "L1", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Errorf("expected 3 history entries, got %d", len(hist))
	}

	// Limit caps the bias window.
	if _, err := store.GetCalibrationBias(context.Background(), "L1", 1); err != nil {
		t.Errorf("bias limit=1: %v", err)
	}
}

// ─── Transfer Records ───────────────────────────────────────────────────────

func TestTransferRecord_CreateAndGet(t *testing.T) {
	store := setupTestDB(t)

	cases := []struct {
		ctx   string
		score float64
		sess  string
	}{
		{"abstract", 0.4, "sess-1"},
		{"applied", 0.7, "sess-2"},
		{"unfamiliar_setting", 0.2, "sess-3"},
	}
	for _, c := range cases {
		r := &models.TransferRecord{
			LearnerID:   "L1",
			ConceptID:   "C1",
			ContextType: c.ctx,
			Score:       c.score,
			SessionID:   c.sess,
		}
		if err := store.CreateTransferRecord(context.Background(), r); err != nil {
			t.Fatalf("create %s: %v", c.ctx, err)
		}
	}

	got, err := store.GetTransferScores(context.Background(), "L1", "C1")
	if err != nil {
		t.Fatalf("GetTransferScores: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}
	// Records are ordered by created_at DESC; since all were inserted within
	// the same nanosecond tick, just sum scores instead of order-checking.
	var sum float64
	for _, r := range got {
		sum += r.Score
		if r.LearnerID != "L1" || r.ConceptID != "C1" {
			t.Errorf("unexpected ids: %+v", r)
		}
	}
	const want = 0.4 + 0.7 + 0.2
	if sum < want-1e-9 || sum > want+1e-9 {
		t.Errorf("score sum = %v want %v", sum, want)
	}

	// Different concept yields nothing.
	got, _ = store.GetTransferScores(context.Background(), "L1", "C-other")
	if len(got) != 0 {
		t.Errorf("expected 0 for other concept, got %d", len(got))
	}
}

// ─── Autonomy Queries ───────────────────────────────────────────────────────

func TestGetHintStatsForMastered(t *testing.T) {
	store := setupTestDB(t)

	// Two concepts: C-mastered (p_mastery=0.9) and C-novice (p_mastery=0.2).
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	now := time.Now().UTC()
	mustExec(
		`INSERT INTO concept_states (learner_id, concept, p_mastery, updated_at) VALUES ('L1','C-mastered',0.9,?)`, now,
	)
	mustExec(
		`INSERT INTO concept_states (learner_id, concept, p_mastery, updated_at) VALUES ('L1','C-novice',0.2,?)`, now,
	)
	// 2 hints requested over 3 mastered interactions.
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, hints_requested, created_at)
		 VALUES ('L1','C-mastered','RECALL_EXERCISE',1,2,?)`, now,
	)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, hints_requested, created_at)
		 VALUES ('L1','C-mastered','RECALL_EXERCISE',1,0,?)`, now,
	)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, hints_requested, created_at)
		 VALUES ('L1','C-mastered','RECALL_EXERCISE',1,0,?)`, now,
	)
	// Novice interactions must NOT be counted.
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, hints_requested, created_at)
		 VALUES ('L1','C-novice','RECALL_EXERCISE',1,5,?)`, now,
	)

	hints, total, err := store.GetHintStatsForMastered(context.Background(), "L1", 0.7)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if hints != 2 {
		t.Errorf("hints = %d want 2", hints)
	}
	if total != 3 {
		t.Errorf("total = %d want 3", total)
	}

	// Threshold above all mastery values returns zeros.
	hints, total, err = store.GetHintStatsForMastered(context.Background(), "L1", 0.99)
	if err != nil {
		t.Fatalf("stats high threshold: %v", err)
	}
	if hints != 0 || total != 0 {
		t.Errorf("expected (0,0), got (%d,%d)", hints, total)
	}
}

func TestCountProactiveReviews(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	// 2 proactive reviews + 1 normal recall + 1 NEW_CONCEPT (excluded) + 1 REST (excluded)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, is_proactive_review, success, created_at)
		 VALUES ('L1','C1','RECALL_EXERCISE',1,1,?)`, now,
	)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, is_proactive_review, success, created_at)
		 VALUES ('L1','C1','RECALL_EXERCISE',1,1,?)`, now,
	)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, is_proactive_review, success, created_at)
		 VALUES ('L1','C1','RECALL_EXERCISE',0,1,?)`, now,
	)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, is_proactive_review, success, created_at)
		 VALUES ('L1','C1','NEW_CONCEPT',0,1,?)`, now,
	)
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, is_proactive_review, success, created_at)
		 VALUES ('L1','C1','REST',0,1,?)`, now,
	)
	// Old row outside the since window.
	mustExec(
		`INSERT INTO interactions (learner_id, concept, activity_type, is_proactive_review, success, created_at)
		 VALUES ('L1','C1','RECALL_EXERCISE',1,1,?)`, now.Add(-48*time.Hour),
	)

	proactive, total, err := store.CountProactiveReviews(context.Background(), "L1", now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if proactive != 2 {
		t.Errorf("proactive = %d want 2", proactive)
	}
	if total != 3 {
		t.Errorf("total = %d want 3 (excluding NEW_CONCEPT and REST)", total)
	}
}
