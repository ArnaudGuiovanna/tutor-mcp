// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriteReadAndReplaceSection(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")

	if err := Write(WriteRequest{
		LearnerID:  "L1",
		Scope:      ScopeMemory,
		Operation:  OpReplaceSection,
		SectionKey: "Current state",
		Content:    "First version.",
	}); err != nil {
		t.Fatalf("Write replace section: %v", err)
	}
	if err := Write(WriteRequest{
		LearnerID:  "L1",
		Scope:      ScopeMemory,
		Operation:  OpReplaceSection,
		SectionKey: "Current state",
		Content:    "Second version.",
	}); err != nil {
		t.Fatalf("Write replace section again: %v", err)
	}
	got, err := Read("L1", ScopeMemory, "")
	if err != nil {
		t.Fatalf("Read memory: %v", err)
	}
	if strings.Count(got, "## Current state") != 1 {
		t.Fatalf("section duplicated:\n%s", got)
	}
	if !strings.Contains(got, "Second version.") || strings.Contains(got, "First version.") {
		t.Fatalf("section was not replaced:\n%s", got)
	}
}

func configureTestLimits(t *testing.T, limits Limits) {
	t.Helper()
	previous, _ := configuredLimits()
	if err := ConfigureLimits(limits); err != nil {
		t.Fatalf("configure limits: %v", err)
	}
	t.Cleanup(func() {
		if err := ConfigureLimits(previous); err != nil {
			t.Fatalf("restore limits: %v", err)
		}
	})
}

func TestMemoryIntLimitEnvBoundsAndOversizedValue(t *testing.T) {
	const name = "TUTOR_MCP_TEST_NATIVE_INT_LIMIT"
	for _, tc := range []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "minimum", value: "1", want: 1},
		{name: "maximum", value: "100000", want: 100000},
		{name: "above maximum", value: "100001", wantErr: true},
		{name: "above int32", value: "2147483648", wantErr: true},
		{name: "maximum int64", value: "9223372036854775807", wantErr: true},
		{name: "oversized native integer", value: "999999999999999999999999999999999999999999", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(name, tc.value)
			got, err := memoryIntLimitEnv(name, 2, 1, 100000)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("value %q accepted as %d", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("value=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestWriteEnforcesPayloadFileLearnerAndFileCountQuotas(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	configureTestLimits(t, Limits{
		MaxWriteBytes: 32, MaxFileBytes: 48, MaxLearnerBytes: 64,
		MaxFilesPerLearner: 2, MaxConcurrentWrites: 2,
	})

	if err := Write(WriteRequest{LearnerID: "payload", Scope: ScopeMemory, Operation: OpReplaceFile, Content: strings.Repeat("x", 33)}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("oversized payload error=%v", err)
	}
	if err := Write(WriteRequest{LearnerID: "file", Scope: ScopeMemoryPending, Operation: OpAppend, Content: strings.Repeat("a", 30)}); err != nil {
		t.Fatal(err)
	}
	if err := Write(WriteRequest{LearnerID: "file", Scope: ScopeMemoryPending, Operation: OpAppend, Content: strings.Repeat("b", 20)}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("oversized file error=%v", err)
	}
	if err := Write(WriteRequest{LearnerID: "learner", Scope: ScopeMemory, Operation: OpReplaceFile, Content: strings.Repeat("a", 32)}); err != nil {
		t.Fatal(err)
	}
	if err := Write(WriteRequest{LearnerID: "learner", Scope: ScopeMemoryPending, Operation: OpReplaceFile, Content: strings.Repeat("b", 32)}); err != nil {
		t.Fatal(err)
	}
	if err := Write(WriteRequest{LearnerID: "learner", Scope: ScopeSession, Timestamp: time.Now(), Operation: OpReplaceFile, Content: "third"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("file-count error=%v", err)
	}
	if err := Write(WriteRequest{LearnerID: "learner-bytes", Scope: ScopeMemory, Operation: OpReplaceFile, Content: strings.Repeat("a", 32)}); err != nil {
		t.Fatal(err)
	}
	if err := Write(WriteRequest{LearnerID: "learner-bytes", Scope: ScopeMemoryPending, Operation: OpReplaceFile, Content: strings.Repeat("b", 32)}); err != nil {
		t.Fatal(err)
	}
	if err := Write(WriteRequest{LearnerID: "learner-bytes", Scope: ScopeMemory, Operation: OpAppend, Content: "x"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("learner-byte error=%v", err)
	}
}

func TestReadRejectsExternallyOversizedNarrativeFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", root)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	configureTestLimits(t, Limits{
		MaxWriteBytes: 16, MaxFileBytes: 16, MaxLearnerBytes: 64,
		MaxFilesPerLearner: 4, MaxConcurrentWrites: 2,
	})
	path, err := PathForRead("L1", ScopeMemory, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 17)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read("L1", ScopeMemory, ""); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("oversized read error=%v", err)
	}
}

