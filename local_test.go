package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/db"
	"tutor-mcp/memory"
)

func TestLocalProcessHelper(t *testing.T) {
	if os.Getenv("TUTOR_LOCAL_TEST_HELPER") != "1" {
		return
	}
	os.Args = []string{"tutor-mcp", "--local"}
	if dir := os.Getenv("TUTOR_LOCAL_TEST_DIR"); dir != "" {
		os.Args = append(os.Args, "--data-dir", dir)
	}
	main()
	os.Exit(0) // never emit the Go test runner's PASS on protocol stdout
}

type localTestClient struct {
	cmd     *exec.Cmd
	session *mcp.ClientSession
	stderr  *bytes.Buffer
}

func connectLocalClient(ctx context.Context, dir, home string) (*localTestClient, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLocalProcessHelper$")
	cmd.Env = []string{"TUTOR_LOCAL_TEST_HELPER=1", "TUTOR_LOCAL_TEST_DIR=" + dir, "HOME=" + home, "USERPROFILE=" + home, "SystemRoot=" + os.Getenv("SystemRoot")}
	// Deliberately invalid server variables must not affect local startup.
	cmd.Env = append(cmd.Env, "DB_DRIVER=postgres", "DATABASE_URL=invalid", "JWT_SECRET=invalid", "SMTP_ADDR=invalid", "BASE_URL=invalid", "DEPLOYMENT_PROFILE=production", "PROCESS_ROLE=api")
	stderr := new(bytes.Buffer)
	cmd.Stderr = stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "local-acceptance", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return nil, fmt.Errorf("connect: %w: %s", err, stderr.String())
	}
	return &localTestClient{cmd, session, stderr}, nil
}

func localCall(t *testing.T, ctx context.Context, client *localTestClient, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := client.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil || result.IsError {
		data, _ := json.Marshal(result)
		t.Fatalf("%s result=%s err=%v", name, data, err)
	}
	return result
}

func TestLocalStdioConcurrentPersistenceAndCrashRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "profile")
	clients := make([]*localTestClient, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() { clients[i], errs[i] = connectLocalClient(ctx, dir, t.TempDir()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		client := clients[i]
		t.Cleanup(func() { _ = client.session.Close() })
	}
	listed, err := clients[0].session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) == 0 {
		t.Fatalf("tools: %+v %v", listed, err)
	}
	for _, tool := range listed.Tools {
		data, _ := json.Marshal(tool.Meta["securitySchemes"])
		if string(data) != `[{"type":"noauth"}]` {
			t.Fatalf("%s auth=%s", tool.Name, data)
		}
	}
	args := map[string]any{"name": "Persistent algebra", "concepts": []string{"fractions"}, "prerequisites": map[string][]string{}, "idempotency_key": "create-domain"}
	first := localCall(t, ctx, clients[0], "init_domain", args)
	replayed := localCall(t, ctx, clients[1], "init_domain", args)
	firstFields := first.StructuredContent.(map[string]any)
	replayFields := replayed.StructuredContent.(map[string]any)
	if replayFields["idempotent_replay"] != true {
		t.Fatal("missing cross-process replay marker")
	}
	delete(replayFields, "idempotent_replay")
	one, _ := json.Marshal(firstFields)
	two, _ := json.Marshal(replayFields)
	if !bytes.Equal(one, two) {
		t.Fatalf("cross-process idempotency differs: %s / %s", one, two)
	}
	for i, client := range clients {
		wg.Go(func() {
			localCall(t, ctx, client, "update_learner_memory", map[string]any{"scope": "memory", "operation": "append", "content": fmt.Sprintf("private-local-memory-%d", i), "idempotency_key": fmt.Sprintf("memory-%d", i)})
		})
	}
	wg.Wait()
	localCall(t, ctx, clients[0], "get_memory_state", map[string]any{})
	localCall(t, ctx, clients[0], "get_next_activity", map[string]any{})
	localCall(t, ctx, clients[0], "record_interaction", map[string]any{"concept": "fractions", "activity_type": "DIAGNOSTIC_ASSESSMENT", "success": true, "response_time_seconds": 20, "confidence": 0.7, "notes": "acceptance", "idempotency_key": "progress"})
	// Lose one process without closing SQLite; its sibling continues normally.
	if err := clients[0].cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = clients[0].session.Close()
	localCall(t, ctx, clients[1], "check_mastery", map[string]any{"concept": "fractions"})
	if err := clients[1].session.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(clients[1].stderr.String(), "local MCP ready") {
		t.Fatal("logs missing from stderr")
	}
	restarted, err := connectLocalClient(ctx, dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	localCall(t, ctx, restarted, "check_mastery", map[string]any{"concept": "fractions"})
	if err := restarted.session.Close(); err != nil {
		t.Fatal(err)
	}
	store, _, err := openProfileStore(ctx, "local", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := store.EnsureLocalIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var learners, domains, interactions int
	if err := store.RawDB().QueryRow(`SELECT COUNT(*) FROM learners`).Scan(&learners); err != nil {
		t.Fatal(err)
	}
	if err := store.RawDB().QueryRow(`SELECT COUNT(*) FROM domains`).Scan(&domains); err != nil {
		t.Fatal(err)
	}
	if err := store.RawDB().QueryRow(`SELECT COUNT(*) FROM interactions`).Scan(&interactions); err != nil {
		t.Fatal(err)
	}
	if interactions != 1 {
		t.Fatalf("persisted interactions=%d", interactions)
	}
	if learners != 1 || domains != 1 {
		t.Fatalf("learners=%d domains=%d", learners, domains)
	}
	object, err := store.GetNarrative(ctx, memory.NarrativeKey{LearnerID: id, Scope: memory.ScopeMemory})
	if err != nil || !strings.Contains(object.Content, "private-local-memory-0") || !strings.Contains(object.Content, "private-local-memory-1") {
		t.Fatalf("memory=%+v err=%v", object, err)
	}
	var ciphertext string
	if err := store.RawDB().QueryRow(`SELECT ciphertext FROM narrative_objects WHERE learner_id = ?`, id).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, "private-local-memory") {
		t.Fatal("narrative is not encrypted")
	}
}

func TestLocalDefaultDirectoryAndMissingKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	home := t.TempDir()
	client, err := connectLocalClient(ctx, "", home)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.session.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".tutor-mcp", "local")
	if _, err = os.Stat(filepath.Join(dir, "runtime.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "keys.json"), filepath.Join(dir, "keys.backup")); err != nil {
		t.Fatal(err)
	}
	if store, _, err := openProfileStore(ctx, "local", dir, ""); err == nil {
		store.Close()
		t.Fatal("missing key was replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, "keys.json")); !os.IsNotExist(err) {
		t.Fatal("missing key recreated")
	}
	orphan := filepath.Join(t.TempDir(), "orphan")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "runtime.db-wal"), []byte("previous installation"), 0600); err != nil {
		t.Fatal(err)
	}
	if store, _, err := openProfileStore(ctx, "local", orphan, ""); err == nil {
		store.Close()
		t.Fatal("orphaned database sidecar received a new key")
	}
}

