// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mkGoalDomain(t *testing.T, store *db.Store, ownerID string, concepts []string) *models.Domain {
	t.Helper()
	d, err := store.CreateDomain(context.Background(), ownerID, "TestDomain", "ship a Go backend", models.KnowledgeSpace{
		Concepts:      concepts,
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	return d
}

// ─── set_goal_relevance ──────────────────────────────────────────────────────

// callToolRaw is like callTool but returns the SDK's transport-level error
// rather than fataling on it. Used to assert that flag-gated tools are
// invisible to the LLM (the SDK reports "unknown tool" as a Go error, not
// via CallToolResult.IsError).
func callToolRaw(t *testing.T, deps *Deps, register func(*mcp.Server, *Deps), learnerID, name string, args any) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	register(server, deps)
	if learnerID != "" {
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				ctx = context.WithValue(ctx, auth.LearnerIDKey, learnerID)
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
	return session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: json.RawMessage(argsJSON),
	})
}

func TestSetGoalRelevance_FlagOff_ToolNotRegistered(t *testing.T) {
	// Explicit opt-out: REGULATION_GOAL="off" → register noop → tool
	// absent from server. The SDK reports this as a transport-level
	// "unknown tool" error, not via CallToolResult.IsError.
	t.Setenv("REGULATION_GOAL", "off")
	_, deps := setupToolsTest(t)
	_, err := callToolRaw(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance",
		map[string]any{"relevance": map[string]float64{"A": 0.5}})
	if err == nil {
		t.Fatal("expected unknown-tool error when flag off")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected 'unknown tool' error, got %v", err)
	}
}

func TestSetGoalRelevance_FreshRoundTrip(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"Goroutines", "Channels", "Interfaces"})

	res := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": d.ID,
		"relevance": map[string]float64{"Goroutines": 0.9, "Channels": 0.4},
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if v, _ := out["concepts_updated"].(float64); int(v) != 2 {
		t.Errorf("concepts_updated: want 2, got %v", out["concepts_updated"])
	}
	if v, _ := out["covered_concepts_count"].(float64); int(v) != 2 {
		t.Errorf("covered: want 2, got %v", out["covered_concepts_count"])
	}
	if v, _ := out["all_concepts_count"].(float64); int(v) != 3 {
		t.Errorf("all: want 3, got %v", out["all_concepts_count"])
	}
	uncovered, _ := out["uncovered_concepts"].([]any)
	if len(uncovered) != 1 || uncovered[0].(string) != "Interfaces" {
		t.Errorf("uncovered: want [Interfaces], got %v", uncovered)
	}
}

func TestSetGoalRelevance_UnknownConceptRejected(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"A", "B"})

	res := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": d.ID,
		"relevance": map[string]float64{"A": 0.5, "Hallucinated": 0.7},
	})
	if !res.IsError {
		t.Fatalf("expected error on unknown concept, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "Hallucinated") {
		t.Errorf("error must cite the unknown concept by name, got %q", resultText(res))
	}
	// And nothing must have been persisted (atomic rejection).
	gr, _ := store.GetDomainGoalRelevance(context.Background(), d.ID)
	if gr != nil {
		t.Errorf("partial persistence on rejected call: %+v", gr)
	}
}

func TestSetGoalRelevance_ClampsOutOfRange(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"A", "B"})

	res := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": d.ID,
		"relevance": map[string]float64{"A": -0.3, "B": 1.7},
	})
	if res.IsError {
		t.Fatalf("clamping must succeed, got error %q", resultText(res))
	}
	out := decodeResult(t, res)
	if v, _ := out["concepts_clamped"].(float64); int(v) != 2 {
		t.Errorf("clamped: want 2, got %v", out["concepts_clamped"])
	}
	gr, _ := store.GetDomainGoalRelevance(context.Background(), d.ID)
	if gr.Relevance["A"] != 0 || gr.Relevance["B"] != 1 {
		t.Errorf("clamp values: got A=%v B=%v", gr.Relevance["A"], gr.Relevance["B"])
	}
}

func TestSetGoalRelevance_IncrementalMergeKeepsExisting(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"A", "B", "C"})

	first := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": d.ID,
		"relevance": map[string]float64{"A": 0.9, "B": 0.4},
	})
	if first.IsError {
		t.Fatalf("first set: %q", resultText(first))
	}
	second := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": d.ID,
		"relevance": map[string]float64{"C": 0.2},
	})
	if second.IsError {
		t.Fatalf("second set: %q", resultText(second))
	}
	gr, _ := store.GetDomainGoalRelevance(context.Background(), d.ID)
	if gr.Relevance["A"] != 0.9 || gr.Relevance["B"] != 0.4 || gr.Relevance["C"] != 0.2 {
		t.Errorf("incremental merge lost data: %+v", gr.Relevance)
	}
	out := decodeResult(t, second)
	uncovered, _ := out["uncovered_concepts"].([]any)
	if len(uncovered) != 0 {
		t.Errorf("after covering all 3, uncovered should be []; got %v", uncovered)
	}
}

