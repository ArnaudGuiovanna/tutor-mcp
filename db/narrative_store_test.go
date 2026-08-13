// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
)

func narrativeTestLimits() memory.Limits {
	return memory.Limits{
		MaxWriteBytes: 1 << 20, MaxFileBytes: 1 << 20,
		MaxLearnerBytes: 4 << 20, MaxFilesPerLearner: 32,
		MaxConcurrentWrites: 8,
	}
}

func narrativeTestStore(t *testing.T) *Store {
	t.Helper()
	store := setupTestDB(t)
	store.SetIntegrationSecretKeyring(testSecretKeyring(t, "narrative:"+encodedSecretKey(21), "narrative"))
	return store
}

func TestNarrativeStoreEncryptsVersionsAndDetectsStaleCAS(t *testing.T) {
	store := narrativeTestStore(t)
	ctx := context.Background()
	key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeMemory}
	const first = "private narrative content"

	created, replayed, err := store.CompareAndSwapNarrative(ctx, key, 0, first, "create-memory", narrativeChecksum("create-memory"), narrativeTestLimits())
	if err != nil || replayed || created.Version != 1 || created.Checksum != narrativeChecksum(first) {
		t.Fatalf("create object=%+v replayed=%v err=%v", created, replayed, err)
	}
	var ciphertext, keyID string
	if err := store.root.QueryRow(rb(store,
		`SELECT ciphertext, key_id FROM narrative_objects
		 WHERE learner_id = ? AND scope = ? AND domain_id = '' AND object_key = ''`),
		"L1", string(memory.ScopeMemory),
	).Scan(&ciphertext, &keyID); err != nil {
		t.Fatal(err)
	}
	if ciphertext == first || strings.Contains(ciphertext, first) || keyID != "narrative" {
		t.Fatalf("narrative dump exposed plaintext or key metadata drifted: ciphertext=%q key=%q", ciphertext, keyID)
	}
	got, err := store.GetNarrative(ctx, key)
	if err != nil || got.Content != first || got.Version != 1 {
		t.Fatalf("get object=%+v err=%v", got, err)
	}

	updated, replayed, err := store.CompareAndSwapNarrative(ctx, key, 1, "second", "update-memory", narrativeChecksum("update-memory"), narrativeTestLimits())
	if err != nil || replayed || updated.Version != 2 {
		t.Fatalf("update object=%+v replayed=%v err=%v", updated, replayed, err)
	}
	if _, _, err := store.CompareAndSwapNarrative(ctx, key, 1, "stale", "stale-memory", narrativeChecksum("stale-memory"), narrativeTestLimits()); !errors.Is(err, memory.ErrNarrativeVersionConflict) {
		t.Fatalf("stale CAS error=%v", err)
	}
}

