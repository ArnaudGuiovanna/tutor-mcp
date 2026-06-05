// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

// ─── UpdateDomainPhase ─────────────────────────────────────────────────────

func TestUpdateDomainPhase_DiagnosticPersistsEntropy(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A", "B"})

	now := time.Now().UTC().Truncate(time.Second)
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseDiagnostic, 0.42, now); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Phase != models.PhaseDiagnostic {
		t.Errorf("phase: want DIAGNOSTIC, got %q", got.Phase)
	}
	if got.PhaseEntryEntropy != 0.42 {
		t.Errorf("entropy: want 0.42, got %v", got.PhaseEntryEntropy)
	}
	if !got.PhaseChangedAt.Equal(now) {
		t.Errorf("phase_changed_at: want %v, got %v", now, got.PhaseChangedAt)
	}
}

func TestUpdateDomainPhase_NonDiagnosticNullifiesEntropy(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A"})

	// First write DIAGNOSTIC with entropy populated.
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseDiagnostic, 0.55, time.Now().UTC()); err != nil {
		t.Fatalf("set diag: %v", err)
	}
	// Then transition to INSTRUCTION — entropy column must reset to NULL.
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseInstruction, 999, time.Now().UTC()); err != nil {
		t.Fatalf("set instruction: %v", err)
	}

	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Phase != models.PhaseInstruction {
		t.Errorf("phase: want INSTRUCTION, got %q", got.Phase)
	}
	// Defensive: PhaseEntryEntropy should be 0 (the sql.NullFloat64 default
	// when the column is NULL — we ignore the entropyArg in that branch).
	if got.PhaseEntryEntropy != 0 {
		t.Errorf("entropy: want 0 (NULL), got %v", got.PhaseEntryEntropy)
	}
}

func TestUpdateDomainPhase_MaintenanceAlsoNullifiesEntropy(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A"})

	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseDiagnostic, 0.7, time.Now().UTC()); err != nil {
		t.Fatalf("set diag: %v", err)
	}
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseMaintenance, 999, time.Now().UTC()); err != nil {
		t.Fatalf("set maint: %v", err)
	}
	got, _ := store.GetDomainByID(context.Background(), d.ID)
	if got.Phase != models.PhaseMaintenance {
		t.Errorf("phase: want MAINTENANCE, got %q", got.Phase)
	}
	if got.PhaseEntryEntropy != 0 {
		t.Errorf("entropy on MAINTENANCE: want 0 (NULL), got %v", got.PhaseEntryEntropy)
	}
}

func TestUpdateDomainPhase_NonexistentDomainNoError(t *testing.T) {
	// SQL UPDATE on a missing PK is a no-op, not an error. Pin the
	// behaviour: callers don't need to pre-check existence.
	store := setupTestDB(t)
	err := store.UpdateDomainPhase(context.Background(), "nonexistent", models.PhaseInstruction, 0, time.Now().UTC())
	if err != nil {
		t.Errorf("nonexistent domain: want no error, got %v", err)
	}
}

func TestUpdateDomainPhase_Idempotent(t *testing.T) {
	store := setupTestDB(t)
	d := mkDomain(t, store, []string{"A"})

	t1 := time.Now().UTC().Truncate(time.Second)
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseInstruction, 0, t1); err != nil {
		t.Fatalf("first: %v", err)
	}
	t2 := t1.Add(1 * time.Hour)
	if err := store.UpdateDomainPhase(context.Background(), d.ID, models.PhaseInstruction, 0, t2); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, _ := store.GetDomainByID(context.Background(), d.ID)
	if !got.PhaseChangedAt.Equal(t2) {
		t.Errorf("phase_changed_at: want updated to %v, got %v", t2, got.PhaseChangedAt)
	}
}

// ─── GetActiveMisconceptionsBatch ──────────────────────────────────────────

