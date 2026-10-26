// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestTrainingMutationSchemaRequiresStableKey(t *testing.T) {
	_, deps := setupToolsTest(t)
	for _, name := range []string{"select_conversation_training", "save_training_edit", "propose_training_edit", "share_training", "submit_training", "decide_training", "publish_training", "archive_training", "restore_training", "delete_training", "withdraw_training", "set_training_access", "set_training_policy", "join_training"} {
		schema := advertisedInputSchema(t, registerTrainingTools, deps, name)
		var required []string
		for _, field := range schema["required"].([]any) {
			required = append(required, field.(string))
		}
		if !slices.Contains(required, "key") {
			t.Fatalf("%s advertises its mandatory key as optional", name)
		}
		if name == "select_conversation_training" && (!slices.Contains(required, "id") || !slices.Contains(required, "expected_selection")) {
			t.Fatal("selection inputs are optional in advertised schema")
		}
		key := schemaProperty(t, schema, "key")
		if key["minLength"] != float64(1) || key["maxLength"] != float64(128) {
			t.Fatalf("key bounds=%v", key)
		}
	}
}

func TestTrainingThreeCreationsRestartThenExplicitGoSelectionAndActivity(t *testing.T) {
	s, deps := setupToolsTest(t)
	p, err := s.GetPrincipalForLearner(t.Context(), "L_owner", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	const conversation = "three-training-restart"
	connect := func() (*mcp.ClientSession, func()) {
		t.Helper()
		server := mcp.NewServer(&mcp.Implementation{Name: "real-tutor-restart", Version: "test"}, nil)
		RegisterTools(server, deps)
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				ctx, err := auth.WithPrincipal(ctx, p)
				if err != nil {
					return nil, err
				}
				ctx = auth.WithOAuthScope(ctx, models.OAuthScopeLearner)
				return next(storeport.WithConversation(ctx, conversation), method, req)
			}
		})
		st, ct := mcp.NewInMemoryTransports()
		serverSession, err := server.Connect(t.Context(), st, nil)
		if err != nil {
			t.Fatal(err)
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "restart-client", Version: "test"}, nil)
		session, err := client.Connect(t.Context(), ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		return session, func() { _ = session.Close(); _ = serverSession.Close() }
	}
	call := func(session *mcp.ClientSession, name string, input any) *mcp.CallToolResult {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(raw)})
		if err != nil {
			t.Fatalf("%s transport: %v", name, err)
		}
		return result
	}
	session, closeFirst := connect()
	ids := []string{}
	for index, name := range []string{"Go", "Python", "Astronomie"} {
		result := call(session, "init_domain", map[string]any{"name": name, "concepts": []string{"variables", "fonctions"}, "prerequisites": map[string][]string{}, "idempotency_key": fmt.Sprintf("initial-%d", index)})
		if result.IsError {
			t.Fatalf("creation %s: %s", name, resultText(result))
		}
		created := decodeResult(t, result)
		ids = append(ids, created["domain_id"].(string))
		confirmed := created["conversation_context"].(map[string]any)
		if created["selection_confirmed"] != true || confirmed["selection_version"] != float64(index+1) || confirmed["domain_id"] != ids[index] {
			t.Fatalf("creation omitted confirmed selection: %v", created)
		}
	}
	// Reproduce the actual Claude request: correct expected_selection, but no
	// key. It must now fail at the advertised contract with a useful key error.
	raw, _ := json.Marshal(map[string]any{"id": "personal:" + ids[0], "conversation_id": conversation, "expected_selection": 3})
	missing, missingErr := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "select_conversation_training", Arguments: json.RawMessage(raw)})
	if missingErr == nil && (missing == nil || !missing.IsError) {
		t.Fatal("selection without key was accepted")
	}
	diagnostic := fmt.Sprint(missingErr) + resultText(missing)
	if !strings.Contains(diagnostic, "key") {
		t.Fatalf("missing key failure is opaque: %s", diagnostic)
	}
	var seq int
	var name, path string
	if err = s.RawDB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	closeFirst()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	rawDB, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := db.NewSQLiteStore(rawDB, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.Close() })
	deps.Store = reloaded
	resumed, closeResumed := connect()
	defer closeResumed()
	contextResult := call(resumed, "get_conversation_context", map[string]any{"conversation_id": conversation})
	current := decodeResult(t, contextResult)
	if current["selection_version"] != float64(3) || current["domain_id"] != ids[2] {
		t.Fatalf("restart changed context: %v", current)
	}
	selected := call(resumed, "select_conversation_training", map[string]any{"id": "personal:" + ids[0], "conversation_id": conversation, "expected_selection": 3, "key": "resume-existing-go"})
	if selected.IsError {
		t.Fatalf("explicit Go selection failed: %s", resultText(selected))
	}
	goContext := decodeResult(t, selected)
	if goContext["domain_id"] != ids[0] || goContext["selection_version"] != float64(4) {
		t.Fatalf("Go selection did not commit: %v", goContext)
	}
	activity := call(resumed, "get_next_activity", map[string]any{"domain_id": ids[0], "idempotency_key": "resumed-go-activity"})
	if activity.IsError {
		t.Fatalf("Go activity after restart: %s", resultText(activity))
	}
	if got := decodeResult(t, activity)["domain_id"]; got != ids[0] {
		t.Fatalf("activity routed to %v rather than Go", got)
	}
	var count int
	if err = reloaded.RawDB().QueryRow(`SELECT COUNT(*) FROM domains WHERE learner_id='L_owner' AND deleted_at IS NULL`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("resume recreated a training: %d %v", count, err)
	}
}
