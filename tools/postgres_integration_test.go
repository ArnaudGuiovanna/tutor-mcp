// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"tutor-mcp/db"
)

// TestPostgresMCPProgressionPath proves that the real MCP handlers and the
// BKT/FSRS/IRT mutation chain obey the same contract on PostgreSQL as on the
// default SQLite test profile.
func TestPostgresMCPProgressionPath(t *testing.T) {
	baseDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	if baseDSN == "" {
		t.Skip("set TUTOR_TEST_PG_DSN to run the PostgreSQL tools gate")
	}
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	store := postgresToolsTestStore(t, baseDSN)
	learner, err := store.CreateLearner(context.Background(), "pg-tools@example.com", "hash", "learn", "")
	if err != nil {
		t.Fatal(err)
	}
	deps := &Deps{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if result := callTool(t, deps, registerInitDomain, learner.ID, "init_domain", map[string]any{
		"name":          "PostgreSQL progression",
		"concepts":      []string{"a", "b", "c"},
		"prerequisites": map[string][]string{},
	}); result.IsError {
		t.Fatalf("init_domain failed: %s", resultText(result))
	}

	for index := 0; index < 6; index++ {
		next := callTool(t, deps, registerGetNextActivity, learner.ID, "get_next_activity", map[string]any{})
		if next.IsError {
			t.Fatalf("get_next_activity %d failed: %s", index, resultText(next))
		}
		activity, _ := decodeResult(t, next)["activity"].(map[string]any)
		concept, _ := activity["concept"].(string)
		if concept == "" {
			t.Fatalf("missing concept in iteration %d: %v", index, activity)
		}
		recorded := callTool(t, deps, registerRecordInteraction, learner.ID, "record_interaction", map[string]any{
			"concept": concept, "activity_type": "RECALL_EXERCISE", "success": true,
			"response_time_seconds": 3.0, "confidence": 0.8, "notes": "",
		})
		if recorded.IsError {
			t.Fatalf("record_interaction %d failed: %s", index, resultText(recorded))
		}
	}
	interactions, err := store.GetRecentInteractionsByLearner(context.Background(), learner.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(interactions) != 6 {
		t.Fatalf("PostgreSQL interactions=%d, want 6", len(interactions))
	}
	moved := false
	for _, concept := range []string{"a", "b", "c"} {
		state, err := store.GetConceptState(context.Background(), learner.ID, concept)
		if err == nil && state != nil && state.PMastery > 0.1 {
			moved = true
		}
	}
	if !moved {
		t.Fatal("PostgreSQL MCP progression left every concept at bootstrap mastery")
	}
}

func postgresToolsTestStore(t *testing.T, baseDSN string) *db.Store {
	t.Helper()
	const schema = "p1_tools_mcp"
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(baseDSN, "?") {
		separator = "&"
	}
	raw, err := db.OpenPostgres(baseDSN+separator+"search_path="+schema, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MigratePostgres(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = raw.Close()
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		_ = admin.Close()
	})
	return db.NewStoreWithDialect(raw, db.DialectPostgres)
}
