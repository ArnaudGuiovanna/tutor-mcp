// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRegisterTools_Smoke wires every tool through RegisterTools and lists them
// over an MCP session. This exercises the full registration code path and
// guards against accidental signature drift between handlers and the registry.
func TestRegisterTools_Smoke(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	_, deps := setupToolsTest(t)
	deps.OAuthGranularScopes = true

	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	RegisterTools(server, deps)

	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := []string{
		"start_learning_session",
		"get_pending_alerts",
		"get_next_activity",
		"record_interaction",
		"prepare_assessment_attempt",
		"submit_assessment_attempt",
		"cancel_assessment_attempt",
		"check_mastery",
		"get_learner_context",
		"get_availability_model",
		"update_availability_model",
		"get_olm_snapshot",
		"get_pedagogical_snapshots",
		"get_decision_replay_summary",
		"init_domain",
		"add_concepts",
		"get_curriculum_snapshot",
		"publish_curriculum_revision",
		"validate_domain_graph",
		"update_learner_profile",
		"record_affect",
		"calibration_check",
		"record_calibration_result",
		"get_autonomy_metrics",
		"get_metacognitive_mirror",
		"feynman_challenge",
		"transfer_challenge",
		"record_transfer_result",
		"learning_negotiation",
		"set_domain_priority",
		"mark_domain_high_stakes",
		"update_learner_memory",
		"read_raw_session",
		"get_memory_state",
		"archive_domain",
		"unarchive_domain",
		"delete_domain",
		"get_misconceptions",
		"record_session_close",
		"list_implementation_intentions",
		"update_implementation_intention",
		"queue_webhook_message",
		"get_dashboard_state",
		"set_goal_relevance",
		"get_goal_relevance",
	}
	got := map[string]bool{}
	registered := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		registered[tool.Name] = tool
	}
	var missing []string
	for _, w := range want {
		if !got[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("missing registered tools: %s", strings.Join(missing, ","))
	}
	if len(res.Tools) != len(want) {
		var unexpected []string
		for name := range got {
			found := false
			for _, expected := range want {
				if name == expected {
					found = true
					break
				}
			}
			if !found {
				unexpected = append(unexpected, name)
			}
		}
		t.Fatalf("registered tool count = %d, want %d; unexpected: %s", len(res.Tools), len(want), strings.Join(unexpected, ","))
	}
	for name := range idempotentWriteTools {
		tool := registered[name]
		if tool == nil {
			t.Errorf("idempotency policy names an unregistered tool %q", name)
			continue
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Errorf("%s input schema = %T, want object", name, tool.InputSchema)
			continue
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s schema properties = %T, want object", name, schema["properties"])
			continue
		}
		if _, ok := properties[idempotencyMetaKey]; !ok {
			t.Errorf("retryable mutation %s does not expose %q in its argument schema", name, idempotencyMetaKey)
		}
	}
	for name, tool := range registered {
		if tool.Annotations == nil {
			t.Errorf("%s has no safety annotations", name)
			continue
		}
		if tool.Annotations.ReadOnlyHint != readOnlyTools[name] {
			t.Errorf("%s readOnlyHint=%v, policy=%v", name, tool.Annotations.ReadOnlyHint, readOnlyTools[name])
		}
		if tool.Annotations.DestructiveHint == nil || tool.Annotations.OpenWorldHint == nil {
			t.Errorf("%s has incomplete boolean safety hints", name)
		}
		// Retry protection is conditional on an optional idempotency_key. MCP's
		// hint is unconditional for repeated calls with the same arguments, so no
		// tool may advertise it until the key becomes required (or intrinsic
		// idempotence is proved independently).
		if tool.Annotations.IdempotentHint {
			t.Errorf("%s overclaims unconditional idempotence", name)
		}
		schemes, ok := tool.Meta["securitySchemes"]
		if !ok {
			t.Errorf("%s has no OAuth securitySchemes metadata", name)
			continue
		}
		encoded, err := json.Marshal(schemes)
		requiredScopes, known := requiredOAuthScopesForTool(name)
		if !known {
			t.Errorf("%s has no OAuth scope policy", name)
			continue
		}
		var advertised []struct {
			Type   string   `json:"type"`
			Scopes []string `json:"scopes"`
		}
		decodeErr := json.Unmarshal(encoded, &advertised)
		if err != nil || decodeErr != nil || len(advertised) != 1 || advertised[0].Type != "oauth2" ||
			!slices.Equal(advertised[0].Scopes, requiredScopes) {
			t.Errorf("%s securitySchemes=%s err=%v", name, encoded, err)
		}
	}
	if deleteTool := registered["delete_domain"]; deleteTool.Annotations.DestructiveHint == nil || !*deleteTool.Annotations.DestructiveHint {
		t.Error("delete_domain must be marked destructive")
	}
	if recordTool := registered["record_interaction"]; recordTool.Annotations.DestructiveHint == nil || *recordTool.Annotations.DestructiveHint {
		t.Error("record_interaction must be marked additive/non-destructive")
	}
	if webhookTool := registered["queue_webhook_message"]; webhookTool.Annotations.OpenWorldHint == nil || !*webhookTool.Annotations.OpenWorldHint {
		t.Error("queue_webhook_message must be marked open-world")
	}
	assertToolSafetyHints(t, registered["learning_negotiation"], false, true, false)
	assertToolSafetyHints(t, registered["get_metacognitive_mirror"], false, false, true)
	assertToolSafetyHints(t, registered["get_curriculum_snapshot"], false, false, false)
	assertToolSafetyHints(t, registered["get_memory_state"], false, false, false)
	// get_next_activity can enqueue the same proactive mirror webhook as the
	// explicit mirror tool, so its open-world hint must remain consistent.
	assertToolSafetyHints(t, registered["get_next_activity"], false, false, true)
}

func TestRegisterTools_LegacyRolloutAdvertisesOnlyBundle(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	_, deps := setupToolsTest(t)
	deps.OAuthGranularScopes = false

	server := mcp.NewServer(&mcp.Implementation{Name: "legacy-scope-metadata", Version: "0"}, nil)
	RegisterTools(server, deps)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "legacy-scope-client", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 45 {
		t.Fatalf("listed tools=%d, want 45", len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		encoded, err := json.Marshal(tool.Meta["securitySchemes"])
		if err != nil {
			t.Fatalf("%s encode securitySchemes: %v", tool.Name, err)
		}
		var schemes []struct {
			Type   string   `json:"type"`
			Scopes []string `json:"scopes"`
		}
		if err := json.Unmarshal(encoded, &schemes); err != nil || len(schemes) != 1 ||
			schemes[0].Type != "oauth2" || !slices.Equal(schemes[0].Scopes, []string{"learner"}) {
			t.Fatalf("%s phase-A securitySchemes=%s decodeErr=%v", tool.Name, encoded, err)
		}
	}
}

func assertToolSafetyHints(t *testing.T, tool *mcp.Tool, readOnly, destructive, openWorld bool) {
	t.Helper()
	if tool == nil || tool.Annotations == nil {
		t.Fatal("tool or annotations are nil")
	}
	if tool.Annotations.ReadOnlyHint != readOnly {
		t.Errorf("%s readOnlyHint=%v, want %v", tool.Name, tool.Annotations.ReadOnlyHint, readOnly)
	}
	if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != destructive {
		t.Errorf("%s destructiveHint=%v, want %v", tool.Name, tool.Annotations.DestructiveHint, destructive)
	}
	if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint != openWorld {
		t.Errorf("%s openWorldHint=%v, want %v", tool.Name, tool.Annotations.OpenWorldHint, openWorld)
	}
}
