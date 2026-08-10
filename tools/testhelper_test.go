// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

var assessmentFixtureCounter int64

var toolsTestDBTemplate struct {
	sync.Once
	data []byte
	err  error
}

// setupToolsTest opens a private copy of a fully migrated SQLite template and
// pre-created learners. Replaying every migration for each of the hundreds of
// tool subtests makes the race-enabled CI job spend minutes in SQLite's parser;
// copying the closed template preserves per-test isolation while leaving the
// migration matrix itself to the db package.
func setupToolsTest(t *testing.T) (*db.Store, *Deps) {
	t.Helper()
	if os.Getenv("TUTOR_MCP_MEMORY_ROOT") == "" {
		t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	}
	template, err := toolsTestDBTemplateBytes()
	if err != nil {
		t.Fatalf("build tools test database template: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "tools-test.db")
	if err := os.WriteFile(dbPath, template, 0o600); err != nil {
		t.Fatalf("copy tools test database template: %v", err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open tools test database: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	store := db.NewStore(raw)
	deps := &Deps{
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return store, deps
}

func toolsTestDBTemplateBytes() ([]byte, error) {
	toolsTestDBTemplate.Do(func() {
		dir, err := os.MkdirTemp("", "tutor-mcp-tools-test-template-")
		if err != nil {
			toolsTestDBTemplate.err = err
			return
		}
		defer os.RemoveAll(dir)

		path := filepath.Join(dir, "template.db")
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			toolsTestDBTemplate.err = err
			return
		}
		if err := db.Migrate(raw); err != nil {
			_ = raw.Close()
			toolsTestDBTemplate.err = err
			return
		}
		now := time.Now().UTC()
		for _, id := range []string{"L_owner", "L_attacker"} {
			if _, err := raw.Exec(
				`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, 'hash', 'test', ?)`,
				id, id+"@test.com", now,
			); err != nil {
				_ = raw.Close()
				toolsTestDBTemplate.err = err
				return
			}
		}
		if err := raw.Close(); err != nil {
			toolsTestDBTemplate.err = err
			return
		}
		toolsTestDBTemplate.data, toolsTestDBTemplate.err = os.ReadFile(path)
	})
	return toolsTestDBTemplate.data, toolsTestDBTemplate.err
}

func openTestLearningSession(t *testing.T, store *db.Store, learnerID, sessionID, domainID string) *models.LearningSession {
	t.Helper()
	session, err := store.OpenLearningSession(context.Background(), learnerID, domainID, sessionID, time.Now().UTC())
	if err != nil {
		t.Fatalf("open test learning session: %v", err)
	}
	return session
}

// seedEvaluatedAssessmentFixture creates a complete prepare -> submit ->
// evaluate envelope for read-model tests. Public/host evaluations remain
// untrusted. Trusted methods are installed only by this DB-side test fixture,
// mirroring the non-MCP server boundary that production intentionally leaves
// configurable.
func seedEvaluatedAssessmentFixture(t *testing.T, store *db.Store, learnerID, domainID, concept string, activity models.ActivityType, passed bool, at time.Time, trustedMethod models.EvaluationMethod) string {
	t.Helper()
	sequence := atomic.AddInt64(&assessmentFixtureCounter, 1)
	attemptID := fmt.Sprintf("assessment-fixture-%d", sequence)
	attempt := &models.AssessmentAttempt{
		ID: attemptID, LearnerID: learnerID, DomainID: domainID, ConceptID: concept,
		ActivityID: attemptID + "-activity", ActivityVersion: 1,
		ActivityType: string(activity), Observable: "observable response",
		TaskText: "frozen task", RubricJSON: `{"criteria":[{"id":"correct","weight":1}]}`,
		PassingScore: 0.6, CreatedAt: at.Add(-2 * time.Minute),
	}
	ctx := context.Background()
	if err := store.CreateAssessmentAttempt(ctx, attempt); err != nil {
		t.Fatalf("create assessment fixture: %v", err)
	}
	if err := store.SubmitAssessmentAttempt(ctx, learnerID, attemptID, "learner response", "", at.Add(-time.Minute)); err != nil {
		t.Fatalf("submit assessment fixture: %v", err)
	}
	score := 0.1
	if passed {
		score = 0.9
	}
	if err := store.CompleteAssessmentEvaluation(ctx, learnerID, attemptID, `{"correct":1}`, "test-host", models.EvaluationMethodHostLLM, "", score, passed, at); err != nil {
		t.Fatalf("evaluate assessment fixture: %v", err)
	}
	if trustedMethod != "" {
		if _, err := store.RawDB().Exec(`UPDATE assessment_attempts
			SET trusted_evaluation = 1, evaluation_method = ? WHERE id = ?`, string(trustedMethod), attemptID); err != nil {
			t.Fatalf("trust assessment fixture: %v", err)
		}
	}
	return attemptID
}

// callTool spins up an MCP server with the provided register function, injects
// the given learnerID into the receiving context, then calls the tool with the
// provided arguments. When learnerID is empty no auth context is injected.
func callTool(
	t *testing.T,
	deps *Deps,
	register func(*mcp.Server, *Deps),
	learnerID, name string,
	args any,
) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	register(server, deps)
	if learnerID != "" {
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				ctx = context.WithValue(ctx, auth.LearnerIDKey, learnerID)
				ctx = auth.WithOAuthScope(ctx, models.OAuthScopeLearner)
				return next(ctx, method, req)
			}
		})
	}

	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	argsJSON, _ := json.Marshal(args)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: json.RawMessage(argsJSON),
	})
	if err != nil {
		t.Fatalf("CallTool transport error for %q: %v", name, err)
	}
	return res
}

// resultText extracts the first text content from a CallToolResult.
func resultText(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// decodeResult parses the JSON returned in the first text-content block.
func decodeResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	out := map[string]any{}
	txt := resultText(res)
	if txt == "" {
		return out
	}
	_ = json.Unmarshal([]byte(txt), &out)
	return out
}
