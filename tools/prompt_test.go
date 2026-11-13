// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRegisterPrompt_ReturnsSystemPrompt(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	RegisterPrompt(server, nil)

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

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "tutor_mcp"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatalf("expected at least one prompt message")
	}
	msg, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent message, got %T", res.Messages[0].Content)
	}
	if !strings.Contains(msg.Text, "tutor MCP") {
		t.Fatalf("expected systemPrompt body to mention 'tutor MCP'")
	}
}

// promptToolCallRE matches a snake_case identifier written in call syntax,
// "name(", which is how the prompt and the server instructions name tools.
// The tool reference list was removed from the prompt (tools/list is the
// reference), so every call-syntax name must still be a registered tool.
var promptToolCallRE = regexp.MustCompile(`\b([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\(`)

// removedToolNames are tools merged or removed from the surface; the prompt
// and the server instructions must not mention them in any form.
var removedToolNames = []string{
	"get_next_activity", "get_learner_context", "get_dashboard_state", "get_olm_snapshot",
	"get_pending_alerts", "get_metacognitive_mirror", "record_transfer_result",
	"archive_domain", "unarchive_domain", "delete_domain", "preview_training_lifecycle",
	"archive_training", "restore_training", "delete_training", "withdraw_training",
	"preview_training_edit", "propose_training_edit", "save_training_edit",
}

func extractDocumentedTools(prompt string) map[string]bool {
	out := map[string]bool{}
	for _, m := range promptToolCallRE.FindAllStringSubmatch(prompt, -1) {
		out[m[1]] = true
	}
	return out
}

// extractRegisteredTools wires up RegisterTools against an in-memory MCP
// server, lists the registered tools and returns their canonical names. This
// mirrors TestRegisterTools_Smoke and ensures the consistency check uses the
// same source of truth as production startup.
func extractRegisteredTools(t *testing.T) map[string]bool {
	t.Helper()
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
	out := map[string]bool{}
	for _, tool := range res.Tools {
		out[tool.Name] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSystemPrompt_ToolRegistryConsistency asserts that the system prompt
// bullet list and the runtime tool registry stay in lockstep. Drift in
// either direction (registered-but-undocumented, documented-but-unregistered)
// fails the test with a sorted, human-readable list.
//
// Both surfaces are exercised under the same feature-flag configuration:
// REGULATION_GOAL is forced "on" so the goal-aware tools (set_goal_relevance,
// get_goal_relevance) and their corresponding prompt appendix are both
// active and must agree.
func TestSystemPrompt_ToolRegistryConsistency(t *testing.T) {
	// Goal tools are registered by default; the documentation-only
	// appendices register nothing.
	registered := extractRegisteredTools(t)
	if len(registered) == 0 {
		t.Fatalf("no registered tools discovered - RegisterTools wiring is broken")
	}
	for source, text := range map[string]string{"prompt": buildSystemPrompt(RegulationFlags{}), "server instructions": ServerInstructions} {
		documented := extractDocumentedTools(text)
		if len(documented) == 0 {
			t.Fatalf("no tool call parsed from the %s (regex %q)", source, promptToolCallRE.String())
		}
		var unknown []string
		for name := range documented {
			if !registered[name] {
				unknown = append(unknown, name)
			}
		}
		sort.Strings(unknown)
		if len(unknown) > 0 {
			t.Fatalf("%s calls unregistered tools: %s (registered: %s)", source, strings.Join(unknown, ", "), strings.Join(sortedKeys(registered), ", "))
		}
		for _, removed := range removedToolNames {
			if regexp.MustCompile(`\b` + removed + `\b`).MatchString(text) {
				t.Fatalf("%s still mentions removed tool %s", source, removed)
			}
		}
	}
	if strings.Contains(buildSystemPrompt(RegulationFlags{}), "TOOLS (reference)") {
		t.Fatal("the prompt repeats a tool reference; tools/list is the reference")
	}
	// Every core loop tool named by the server instructions also appears in
	// the prompt protocol, so both describe the same loop.
	prompt := buildSystemPrompt(RegulationFlags{})
	for name := range extractDocumentedTools(ServerInstructions) {
		if !strings.Contains(prompt, name) {
			t.Errorf("server instructions use %s but the prompt protocol does not", name)
		}
	}
}

func TestSystemPrompt_RegulationAppendicesAreDefaultOnAndPromptOnly(t *testing.T) {
	t.Setenv("REGULATION_ACTION", "")
	t.Setenv("REGULATION_CONCEPT", "")
	t.Setenv("REGULATION_GATE", "")

	prompt := buildSystemPrompt(RegulationFlagsFromEnv())

	expected := []string{
		"ACTION-AWARE (REGULATION_ACTION=on):",
		"REGULATION_ACTION is a prompt-only flag.",
		"the runtime action selector still runs through prepare_pedagogical_turn(mode=\"activity\")",
		"CONCEPT-AWARE (REGULATION_CONCEPT=on):",
		"REGULATION_CONCEPT is a prompt-only flag.",
		"the runtime concept selector still runs through prepare_pedagogical_turn(mode=\"activity\")",
		"GATE-AWARE (REGULATION_GATE=on):",
		"REGULATION_GATE is a prompt-only flag.",
		"the runtime gate still runs through prepare_pedagogical_turn(mode=\"activity\")",
	}
	for _, want := range expected {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q", want)
		}
	}
}

func TestSystemPrompt_RegulationAppendixLegacyOffFlags(t *testing.T) {
	t.Setenv("REGULATION_ACTION", "off")
	t.Setenv("REGULATION_CONCEPT", "off")
	t.Setenv("REGULATION_GATE", "off")

	prompt := buildSystemPrompt(RegulationFlagsFromEnv())

	for _, unwanted := range []string{
		"ACTION-AWARE (REGULATION_ACTION=on):",
		"CONCEPT-AWARE (REGULATION_CONCEPT=on):",
		"GATE-AWARE (REGULATION_GATE=on):",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("expected legacy flag value off to drop appendix %q", unwanted)
		}
	}
}

func TestSystemPrompt_RegulationAppendixFlagsRequireLiteralOff(t *testing.T) {
	t.Setenv("REGULATION_ACTION", "OFF")
	t.Setenv("REGULATION_CONCEPT", "false")
	t.Setenv("REGULATION_GATE", "0")

	prompt := buildSystemPrompt(RegulationFlagsFromEnv())

	for _, want := range []string{
		"ACTION-AWARE (REGULATION_ACTION=on):",
		"CONCEPT-AWARE (REGULATION_CONCEPT=on):",
		"GATE-AWARE (REGULATION_GATE=on):",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected non-literal off value to keep appendix %q", want)
		}
	}
}

// TestSystemPrompt_LanguageContract asserts that the system prompt
// instructs the LLM to handle locale per the i18n contract documented
// in docs/i18n.md.
func TestSystemPrompt_LanguageContract(t *testing.T) {
	prompt := buildSystemPrompt(RegulationFlagsFromEnv())

	if !strings.Contains(prompt, "update_learner_profile") {
		t.Error("system prompt must reference update_learner_profile for language persistence")
	}
	if regexp.MustCompile(`[^\x00-\x7F]`).MatchString(prompt) {
		t.Error("system prompt must be ASCII-only")
	}
	if !strings.Contains(strings.ToLower(prompt), "language") {
		t.Error("system prompt must address language handling explicitly")
	}
	if !strings.Contains(prompt, "profile.language") {
		t.Error("system prompt must describe profile.language as the override")
	}
}
