package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileIndexTestSetup(t *testing.T) string {
	t.Helper()
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	ConfigureNarrativeStore(nil)
	if err := EnsureLearnerDirs("L1"); err != nil {
		t.Fatal(err)
	}
	base, err := learnerDir("L1")
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func TestFileSessionIndexLegacyRepairAndBoundedSelection(t *testing.T) {
	base := fileIndexTestSetup(t)
	limits, _ := configuredLimits()
	expanded := limits
	expanded.MaxFilesPerLearner = 4096
	if err := ConfigureLimits(expanded); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ConfigureLimits(limits) })
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2051; i++ {
		domain := "foreign"
		if i < 3 {
			domain = "wanted"
		}
		path := filepath.Join(base, "sessions", now.Add(time.Duration(i)*time.Second).Format(sessionFilenameLayout)+".md")
		if err := os.WriteFile(path, []byte("---\ndomain_id: "+domain+"\n---\n"+fmt.Sprint(i)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadContextForDomain("L1", "wanted", "", nil, nil); !errors.Is(err, ErrSessionIndexIncomplete) {
		t.Fatalf("legacy read=%v", err)
	}
	if _, err := os.Stat(filepath.Join(base, fileSessionIndexName)); !os.IsNotExist(err) {
		t.Fatal("request rebuilt missing index")
	}
	if n, err := RebuildFileSessionIndex(t.Context(), "L1"); err != nil || n != 2051 {
		t.Fatalf("rebuild=%d,%v", n, err)
	}
	got, err := LoadContextForDomain("L1", "wanted", "", nil, nil)
	if err != nil || len(got.RecentSessions) != 3 || got.RecentSessions[0].Body != "2" {
		t.Fatalf("selected=%+v err=%v", got, err)
	}
	// A changed foreign file must signal incomplete using stats alone, even
	// when its body is invalid YAML. Parsing it belongs to the repair path.
	foreign := filepath.Join(base, "sessions", now.Add(2048*time.Second).Format(sessionFilenameLayout)+".md")
	if err := os.WriteFile(foreign, []byte("---\ndomain_id: [\n---\ncorrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContextForDomain("L1", "wanted", "", nil, nil); !errors.Is(err, ErrSessionIndexIncomplete) {
		t.Fatalf("hot path parsed foreign body: %v", err)
	}
}

func TestFileSessionIndexCommittedWriteFailureRecoversWithoutReplay(t *testing.T) {
	base := fileIndexTestSetup(t)
	at := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	req := WriteRequest{LearnerID: "L1", Scope: ScopeSession, Timestamp: at, Operation: OpReplaceFile, Content: "---\ndomain_id: first\n---\nold"}
	if err := Write(req); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(base, fileSessionIndexName)
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(indexPath, 0o700); err != nil {
		t.Fatal(err)
	}
	req.Operation = OpAppend
	req.Content = "added-once"
	if err := Write(req); !IsCommittedWriteError(err) {
		t.Fatalf("write=%v, want committed", err)
	}
	raw, err := Read("L1", ScopeSession, at.Format(time.RFC3339))
	if err != nil || strings.Count(raw, "added-once") != 1 {
		t.Fatalf("committed body=%q err=%v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(base, fileSessionIndexDirty)); err != nil {
		t.Fatal("dirty marker lost", err)
	}
	if _, err := LoadContextForDomain("L1", "first", "", nil, nil); !errors.Is(err, ErrSessionIndexIncomplete) {
		t.Fatalf("dirty read=%v", err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildFileSessionIndex(t.Context(), "L1"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadContextForDomain("L1", "first", "", nil, nil)
	if err != nil || len(got.RecentSessions) != 1 || strings.Count(got.RecentSessions[0].Body, "added-once") != 1 {
		t.Fatalf("recovered=%+v err=%v", got, err)
	}
}

func TestFileSessionIndexExternalChangesAndCorruptManifest(t *testing.T) {
	base := fileIndexTestSetup(t)
	at := time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)
	if err := Write(WriteRequest{LearnerID: "L1", Scope: ScopeSession, Timestamp: at, Operation: OpReplaceFile, Content: "---\ndomain_id: first\n---\none"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "sessions", at.Format(sessionFilenameLayout)+".md")
	info, _ := os.Stat(path)
	// Same-size, same-mtime atomic replacement is detected in-process by
	// identity. An offline metadata-preserving edit uses the explicit rebuild.
	tmp := path + ".external"
	if err := os.WriteFile(tmp, []byte("---\ndomain_id: other\n---\ntwo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContextForDomain("L1", "other", "", nil, nil); !errors.Is(err, ErrSessionIndexIncomplete) {
		t.Fatalf("external replace=%v", err)
	}
	if _, err := RepairFileSessionIndexes(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := LoadContextForDomain("L1", "other", "", nil, nil)
	if err != nil || len(got.RecentSessions) != 1 {
		t.Fatalf("repair=%+v,%v", got, err)
	}
	indexPath := filepath.Join(base, fileSessionIndexName)
	var index fileSessionIndex
	data, _ := os.ReadFile(indexPath)
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	entry := index.Entries[at.Format(sessionFilenameLayout)]
	entry.DomainDigest = ""
	index.Entries[at.Format(sessionFilenameLayout)] = entry
	data, _ = json.Marshal(index)
	if err := os.WriteFile(indexPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateFileSessionIndex(base)
	if _, err := LoadContextForDomain("L1", "other", "", nil, nil); !errors.Is(err, ErrSessionIndexIncomplete) {
		t.Fatalf("corrupt projection=%v", err)
	}
	if _, err := RebuildFileSessionIndex(t.Context(), "L1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContext("L1", "", nil, nil); !errors.Is(err, ErrSessionIndexIncomplete) {
		t.Fatalf("remove=%v", err)
	}
	if _, err := RepairFileSessionIndexes(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = LoadContext("L1", "", nil, nil)
	if err != nil || len(got.RecentSessions) != 0 {
		t.Fatalf("removed=%+v,%v", got, err)
	}
}

func TestFileSessionIndexLongDomainCancellationAndLearnerIsolation(t *testing.T) {
	base := fileIndexTestSetup(t)
	domain := strings.Repeat("long", 16<<10)
	for i := 0; i < 70; i++ {
		key := time.Date(2026, 10, 15, 3, 0, i, 0, time.UTC).Format(sessionFilenameLayout)
		if err := os.WriteFile(filepath.Join(base, "sessions", key+".md"), []byte("---\ndomain_id: "+domain+"\n---\nbody"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildFileSessionIndex(t.Context(), "L1"); err != nil {
		t.Fatal(err)
	}
	invalidateFileSessionIndex(base)
	if got, err := recentFileSessionPayloads(t.Context(), "L1", domain, 3); err != nil || len(got) != 3 {
		t.Fatalf("long legacy projection=%d,%v", len(got), err)
	}
	lock := memoryWriteLock(base)
	lock.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, err := recentFileSessionPayloads(ctx, "L1", domain, 3)
	cancel()
	lock.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation=%v", err)
	}
	if err := EnsureLearnerDirs("healthy"); err != nil {
		t.Fatal(err)
	}
	healthy, _ := learnerDir("healthy")
	if err := os.WriteFile(filepath.Join(healthy, "sessions", "2026-10-15T04-00-00Z.md"), []byte("---\ndomain_id: ok\n---\nhealthy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "sessions", "2026-10-15T05-00-00Z.md"), []byte("---\ndomain_id: [\n---\nbad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if indexed, err := RepairFileSessionIndexes(t.Context()); !errors.Is(err, ErrSessionIndexIncomplete) || indexed != 1 {
		t.Fatalf("corrupt learner or uncommitted entries counted: indexed=%d err=%v", indexed, err)
	}
	if got, err := recentFileSessionPayloads(t.Context(), "healthy", "ok", 3); err != nil || len(got) != 1 {
		t.Fatalf("healthy learner starved=%+v,%v", got, err)
	}
}

func TestFileSessionIndexOrdersMixedTimestampNames(t *testing.T) {
	base := fileIndexTestSetup(t)
	keys := []string{"2026-10-15T13-59-59Z", "2026-10-15T13-59-58Z", "2026-10-15T13-59-57Z", "2026-10-15T13:00:00Z", "2026-10-15T14:59:59.5+01:00"}
	for _, key := range keys {
		if err := os.WriteFile(filepath.Join(base, "sessions", key+".md"), []byte("---\ndomain_id: course\n---\n"+key), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildFileSessionIndex(t.Context(), "L1"); err != nil {
		t.Fatal(err)
	}
	got, err := recentFileSessionPayloads(t.Context(), "L1", "course", 3)
	if err != nil || len(got) != 3 {
		t.Fatalf("mixed timestamp read=%+v,%v", got, err)
	}
	for i, want := range []string{keys[4], keys[0], keys[1]} {
		if got[i].Body != want {
			t.Fatalf("mixed key rank%d=%q want%q", i, got[i].Body, want)
		}
	}
}

func TestFileSessionIndexContentSyncFailureRetainsDirtyUntilRepair(t *testing.T) {
	base := fileIndexTestSetup(t)
	at := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	req := WriteRequest{LearnerID: "L1", Scope: ScopeSession, Timestamp: at, Operation: OpReplaceFile, Content: "---\ndomain_id: course\n---\nold"}
	if err := Write(req); err != nil {
		t.Fatal(err)
	}
	req.Operation = OpAppend
	req.Content = "new-once"
	syncFailure := errors.New("injected content directory sync failure")
	err := writeNarrativeContext(t.Context(), req, func(path, content string) error {
		return atomicWriteWithParentSync(path, content, func(directory string) error {
			if directory != filepath.Join(base, "sessions") {
				t.Errorf("wrong durability boundary: %s", directory)
			}
			// atomicWrite has already performed its real rename at this point.
			if raw, err := os.ReadFile(path); err != nil || !strings.Contains(string(raw), "new-once") {
				t.Errorf("content not visible before sync failure: %q,%v", raw, err)
			}
			return syncFailure
		})
	})
	if !IsCommittedWriteError(err) || !errors.Is(err, syncFailure) {
		t.Fatalf("content sync error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(base, fileSessionIndexDirty)); err != nil {
		t.Fatalf("content sync failure cleaned index: %v", err)
	}
	if _, err := LoadContextForDomain("L1", "course", "", nil, nil); !errors.Is(err, ErrSessionIndexIncomplete) {
		t.Fatalf("unsynced content looked complete: %v", err)
	}
	if _, err := RebuildFileSessionIndex(t.Context(), "L1"); err != nil {
		t.Fatal(err)
	}
	view, err := LoadContextForDomain("L1", "course", "", nil, nil)
	if err != nil || len(view.RecentSessions) != 1 || strings.Count(view.RecentSessions[0].Body, "new-once") != 1 {
		t.Fatalf("repaired content replayed/lost: %+v,%v", view, err)
	}
}

func TestFileSessionIndexFailedExplicitRebuildInvalidatesOldManifest(t *testing.T) {
	base := fileIndexTestSetup(t)
	at := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	req := WriteRequest{LearnerID: "L1", Scope: ScopeSession, Timestamp: at, Operation: OpReplaceFile, Content: "---\ndomain_id: course\n---\noriginal body"}
	if err := Write(req); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "sessions", at.Format(sessionFilenameLayout)+".md")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := "---\ndomain_id: [\n---\n"
	bad += strings.Repeat("x", int(info.Size())-len(bad))
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if indexed, err := RebuildFileSessionIndex(t.Context(), "L1"); err == nil || indexed != 0 {
		t.Fatalf("failed rebuild reported committed entries=%d err=%v", indexed, err)
	}
	if _, err := os.Stat(filepath.Join(base, fileSessionIndexDirty)); err != nil {
		t.Fatalf("failed force rebuild lost dirty marker: %v", err)
	}
	invalidateFileSessionIndex(base) // model a restart/cache eviction
	if _, err := LoadContextForDomain("L1", "absent-domain", "", nil, nil); !errors.Is(err, ErrSessionIndexIncomplete) {
		t.Fatalf("old manifest served apparently complete history: %v", err)
	}
}