func TestNarrativeStoreMutationReplayIsIdempotentAndConflictSafe(t *testing.T) {
	store := narrativeTestStore(t)
	ctx := context.Background()
	key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeMemoryPending}
	limits := narrativeTestLimits()

	first, replayed, err := store.CompareAndSwapNarrative(ctx, key, 0, "one\n", "append-1", narrativeChecksum("append one"), limits)
	if err != nil || replayed {
		t.Fatalf("first mutation object=%+v replayed=%v err=%v", first, replayed, err)
	}
	replay, replayed, err := store.CompareAndSwapNarrative(ctx, key, 0, "one\none\n", "append-1", narrativeChecksum("append one"), limits)
	if err != nil || !replayed || replay.Version != first.Version {
		t.Fatalf("replay object=%+v replayed=%v err=%v", replay, replayed, err)
	}
	if _, _, err := store.CompareAndSwapNarrative(ctx, key, 1, "different\n", "append-1", narrativeChecksum("different mutation"), limits); !errors.Is(err, memory.ErrNarrativeMutationConflict) {
		t.Fatalf("mutation key reuse error=%v", err)
	}
	stats, err := store.NarrativeStats(ctx, "L1")
	if err != nil || stats.ObjectCount != 1 || stats.TotalBytes != int64(len("one\n")) {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestNarrativeStoreCanonicalEnrollmentKeyIsolatesSameLearner(t *testing.T) {
	store := narrativeTestStore(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, store)
	_, version, err := store.CreateFormationDraft(ctx, owner, "Narrative isolation", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddFormationModule(ctx, owner, version.ID, models.FormationModuleInput{
		StableKey: "module", Title: "Module",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddFormationConcept(ctx, owner, version.ID, models.FormationConceptInput{
		ModuleStableKey: "module", StableKey: "concept", Label: "Concept",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishFormationVersion(ctx, owner, version.ID); err != nil {
		t.Fatal(err)
	}
	cohort, err := store.CreateCohort(ctx, owner, version.ID, "Canonical cohort", 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	formal, err := store.EnrollMembership(ctx, owner, cohort.ID, owner.MembershipID, `{}`)
	if err != nil {
		t.Fatal(err)
	}

	recoveryKey := memory.NarrativeKey{
		TenantID: owner.TenantID, EnrollmentID: "legacy_recovery_enrollment_L1",
		LearnerID: "L1", Scope: memory.ScopeMemory,
	}
	formalKey := memory.NarrativeKey{
		TenantID: owner.TenantID, EnrollmentID: formal.ID,
		LearnerID: "L1", Scope: memory.ScopeMemory,
	}
	for _, write := range []struct {
		key     memory.NarrativeKey
		content string
	}{
		{key: recoveryKey, content: "recovery narrative"},
		{key: formalKey, content: "formal narrative"},
	} {
		object, replayed, err := store.CompareAndSwapNarrative(
			ctx, write.key, 0, write.content, "same-mutation-id",
			narrativeChecksum("same canonical mutation"), narrativeTestLimits(),
		)
		if err != nil || replayed || object.Content != write.content {
			t.Fatalf("write enrollment=%q object=%+v replayed=%v err=%v",
				write.key.EnrollmentID, object, replayed, err)
		}
	}
	for _, want := range []struct {
		key     memory.NarrativeKey
		content string
	}{
		{key: recoveryKey, content: "recovery narrative"},
		{key: formalKey, content: "formal narrative"},
	} {
		object, err := store.GetNarrative(ctx, want.key)
		if err != nil || object.Content != want.content || object.EnrollmentID != want.key.EnrollmentID {
			t.Fatalf("read enrollment=%q object=%+v err=%v", want.key.EnrollmentID, object, err)
		}
	}

	// The canonical enrollment is part of the authenticated envelope. Even a
	// database-level ciphertext transplant between two rows for one learner is
	// rejected before plaintext reaches the application boundary.
	var recoveryCiphertext, recoveryKeyID string
	if err := store.queryRow(ctx, `SELECT ciphertext, key_id FROM narrative_objects
		WHERE tenant_id = ? AND enrollment_id = ? AND scope = 'memory'`,
		recoveryKey.TenantID, recoveryKey.EnrollmentID).
		Scan(&recoveryCiphertext, &recoveryKeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.exec(ctx, `UPDATE narrative_objects SET ciphertext = ?, key_id = ?
		WHERE tenant_id = ? AND enrollment_id = ? AND scope = 'memory'`,
		recoveryCiphertext, recoveryKeyID, formalKey.TenantID, formalKey.EnrollmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetNarrative(ctx, formalKey); !errors.Is(err, memory.ErrNarrativeCorrupt) {
		t.Fatalf("cross-enrollment ciphertext transplant error=%v", err)
	}
}

func TestNarrativeStoreTwoNodesDetectConcurrentWriter(t *testing.T) {
	firstNode := narrativeTestStore(t)
	secondNode := NewStoreWithDialect(firstNode.root, firstNode.dialect)
	secondNode.SetIntegrationSecretKeyring(firstNode.secretKeyring)
	ctx := context.Background()
	key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeConcept, DomainID: "domain-1", Key: "concept-1"}
	limits := narrativeTestLimits()
	if _, _, err := firstNode.CompareAndSwapNarrative(ctx, key, 0, "base", "base", narrativeChecksum("base"), limits); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for index, node := range []*Store{firstNode, secondNode} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			mutationID := "writer-" + string(rune('a'+index))
			_, _, err := node.CompareAndSwapNarrative(ctx, key, 1, "writer-content", mutationID, narrativeChecksum(mutationID), limits)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	var succeeded, conflicted int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, memory.ErrNarrativeVersionConflict):
			conflicted++
		default:
			t.Fatalf("concurrent writer error=%v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent results succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	object, err := firstNode.GetNarrative(ctx, key)
	if err != nil || object.Version != 2 || object.Content != "writer-content" {
		t.Fatalf("final object=%+v err=%v", object, err)
	}
}

func TestNarrativeStoreRejectsMissingAndCorruptObjects(t *testing.T) {
	store := narrativeTestStore(t)
	ctx := context.Background()
	missing := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeArchive, Key: "2026-01"}
	if _, err := store.GetNarrative(ctx, missing); !errors.Is(err, memory.ErrNarrativeNotFound) {
		t.Fatalf("missing error=%v", err)
	}
	key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeArchive, Key: "2026-02"}
	if _, _, err := store.CompareAndSwapNarrative(ctx, key, 0, "archive", "archive", narrativeChecksum("archive"), narrativeTestLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Exec(rb(store,
		`UPDATE narrative_objects SET checksum = ?
		 WHERE learner_id = ? AND scope = ? AND domain_id = '' AND object_key = ?`),
		strings.Repeat("0", 64), "L1", string(memory.ScopeArchive), "2026-02",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetNarrative(ctx, key); !errors.Is(err, memory.ErrNarrativeCorrupt) {
		t.Fatalf("corrupt checksum error=%v", err)
	}
}

func TestNarrativeStoreEnforcesLearnerQuotaInsideCAS(t *testing.T) {
	store := narrativeTestStore(t)
	ctx := context.Background()
	limits := memory.Limits{
		MaxWriteBytes: 8, MaxFileBytes: 8, MaxLearnerBytes: 8,
		MaxFilesPerLearner: 1, MaxConcurrentWrites: 1,
	}
	first := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeMemory}
	if _, _, err := store.CompareAndSwapNarrative(ctx, first, 0, "12345678", "quota-1", narrativeChecksum("quota-1"), limits); err != nil {
		t.Fatal(err)
	}
	second := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeMemoryPending}
	if _, _, err := store.CompareAndSwapNarrative(ctx, second, 0, "x", "quota-2", narrativeChecksum("quota-2"), limits); !errors.Is(err, memory.ErrQuotaExceeded) {
		t.Fatalf("quota error=%v", err)
	}
}

func TestNarrativeStorePackageAPIReplaysAppendWithoutDuplication(t *testing.T) {
	store := narrativeTestStore(t)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	memory.ConfigureNarrativeStore(store)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })

	req := memory.WriteRequest{
		LearnerID: "L1", Scope: memory.ScopeMemoryPending,
		Operation: memory.OpAppend, Content: "one observation", MutationID: "tool-request-1",
	}
	if err := memory.Write(req); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := memory.Write(req); err != nil {
		t.Fatalf("idempotent append replay: %v", err)
	}
	got, err := memory.Read("L1", memory.ScopeMemoryPending, "")
	if err != nil || strings.Count(got, "one observation") != 1 {
		t.Fatalf("replayed content=%q err=%v", got, err)
	}
	conflict := req
	conflict.Content = "different observation"
	if err := memory.Write(conflict); !errors.Is(err, memory.ErrNarrativeMutationConflict) {
		t.Fatalf("mutation replay conflict error=%v", err)
	}
}

func TestNarrativeStoreRotationRetainsVersionAndSupportsRollingKeys(t *testing.T) {
	store := setupTestDB(t)
	oldRing := testSecretKeyring(t, "old:"+encodedSecretKey(22), "old")
	store.SetIntegrationSecretKeyring(oldRing)
	key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeMemory}
	if _, _, err := store.CompareAndSwapNarrative(
		context.Background(), key, 0, "rotating narrative", "rotation-create",
		narrativeChecksum("rotation-create"), narrativeTestLimits(),
	); err != nil {
		t.Fatal(err)
	}
	var beforeUpdated flexTime
	if err := store.root.QueryRow(rb(store,
		`SELECT updated_at FROM narrative_objects
		 WHERE learner_id = ? AND scope = ? AND domain_id = '' AND object_key = ''`),
		"L1", string(memory.ScopeMemory),
	).Scan(&beforeUpdated); err != nil {
		t.Fatal(err)
	}
	store.SetIntegrationSecretKeyring(testSecretKeyring(t,
		"old:"+encodedSecretKey(22)+",new:"+encodedSecretKey(23), "new",
	))
	rotated, err := store.RotateNarrativeSecrets(context.Background())
	if err != nil || rotated != 1 {
		t.Fatalf("rotated=%d err=%v", rotated, err)
	}
	var keyID string
	var version int64
	var afterUpdated flexTime
	if err := store.root.QueryRow(rb(store,
		`SELECT key_id, version, updated_at FROM narrative_objects
		 WHERE learner_id = ? AND scope = ? AND domain_id = '' AND object_key = ''`),
		"L1", string(memory.ScopeMemory),
	).Scan(&keyID, &version, &afterUpdated); err != nil {
		t.Fatal(err)
	}
	if keyID != "new" || version != 1 || !afterUpdated.Time.Equal(beforeUpdated.Time) {
		t.Fatalf("rotation metadata key=%q version=%d before=%v after=%v", keyID, version, beforeUpdated.Time, afterUpdated.Time)
	}
	store.SetIntegrationSecretKeyring(testSecretKeyring(t, "new:"+encodedSecretKey(23), "new"))
	object, err := store.GetNarrative(context.Background(), key)
	if err != nil || object.Content != "rotating narrative" {
		t.Fatalf("post-rotation object=%+v err=%v", object, err)
	}
}

func TestNarrativeStoreBackfillReconcilesByChecksumWithoutDeletingSource(t *testing.T) {
	store := narrativeTestStore(t)
	root := t.TempDir()
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", root)
	memory.ConfigureNarrativeStore(nil)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	sessionAt := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)
	for _, req := range []memory.WriteRequest{
		{LearnerID: "L1", Scope: memory.ScopeMemory, Operation: memory.OpReplaceFile, Content: "stable"},
		{LearnerID: "L1", DomainID: "domain/one", Scope: memory.ScopeConcept, ConceptSlug: "concept/one", Operation: memory.OpReplaceFile, Content: "concept"},
		{LearnerID: "L1", Scope: memory.ScopeSession, Timestamp: sessionAt, Operation: memory.OpReplaceFile, Content: "session"},
		{LearnerID: "L1", Scope: memory.ScopeArchive, Period: "2026-07", Operation: memory.OpReplaceFile, Content: "archive"},
	} {
		if err := memory.Write(req); err != nil {
			t.Fatalf("seed local narrative: %v", err)
		}
	}
	memoryPath, err := memory.PathForRead("L1", memory.ScopeMemory, "")
	if err != nil {
		t.Fatal(err)
	}

	report, err := memory.BackfillLocalNarratives(context.Background(), store, root)
	if err != nil || report.Scanned != 4 || report.Imported != 4 || report.Conflicts != 0 {
		t.Fatalf("first backfill report=%+v err=%v", report, err)
	}
	if _, err := store.GetNarrative(context.Background(), memory.NarrativeKey{
		LearnerID: "L1", DomainID: "domain/one", Scope: memory.ScopeConcept, Key: "concept/one",
	}); err != nil {
		t.Fatalf("domain concept was not backfilled: %v", err)
	}
	report, err = memory.BackfillLocalNarratives(context.Background(), store, root)
	if err != nil || report.Imported != 0 || report.Reconciled != 4 {
		t.Fatalf("reconciled backfill report=%+v err=%v", report, err)
	}

	// A divergent local edit is surfaced and never overwrites the shared copy.
	if err := memory.Write(memory.WriteRequest{
		LearnerID: "L1", Scope: memory.ScopeMemory,
		Operation: memory.OpReplaceFile, Content: "divergent local",
	}); err != nil {
		t.Fatal(err)
	}
	report, err = memory.BackfillLocalNarratives(context.Background(), store, root)
	if !errors.Is(err, memory.ErrNarrativeBackfillConflict) || report.Conflicts != 1 {
		t.Fatalf("divergent backfill report=%+v err=%v", report, err)
	}
	shared, err := store.GetNarrative(context.Background(), memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeMemory})
	if err != nil || shared.Content != "stable" {
		t.Fatalf("shared conflict winner changed: object=%+v err=%v", shared, err)
	}
	if _, err := os.Stat(memoryPath); err != nil {
		t.Fatalf("backfill deleted rollback source: %v", err)
	}
}

func TestNarrativeStoreTwoPackageInstancesReadSameEpisodicState(t *testing.T) {
	firstNode := narrativeTestStore(t)
	secondNode := NewStoreWithDialect(firstNode.root, firstNode.dialect)
	secondNode.SetIntegrationSecretKeyring(firstNode.secretKeyring)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	memory.ConfigureNarrativeStore(firstNode)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	sessionAt := time.Date(2026, 8, 10, 14, 0, 0, 0, time.UTC)
	for _, req := range []memory.WriteRequest{
		{LearnerID: "L1", Scope: memory.ScopeMemory, Operation: memory.OpReplaceFile, Content: "shared stable", MutationID: "node-one-stable"},
		{LearnerID: "L1", Scope: memory.ScopeMemoryPending, Operation: memory.OpAppend, Content: "- shared pending", MutationID: "node-one-pending"},
		{LearnerID: "L1", Scope: memory.ScopeSession, Timestamp: sessionAt, Operation: memory.OpReplaceFile, Content: "shared session", MutationID: "node-one-session"},
	} {
		if err := memory.Write(req); err != nil {
			t.Fatalf("first node write: %v", err)
		}
	}

	// A separate Store value models another process/pool connected to the same
	// database and carrying the same rolling keyring.
	memory.ConfigureNarrativeStore(secondNode)
	contextView, err := memory.LoadContext("L1", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if contextView.LearnerMemory != "shared stable" || !strings.Contains(contextView.PendingMemory, "shared pending") || len(contextView.RecentSessions) != 1 || contextView.RecentSessions[0].Body != "shared session" {
		t.Fatalf("second-node episodic context=%+v", contextView)
	}
}

func TestNarrativeStoreConcurrentAppendsPreserveEveryWriter(t *testing.T) {
	store := narrativeTestStore(t)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	memory.ConfigureNarrativeStore(store)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })

	const writers = 16
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for index := 0; index < writers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			entry := fmt.Sprintf("shared-writer-%02d", index)
			errs <- memory.Write(memory.WriteRequest{
				LearnerID: "L1", Scope: memory.ScopeMemoryPending,
				Operation: memory.OpAppend, Content: entry, MutationID: entry,
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	content, err := memory.Read("L1", memory.ScopeMemoryPending, "")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < writers; index++ {
		entry := fmt.Sprintf("shared-writer-%02d", index)
		if count := strings.Count(content, entry); count != 1 {
			t.Fatalf("%q count=%d, want 1 in %q", entry, count, content)
		}
	}
}