func TestSetGoalRelevance_EmptyMapRejected(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"A"})

	res := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": d.ID,
		"relevance": map[string]float64{},
	})
	if !res.IsError {
		t.Fatalf("expected error on empty relevance map")
	}
}

func TestSetGoalRelevance_AddConceptsAfterDoesNotInvalidatePrior(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"A", "B"})

	if r := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": d.ID,
		"relevance": map[string]float64{"A": 0.9, "B": 0.4},
	}); r.IsError {
		t.Fatalf("set: %q", resultText(r))
	}

	// Simulate add_concepts: graph_version bumps to 2.
	d.Graph.Concepts = append(d.Graph.Concepts, "C")
	if err := store.UpdateDomainGraph(context.Background(), d.ID, d.Graph); err != nil {
		t.Fatalf("update graph: %v", err)
	}

	gr, _ := store.GetDomainGoalRelevance(context.Background(), d.ID)
	if gr.Relevance["A"] != 0.9 || gr.Relevance["B"] != 0.4 {
		t.Errorf("prior entries lost after add_concepts: %+v", gr.Relevance)
	}
	fresh, _ := store.GetDomainByID(context.Background(), d.ID)
	if !fresh.IsGoalRelevanceStale() {
		t.Error("expected stale after add_concepts")
	}
	uncov := fresh.UncoveredConcepts()
	if len(uncov) != 1 || uncov[0] != "C" {
		t.Errorf("uncovered: want [C], got %v", uncov)
	}
}

// ─── get_goal_relevance ──────────────────────────────────────────────────────

func TestGetGoalRelevance_FlagOff_NotRegistered(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "off")
	_, deps := setupToolsTest(t)
	_, err := callToolRaw(t, deps, registerGetGoalRelevance, "L_owner", "get_goal_relevance", map[string]any{})
	if err == nil {
		t.Fatal("expected unknown-tool error when flag off")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected 'unknown tool' error, got %v", err)
	}
}

func TestGetGoalRelevance_EmptyDomainReturnsAllUncovered(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"A", "B", "C"})

	res := callTool(t, deps, registerGetGoalRelevance, "L_owner", "get_goal_relevance", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if v, _ := out["covered_concepts_count"].(float64); int(v) != 0 {
		t.Errorf("covered: want 0, got %v", out["covered_concepts_count"])
	}
	uncov, _ := out["uncovered_concepts"].([]any)
	if len(uncov) != 3 {
		t.Errorf("uncovered: want 3, got %v", uncov)
	}
	if _, has := out["relevance"]; has {
		t.Errorf("expected no relevance field on empty domain (avoid misleading {}); got %v", out["relevance"])
	}
}

func TestGetGoalRelevance_StaleFlagAfterAddConcepts(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"A", "B"})

	// Set, then bump graph.
	if r := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": d.ID,
		"relevance": map[string]float64{"A": 0.9, "B": 0.4},
	}); r.IsError {
		t.Fatalf("set: %q", resultText(r))
	}
	d.Graph.Concepts = append(d.Graph.Concepts, "C")
	_ = store.UpdateDomainGraph(context.Background(), d.ID, d.Graph)

	res := callTool(t, deps, registerGetGoalRelevance, "L_owner", "get_goal_relevance", map[string]any{
		"domain_id": d.ID,
	})
	out := decodeResult(t, res)
	if stale, _ := out["stale"].(bool); !stale {
		t.Errorf("expected stale=true, got %v", out["stale"])
	}
	if v, _ := out["graph_version"].(float64); int(v) != 2 {
		t.Errorf("graph_version: want 2, got %v", out["graph_version"])
	}
	if v, _ := out["for_graph_version"].(float64); int(v) != 1 {
		t.Errorf("for_graph_version: want 1, got %v", out["for_graph_version"])
	}
}

func TestGetGoalRelevance_OwnershipEnforced(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	d := mkGoalDomain(t, store, "L_owner", []string{"A"})

	res := callTool(t, deps, registerGetGoalRelevance, "L_attacker", "get_goal_relevance", map[string]any{
		"domain_id": d.ID,
	})
	// resolveDomain should reject because L_attacker doesn't own d.
	if !res.IsError {
		t.Fatalf("expected ownership rejection, got %q", resultText(res))
	}
}