func TestInstallationProfileIsolation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hobby")
	store, _, err := openProfileStore(context.Background(), "hobby", dir, "https://tutor.example.org")
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if store, _, err := openProfileStore(context.Background(), "local", dir, ""); err == nil {
		store.Close()
		t.Fatal("opened hobby as local")
	}
	legacyPath := filepath.Join(t.TempDir(), "server", "runtime.db")
	database, err := db.OpenDB(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	legacy := db.NewStore(database)
	if _, err := legacy.CreateLearner(context.Background(), "person@example.org", "hash", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := legacy.EnsureInstallation(context.Background(), "local"); err == nil {
		t.Fatal("legacy server acquired local identity")
	}
}

func TestLocalCancellationAndNoHTTPListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Reserve the default HTTP port if available: stdio must still initialize
	// even when that port is already occupied by another application.
	listener, listenErr := net.Listen("tcp", "127.0.0.1:3000")
	if listenErr == nil {
		defer listener.Close()
	}
	dir := filepath.Join(t.TempDir(), "private")
	client, err := connectLocalClient(ctx, dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer client.session.Close()
	store, _, err := openProfileStore(ctx, "local", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tx, err := store.RawDB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	callCtx, stopCall := context.WithTimeout(ctx, 150*time.Millisecond)
	defer stopCall()
	_, err = client.session.CallTool(callCtx, &mcp.CallToolParams{Name: "init_domain", Arguments: map[string]any{
		"name": "Must be cancelled", "concepts": []string{"cancelled"}, "prerequisites": map[string][]string{},
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked call was not cancelled: %v", err)
	}
	// Leave time for the cancellation notification to reach the server before
	// releasing the competing SQLite writer. No user mutation may then commit.
	time.Sleep(100 * time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	localCall(t, ctx, client, "get_learner_context", map[string]any{})
	var count int
	if err := store.RawDB().QueryRow(`SELECT COUNT(*) FROM domains WHERE name = 'Must be cancelled'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cancelled mutation committed: count=%d err=%v", count, err)
	}
	// Exercise the same durable lease store used by both local schedulers.
	other, _, err := openProfileStore(ctx, "local", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	now := time.Now().UTC()
	if won, err := store.AcquireJobRunLease(ctx, "local-probe", "window", "first", now, time.Minute, 3); err != nil || !won {
		t.Fatalf("first scheduler lease: %v %v", won, err)
	}
	if won, err := other.AcquireJobRunLease(ctx, "local-probe", "window", "second", now, time.Minute, 3); err != nil || won {
		t.Fatalf("duplicate scheduler lease: %v %v", won, err)
	}
	afterCrash := now.Add(2 * time.Minute)
	if won, err := other.AcquireJobRunLease(ctx, "local-probe", "window", "second", afterCrash, time.Minute, 3); err != nil || !won {
		t.Fatalf("expired lease recovery: %v %v", won, err)
	}
	if done, err := store.CompleteJobRun(ctx, "local-probe", "window", "first", afterCrash); err != nil || done {
		t.Fatalf("stale scheduler completed another owner's work: %v %v", done, err)
	}
}