func TestWriteRejectsWhenConcurrentBudgetIsFull(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	configureTestLimits(t, Limits{
		MaxWriteBytes: 16, MaxFileBytes: 16, MaxLearnerBytes: 64,
		MaxFilesPerLearner: 4, MaxConcurrentWrites: 1,
	})
	_, sem := configuredLimits()
	sem <- struct{}{}
	err := Write(WriteRequest{LearnerID: "L1", Scope: ScopeMemory, Operation: OpReplaceFile, Content: "bounded"})
	<-sem
	if !errors.Is(err, ErrQuotaExceeded) || !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("concurrency error=%v", err)
	}
}

func TestWriteSessionUsesTimestampFilenameAndParsesYAML(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")

	ts := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)
	content := `---
timestamp: 2026-05-14T09:30:00Z
duration_minutes: 47
affect_start: focused
affect_end: satisfied
energy_level: high
concepts_touched: ["probabilites_conditionnelles", "bayes_theorem"]
session_type: deep_dive
novelty_flag: true
---

## Summary
The learner connected Bayes and conditional probabilities.`
	if err := Write(WriteRequest{
		LearnerID: "L1",
		Scope:     ScopeSession,
		Timestamp: ts,
		Operation: OpReplaceFile,
		Content:   content,
	}); err != nil {
		t.Fatalf("Write session: %v", err)
	}
	sessions, err := ListSessions("L1")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || !sessions[0].Equal(ts) {
		t.Fatalf("sessions = %v, want [%v]", sessions, ts)
	}
	raw, err := Read("L1", ScopeSession, ts.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("Read session: %v", err)
	}
	payload, err := ParseSessionPayload(ts, raw)
	if err != nil {
		t.Fatalf("ParseSessionPayload: %v", err)
	}
	if payload.Frontmatter["affect_start"] != "focused" {
		t.Fatalf("frontmatter not parsed: %#v", payload.Frontmatter)
	}
	if !truthy(payload.Frontmatter["novelty_flag"]) {
		t.Fatalf("novelty_flag not parsed as truthy: %#v", payload.Frontmatter["novelty_flag"])
	}
	if !strings.Contains(payload.Body, "Bayes") {
		t.Fatalf("body not parsed: %q", payload.Body)
	}
}

func TestConceptSlugMayContainSlash(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")

	if err := Write(WriteRequest{
		LearnerID:   "L1",
		Scope:       ScopeConcept,
		ConceptSlug: "encoding/json",
		Operation:   OpReplaceSection,
		SectionKey:  "Current state",
		Content:     "Needs more transfer practice.",
	}); err != nil {
		t.Fatalf("Write concept: %v", err)
	}
	got, err := Read("L1", ScopeConcept, "encoding/json")
	if err != nil {
		t.Fatalf("Read concept: %v", err)
	}
	if !strings.Contains(got, "Needs more transfer practice.") {
		t.Fatalf("unexpected concept notes: %q", got)
	}
	concepts, err := ListConcepts("L1")
	if err != nil {
		t.Fatalf("ListConcepts: %v", err)
	}
	if len(concepts) != 1 || concepts[0] != "encoding/json" {
		t.Fatalf("concepts = %v, want encoding/json", concepts)
	}
}

func TestLearnerIDCannotTraverseMemoryRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", root)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")

	if err := Write(WriteRequest{
		LearnerID: "..",
		Scope:     ScopeMemoryPending,
		Operation: OpAppend,
		Content:   "contained",
	}); err != nil {
		t.Fatalf("write traversal-shaped learner ID: %v", err)
	}
	path, err := PathForRead("..", ScopeMemoryPending, "")
	if err != nil {
		t.Fatalf("resolve path: %v", err)
	}
	wantPrefix := filepath.Join(root, "learners") + string(filepath.Separator)
	if !strings.HasPrefix(path, wantPrefix) {
		t.Fatalf("memory path escaped learner root: %q, want prefix %q", path, wantPrefix)
	}
}

func TestEnabledDefaultsOnAndCanBeDisabled(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "")
	if !Enabled() {
		t.Fatal("memory should be enabled by default")
	}
	for _, value := range []string{"0", "false", "off", "no"} {
		t.Setenv("TUTOR_MCP_MEMORY_ENABLED", value)
		if Enabled() {
			t.Fatalf("memory should be disabled for %q", value)
		}
	}
}

func TestConcurrentAppendDoesNotLoseNarrativeMemory(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")

	const writers = 64
	limits := defaultLimits
	limits.MaxConcurrentWrites = writers
	configureTestLimits(t, limits)
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- Write(WriteRequest{
				LearnerID: "L1",
				Scope:     ScopeMemoryPending,
				Operation: OpAppend,
				Content:   fmt.Sprintf("observation-%02d", i),
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

	got, err := Read("L1", ScopeMemoryPending, "")
	if err != nil {
		t.Fatalf("read appended memory: %v", err)
	}
	for i := range writers {
		needle := fmt.Sprintf("observation-%02d", i)
		if strings.Count(got, needle) != 1 {
			t.Fatalf("%q count = %d, want 1; memory:\n%s", needle, strings.Count(got, needle), got)
		}
	}
}