func TestGetActiveMisconceptionsBatch_EmptyConceptsReturnsEmptyMap(t *testing.T) {
	store := setupTestDB(t)
	got, err := store.GetActiveMisconceptionsBatch(context.Background(), "L1", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got == nil {
		t.Fatal("expected empty (non-nil) map, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected size 0, got %d", len(got))
	}
}

func TestGetActiveMisconceptionsBatch_OnlyActiveReported(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	// Active misconception on Goroutines (recent fail).
	insertInteraction(t, store, "Goroutines", false, "confusion", "x", now.Add(-1*time.Hour))

	// Resolved misconception on Interfaces (old fail + 3 successes).
	insertInteraction(t, store, "Interfaces", false, "type assertion", "old", now.Add(-5*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-4*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Interfaces", true, "", "", now.Add(-2*time.Hour))

	// Concept with no interactions at all.
	got, err := store.GetActiveMisconceptionsBatch(context.Background(), "L1", []string{"Goroutines", "Interfaces", "Channels"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got["Goroutines"] {
		t.Errorf("expected Goroutines=true, got %v", got["Goroutines"])
	}
	if got["Interfaces"] {
		t.Errorf("expected Interfaces=false (resolved), got true")
	}
	if got["Channels"] {
		t.Errorf("expected Channels=false (no interactions), got true")
	}
}

// ─── GetFirstActiveMisconception ───────────────────────────────────────────

func TestGetFirstActiveMisconception_NoneReturnsNil(t *testing.T) {
	store := setupTestDB(t)
	got, err := store.GetFirstActiveMisconception(context.Background(), "L1", "Goroutines")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestGetFirstActiveMisconception_ReturnsHighestCount(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	// Two distinct misconception types ; "confusion" appears twice ⇒ count=2,
	// "missing" appears once ⇒ count=1. The highest-count group is returned.
	insertInteraction(t, store, "Goroutines", false, "confusion", "d1", now.Add(-3*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "confusion", "d2", now.Add(-1*time.Hour))
	insertInteraction(t, store, "Goroutines", false, "missing", "d3", now.Add(-2*time.Hour))

	got, err := store.GetFirstActiveMisconception(context.Background(), "L1", "Goroutines")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil group")
	}
	if got.MisconceptionType != "confusion" {
		t.Errorf("type: want 'confusion' (highest count), got %q", got.MisconceptionType)
	}
}

// ─── GetRecentConceptsByDomain ─────────────────────────────────────────────

func TestGetRecentConceptsByDomain_EmptyDomainConcepts(t *testing.T) {
	store := setupTestDB(t)
	got, err := store.GetRecentConceptsByDomain(context.Background(), "L1", nil, 20)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestGetRecentConceptsByDomain_ZeroLimit(t *testing.T) {
	store := setupTestDB(t)
	got, err := store.GetRecentConceptsByDomain(context.Background(), "L1", []string{"A"}, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil on limit=0, got %v", got)
	}
}

func TestGetRecentConceptsByDomain_NoInteractions(t *testing.T) {
	store := setupTestDB(t)
	got, err := store.GetRecentConceptsByDomain(context.Background(), "L1", []string{"A", "B"}, 20)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestGetRecentConceptsByDomain_DedupAndOrder(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	// Insert chronologically: B (oldest), A, B, C (newest).
	insertInteraction(t, store, "B", true, "", "", now.Add(-4*time.Hour))
	insertInteraction(t, store, "A", true, "", "", now.Add(-3*time.Hour))
	insertInteraction(t, store, "B", true, "", "", now.Add(-2*time.Hour))
	insertInteraction(t, store, "C", true, "", "", now.Add(-1*time.Hour))

	got, err := store.GetRecentConceptsByDomain(context.Background(), "L1", []string{"A", "B", "C"}, 20)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Most-recent-first, dedup keeps first-seen (most recent) occurrence.
	want := []string{"C", "B", "A"}
	if len(got) != len(want) {
		t.Fatalf("len: want %d, got %d (%v)", len(want), len(got), got)
	}
	for i, c := range want {
		if got[i] != c {
			t.Errorf("pos %d: want %q, got %q", i, c, got[i])
		}
	}
}

func TestGetRecentConceptsByDomain_FiltersOutOfDomain(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	insertInteraction(t, store, "OtherDomain", true, "", "", now.Add(-1*time.Hour))
	insertInteraction(t, store, "A", true, "", "", now.Add(-2*time.Hour))

	got, err := store.GetRecentConceptsByDomain(context.Background(), "L1", []string{"A"}, 20)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0] != "A" {
		t.Errorf("expected [A], got %v", got)
	}
}

// ─── CountInteractionsSince ────────────────────────────────────────────────

func TestCountInteractionsSince_AllConcepts_NoFilter(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	insertInteraction(t, store, "A", true, "", "", now.Add(-3*time.Hour))
	insertInteraction(t, store, "B", true, "", "", now.Add(-1*time.Hour))
	insertInteraction(t, store, "C", true, "", "", now.Add(-30*time.Minute))

	// nil/empty domainConcepts → no Go-side filter, count everything since.
	since := now.Add(-2 * time.Hour)
	n, err := store.CountInteractionsSince(context.Background(), "L1", since, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 2 {
		t.Errorf("count want 2 (B+C since cutoff), got %d", n)
	}
}

func TestCountInteractionsSince_AcrossCutoff(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	// 1 before cutoff, 2 at-or-after — only the 2 should be counted.
	insertInteraction(t, store, "A", true, "", "", now.Add(-3*time.Hour))
	insertInteraction(t, store, "A", true, "", "", now.Add(-30*time.Minute))
	insertInteraction(t, store, "A", true, "", "", now.Add(-15*time.Minute))

	since := now.Add(-1 * time.Hour)
	n, err := store.CountInteractionsSince(context.Background(), "L1", since, []string{"A", "B"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 2 {
		t.Errorf("count: want 2, got %d", n)
	}
}

func TestCountInteractionsSince_FiltersOutOfDomain(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	insertInteraction(t, store, "InDomain", true, "", "", now.Add(-30*time.Minute))
	insertInteraction(t, store, "OutOfDomain", true, "", "", now.Add(-30*time.Minute))

	n, err := store.CountInteractionsSince(context.Background(), "L1", now.Add(-1*time.Hour), []string{"InDomain"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 1 {
		t.Errorf("count: want 1 (only InDomain), got %d", n)
	}
}

func TestCountInteractionsSince_FutureCutoffReturnsZero(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	insertInteraction(t, store, "A", true, "", "", now.Add(-1*time.Hour))

	n, err := store.CountInteractionsSince(context.Background(), "L1", now.Add(1*time.Hour), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("count: want 0, got %d", n)
	}
}

// ─── GetActionHistoryForConcept ────────────────────────────────────────────

func TestGetActionHistoryForConcept_NoInteractions(t *testing.T) {
	store := setupTestDB(t)
	h, err := store.GetActionHistoryForConcept(context.Background(), "L1", "A", 50)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.MasteryChallengeCount != 0 || h.FeynmanCount != 0 || h.TransferCount != 0 || h.InteractionsAboveBKT != 0 {
		t.Errorf("want zero counts, got %+v", h)
	}
}

func TestGetActionHistoryForConcept_CountsByActivityType(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	insertInteractionWithType(t, store, "A", true, string(models.ActivityMasteryChallenge), now.Add(-5*time.Hour))
	insertInteractionWithType(t, store, "A", true, string(models.ActivityMasteryChallenge), now.Add(-4*time.Hour))
	insertInteractionWithType(t, store, "A", true, string(models.ActivityFeynmanPrompt), now.Add(-3*time.Hour))
	insertInteractionWithType(t, store, "A", true, string(models.ActivityTransferProbe), now.Add(-2*time.Hour))
	// A different type that's neither MC, Feynman nor Transfer — should not bump any counter.
	insertInteractionWithType(t, store, "A", true, "PRACTICE", now.Add(-1*time.Hour))

	h, err := store.GetActionHistoryForConcept(context.Background(), "L1", "A", 50)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.MasteryChallengeCount != 2 {
		t.Errorf("MC: want 2, got %d", h.MasteryChallengeCount)
	}
	if h.FeynmanCount != 1 {
		t.Errorf("Feynman: want 1, got %d", h.FeynmanCount)
	}
	if h.TransferCount != 1 {
		t.Errorf("Transfer: want 1, got %d", h.TransferCount)
	}
}

func TestGetActionHistoryForConcept_StreakStopsOnFirstFailure(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	// Chronological: success, failure, success, success (most recent last).
	// DESC scan order is "success, success, failure, success" — streak = 2.
	insertInteractionWithType(t, store, "A", true, "PRACTICE", now.Add(-4*time.Hour))
	insertInteractionWithType(t, store, "A", false, "PRACTICE", now.Add(-3*time.Hour))
	insertInteractionWithType(t, store, "A", true, "PRACTICE", now.Add(-2*time.Hour))
	insertInteractionWithType(t, store, "A", true, "PRACTICE", now.Add(-1*time.Hour))

	h, err := store.GetActionHistoryForConcept(context.Background(), "L1", "A", 50)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.InteractionsAboveBKT != 2 {
		t.Errorf("streak: want 2, got %d", h.InteractionsAboveBKT)
	}
}

func TestGetActionHistoryForConcept_NegativeLimitDefaults(t *testing.T) {
	// recentLimit <= 0 should fall back to 50 (defensive default in the
	// implementation). We just verify the call is non-erroring and the
	// counts are computed.
	store := setupTestDB(t)
	now := time.Now().UTC()
	insertInteractionWithType(t, store, "A", true, "PRACTICE", now.Add(-1*time.Hour))

	h, err := store.GetActionHistoryForConcept(context.Background(), "L1", "A", -5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.InteractionsAboveBKT != 1 {
		t.Errorf("streak: want 1, got %d", h.InteractionsAboveBKT)
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

// insertInteractionWithType is a variant of insertInteraction that lets
// the caller pick the activity_type column value. Used by
// GetActionHistoryForConcept tests where the type drives counters.
func insertInteractionWithType(t *testing.T, store *Store, concept string, success bool, activityType string, createdAt time.Time) {
	t.Helper()
	successInt := 0
	if success {
		successInt = 1
	}
	_, err := store.root.Exec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, response_time, confidence, notes, misconception_type, misconception_detail, created_at)
		 VALUES ('L1', ?, ?, ?, 60, 0.5, '', ?, ?, ?)`,
		concept, activityType, successInt, nullString(""), nullString(""), createdAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}
