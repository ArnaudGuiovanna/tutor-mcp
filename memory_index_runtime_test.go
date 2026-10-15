package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func seedLocalIndexProfile(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "profile")
	persistence, _, err := openProfileStore(t.Context(), "local", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	learner, err := persistence.EnsureLocalIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		key := memory.NarrativeKey{LearnerID: learner, Scope: memory.ScopeSession, Key: time.Date(2026, 10, 15, 0, 0, i, 0, time.UTC).Format("2006-01-02T15-04-05Z")}
		if _, _, err := persistence.CompareAndSwapNarrative(t.Context(), key, 0, "---\ndomain_id: course\n---\nrestored session", "", "", limits); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := persistence.RawDB().Exec(`UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	return dir, learner
}

func localIndexEnvelopeSnapshot(t *testing.T, dir string) string {
	t.Helper()
	database, err := db.OpenDB(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.Query(`SELECT ciphertext,key_id,aad_version,version,checksum,size_bytes,created_at,updated_at FROM narrative_objects ORDER BY object_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []any
	for rows.Next() {
		var cipher, keyID, checksum string
		var aad, version, size int64
		var created, updated any
		if err := rows.Scan(&cipher, &keyID, &aad, &version, &checksum, &size, &created, &updated); err != nil {
			t.Fatal(err)
		}
		values = append(values, []any{cipher, keyID, aad, version, checksum, size, created, updated})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMemoryIndexCommandExistingProfileAndResumableRepair(t *testing.T) {
	dir, _ := seedLocalIndexProfile(t)
	keysBefore, err := os.ReadFile(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	envelopeBefore := localIndexEnvelopeSnapshot(t, dir)
	options, err := parseCommand([]string{"memory-index", "--local", "--data-dir", dir, "--batch-size", "1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runMemoryIndexCommand(t.Context(), options, &output); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["complete"] != true || report["indexed"] != float64(3) || report["remaining"] != float64(0) {
		t.Fatalf("command report=%s", output.String())
	}
	keysAfter, _ := os.ReadFile(filepath.Join(dir, "keys.json"))
	if !bytes.Equal(keysBefore, keysAfter) || localIndexEnvelopeSnapshot(t, dir) != envelopeBefore {
		t.Fatal("index command changed keys or encrypted object identity/content")
	}
	output.Reset()
	if err := runMemoryIndexCommand(t.Context(), options, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"indexed":0`) {
		t.Fatalf("replay reprocessed complete rows: %s", output.String())
	}
}

func TestMemoryIndexCommandRefusesMissingInstallationWithoutCreatingIt(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "missing")
	options, err := parseCommand([]string{"memory-index", "--local", "--data-dir", missing}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := runMemoryIndexCommand(t.Context(), options, io.Discard); err == nil {
		t.Fatal("missing profile reported success")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("command created a profile: %v", err)
	}
	for _, name := range []string{"keys.json", "runtime.db"} {
		dir, _ := seedLocalIndexProfile(t)
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		options.DataDir = dir
		if err := runMemoryIndexCommand(t.Context(), options, io.Discard); err == nil {
			t.Fatalf("missing %s reported success", name)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("command regenerated %s", name)
		}
	}
}

func TestNarrativeIndexAutomaticRepairCancelsAndJoinsBeforeClose(t *testing.T) {
	dir, learner := seedLocalIndexProfile(t)
	persistence, _, err := openProfileStoreMode(t.Context(), "local", dir, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stop := startNarrativeIndexRepair(t.Context(), persistence, logger, true, 5*time.Millisecond)
	defer stop()
	if _, err := persistence.RawDB().Exec(`UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var pending int
		if err := persistence.RawDB().QueryRow(`SELECT COUNT(*) FROM narrative_objects WHERE session_indexed_version<>version AND scope='session'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background repair did not resume")
		}
		time.Sleep(5 * time.Millisecond)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- persistence.WithTenantTx(context.Background(), models.LegacyPrincipal(learner).TenantScope(), func(context.Context, storeport.Store) error { close(entered); <-release; return nil })
	}()
	<-entered
	time.Sleep(15 * time.Millisecond)
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("repair stop did not cancel a pool wait")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err := persistence.RawDB().Exec(`UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	var pending int
	if err := persistence.RawDB().QueryRow(`SELECT COUNT(*) FROM narrative_objects WHERE scope='session' AND session_indexed_version<>version`).Scan(&pending); err != nil || pending != 3 {
		t.Fatalf("repair still running after join: %d,%v", pending, err)
	}
}

func TestMemoryIndexCommandRepairsLegacyFilesystem(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", root)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	memory.ConfigureNarrativeStore(nil)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	if err := memory.EnsureLearnerDirs("legacy"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "learners", "legacy", "sessions", "2026-10-15T00-00-00Z.md")
	if err := os.WriteFile(path, []byte("---\ndomain_id: old\n---\nlegacy body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.LoadContextForDomain("legacy", "old", "", nil, nil); !errors.Is(err, memory.ErrSessionIndexIncomplete) {
		t.Fatalf("unindexed FS=%v", err)
	}
	options, err := parseCommand([]string{"memory-index", "--filesystem", "--memory-root", root}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := runMemoryIndexCommand(t.Context(), options, io.Discard); err != nil {
		t.Fatal(err)
	}
	view, err := memory.LoadContextForDomain("legacy", "old", "", nil, nil)
	if err != nil || len(view.RecentSessions) != 1 || view.RecentSessions[0].Body != "legacy body" {
		t.Fatalf("restored filesystem=%+v,%v", view, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("repair removed source", err)
	}
}

func TestMemoryIndexCommandReportsCorruptionAndRetriesUnresolvedOnly(t *testing.T) {
	dir, _ := seedLocalIndexProfile(t)
	database, err := db.OpenDB(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	var cipher string
	if err := database.QueryRow(`SELECT ciphertext FROM narrative_objects WHERE object_key='2026-10-15T00-00-00Z'`).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE narrative_objects SET ciphertext='broken' WHERE object_key='2026-10-15T00-00-00Z'`); err != nil {
		t.Fatal(err)
	}
	database.Close()
	options, err := parseCommand([]string{"memory-index", "--local", "--data-dir", dir, "--batch-size", "1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runMemoryIndexCommand(t.Context(), options, &output); !errors.Is(err, memory.ErrSessionIndexIncomplete) {
		t.Fatalf("corrupt command=%v", err)
	}
	if !strings.Contains(output.String(), `"complete":false`) || !strings.Contains(output.String(), `"indexed":2`) || !strings.Contains(output.String(), `"remaining":1`) {
		t.Fatalf("incomplete report=%s", output.String())
	}
	database, err = db.OpenDB(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE narrative_objects SET ciphertext=? WHERE object_key='2026-10-15T00-00-00Z'`, cipher); err != nil {
		t.Fatal(err)
	}
	database.Close()
	output.Reset()
	if err := runMemoryIndexCommand(t.Context(), options, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"complete":true`) || !strings.Contains(output.String(), `"indexed":1`) {
		t.Fatalf("resumed report=%s", output.String())
	}
}

func TestNarrativeIndexAutomaticFilesystemRepair(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", root)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	memory.ConfigureNarrativeStore(nil)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	if err := memory.EnsureLearnerDirs("legacy"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "learners", "legacy", "sessions", "2026-10-15T00-00-00Z.md")
	if err := os.WriteFile(path, []byte("---\ndomain_id: before\n---\nold"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startNarrativeIndexRepair(t.Context(), nil, nil, false, 5*time.Millisecond)
	defer stop()
	if view, err := memory.LoadContextForDomain("legacy", "before", "", nil, nil); err != nil || len(view.RecentSessions) != 1 {
		t.Fatalf("startup did not repair legacy FS=%+v,%v", view, err)
	}
	if err := os.WriteFile(path, []byte("---\ndomain_id: after\n---\nnew"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		view, err := memory.LoadContextForDomain("legacy", "after", "", nil, nil)
		if err == nil && len(view.RecentSessions) == 1 && view.RecentSessions[0].Body == "new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance did not repair external edit=%+v,%v", view, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMemoryIndexProcessHelper(t *testing.T) {
	if os.Getenv("TUTOR_MEMORY_INDEX_PROCESS_HELPER") != "1" {
		return
	}
	os.Args = []string{"tutor-mcp", "memory-index", "--local", "--data-dir", os.Getenv("TUTOR_MEMORY_INDEX_PROFILE"), "--batch-size", "1"}
	main()
	os.Exit(0)
}

func TestMemoryIndexCommandProcessExitsWithoutServerConfiguration(t *testing.T) {
	dir, _ := seedLocalIndexProfile(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMemoryIndexProcessHelper$")
	cmd.Env = []string{"TUTOR_MEMORY_INDEX_PROCESS_HELPER=1", "TUTOR_MEMORY_INDEX_PROFILE=" + dir, "HOME=" + t.TempDir(), "SystemRoot=" + os.Getenv("SystemRoot"), "DB_DRIVER=postgres", "DATABASE_URL=invalid", "JWT_SECRET=invalid", "SMTP_ADDR=invalid", "BASE_URL=invalid", "PROCESS_ROLE=api", "DEPLOYMENT_PROFILE=production"}
	var output, stderr bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("memory-index process: %v: %s", err, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || report["complete"] != true || report["indexed"] != float64(3) {
		t.Fatalf("command started transport or emitted non-JSON: %s err=%v", output.String(), err)
	}
}

func TestMemoryIndexCommandFilesystemHonorsConfiguredLimits(t *testing.T) {
	previous, err := memory.LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = memory.ConfigureLimits(previous) })
	root := t.TempDir()
	directory := filepath.Join(root, "learners", "legacy", "sessions")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"2026-10-15T00-00-00Z", "2026-10-15T00-00-01Z"} {
		if err := os.WriteFile(filepath.Join(directory, key+".md"), []byte("---\ndomain_id: course\n---\nbody"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TUTOR_MCP_MEMORY_MAX_FILES_PER_LEARNER", "1")
	options, err := parseCommand([]string{"memory-index", "--filesystem", "--memory-root", root}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runMemoryIndexCommand(t.Context(), options, &output); err == nil {
		t.Fatal("filesystem command ignored configured file limit")
	}
	if !strings.Contains(output.String(), `"complete":false`) || !strings.Contains(output.String(), `"indexed":0`) {
		t.Fatalf("quota failure reported success: %s", output.String())
	}
}
