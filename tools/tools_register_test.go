// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
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
}
