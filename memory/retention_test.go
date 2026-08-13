// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunRetentionDryRunThenApply(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	if err := Write(WriteRequest{LearnerID: "L1", Scope: ScopeMemory, Operation: OpReplaceFile, Content: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := Write(WriteRequest{LearnerID: "L1", DomainID: "D1", Scope: ScopeConcept, ConceptSlug: "fresh", Operation: OpReplaceFile, Content: "fresh"}); err != nil {
		t.Fatal(err)
	}
	oldPath, _ := PathForRead("L1", ScopeMemory, "")
	freshPath, _ := PathForDomainConcept("L1", "D1", "fresh")
	now := time.Now().UTC()
	if err := os.Chtimes(oldPath, now.Add(-60*24*time.Hour), now.Add(-60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	dry, err := RunRetention(now.Add(-30*24*time.Hour), false)
	if err != nil || dry.Eligible != 1 || dry.Applied != 0 {
		t.Fatalf("dry retention = %+v err=%v", dry, err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("dry run removed old file: %v", err)
	}
	applied, err := RunRetention(now.Add(-30*24*time.Hour), true)
	if err != nil || applied.Eligible != 1 || applied.Applied != 1 {
		t.Fatalf("applied retention = %+v err=%v", applied, err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old memory remains: %v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh memory was removed: %v", err)
	}
}

func TestRunRetentionResumesAfterPartialFilesystemFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", root)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	for _, learnerID := range []string{"L1", "L2"} {
		if err := Write(WriteRequest{LearnerID: learnerID, Scope: ScopeMemory, Operation: OpReplaceFile, Content: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for _, learnerID := range []string{"L1", "L2"} {
		path, _ := PathForRead(learnerID, ScopeMemory, "")
		if err := os.Chtimes(path, now.Add(-60*24*time.Hour), now.Add(-60*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	originalRemove := removeRetentionFile
	removals := 0
	removeRetentionFile = func(path string) error {
		removals++
		if removals == 2 {
			return errors.New("injected crash boundary")
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { removeRetentionFile = originalRemove })
	partial, err := RunRetention(now.Add(-30*24*time.Hour), true)
	if err == nil || partial.Applied != 1 {
		t.Fatalf("partial retention=%+v err=%v", partial, err)
	}
	removeRetentionFile = originalRemove
	resumed, err := RunRetention(now.Add(-30*24*time.Hour), true)
	if err != nil || resumed.Eligible != 1 || resumed.Applied != 1 {
		t.Fatalf("resumed retention=%+v err=%v", resumed, err)
	}
	for _, learnerID := range []string{"L1", "L2"} {
		if _, err := os.Stat(filepath.Join(root, "learners", learnerID, "MEMORY.md")); !os.IsNotExist(err) {
			t.Fatalf("old narrative for %s remains: %v", learnerID, err)
		}
	}
}
