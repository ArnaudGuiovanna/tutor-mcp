// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPedagogicalTurnReturnsCorrectableReason(t *testing.T) {
	_, deps, _, p, domain, selected := pedagogicalTurnFixture(t)
	in := map[string]any{"conversation_id": selected.ConversationID, "mode": "activity", "training_id": "personal:" + domain.ID, "training_name": "Go", "idempotency_key": "turn-reason"}
	res := callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", in)
	if want := "pedagogical turn unavailable: training_id and training_name are mutually exclusive"; !res.IsError || resultText(res) != want {
		t.Fatalf("got error=%v %q, want %q", res.IsError, resultText(res), want)
	}
	stale := map[string]any{"conversation_id": selected.ConversationID, "expected_selection": selected.SelectionVersion + 7}
	res = callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", stale)
	if !res.IsError || !strings.Contains(resultText(res), "selection or curriculum revision changed") {
		t.Fatalf("stale selection is not actionable: %q", resultText(res))
	}
}

func TestInitDomainReportsActiveDomainQuota(t *testing.T) {
	store, deps := setupToolsTest(t)
	store.SetMaxActiveDomainsPerLearner(1)
	makeOwnerDomain(t, store, "L_owner", "existing")
	res := callTool(t, deps, RegisterTools, "L_owner", "init_domain", map[string]any{
		"name": "second", "concepts": []string{"a"}, "prerequisites": map[string][]string{}, "idempotency_key": "quota-second",
	})
	if want := "active domain limit reached (1): archive or delete an unused domain before creating a new one"; !res.IsError || resultText(res) != want {
		t.Fatalf("got error=%v %q, want %q", res.IsError, resultText(res), want)
	}
}

func TestHandlerValidatedInputsAdvertiseEnums(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	_, deps := setupToolsTest(t)
	for tool, fields := range map[string][]string{
		"prepare_assessment_attempt":      {"activity_type"},
		"update_learner_memory":           {"scope", "operation"},
		"update_implementation_intention": {"status"},
		"list_implementation_intentions":  {"status"},
		"learning_negotiation":            {"activity_type"},
		"publish_curriculum_revision":     {"operation"},
	} {
		schema := advertisedInputSchema(t, RegisterTools, deps, tool)
		for _, field := range fields {
			if values, _ := schemaProperty(t, schema, field)["enum"].([]any); len(values) == 0 {
				t.Errorf("%s.%s advertises no enum", tool, field)
			}
		}
	}
	// Optional inputs keep accepting the empty string their handler accepts.
	operations, _ := schemaProperty(t, advertisedInputSchema(t, RegisterTools, deps, "update_learner_memory"), "operation")["enum"].([]any)
	if !slices.Contains(operations, any("")) || !slices.Contains(operations, any("append")) {
		t.Fatalf("operation enum = %v", operations)
	}
	// Nested paths reach map values and array items.
	details := schemaProperty(t, advertisedInputSchema(t, RegisterTools, deps, "init_domain"), "details")
	level, _ := details["additionalProperties"].(map[string]any)["properties"].(map[string]any)["level"].(map[string]any)
	if values, _ := level["enum"].([]any); !slices.Contains(values, any("foundation")) {
		t.Fatalf("init_domain details level enum = %v", level)
	}
}

// The pedagogical turn's schema and handler accept exactly the same values:
// the empty string (default) and the canonical names, never aliases.
func TestPedagogicalTurnModeAndIntentSchemaMatchHandler(t *testing.T) {
	_, deps := setupToolsTest(t)
	schema := advertisedInputSchema(t, RegisterTools, deps, "prepare_pedagogical_turn")
	for field, want := range map[string][]any{"mode": {"", "context", "activity"}, "intent": {"", "auto", "review"}} {
		got, _ := schemaProperty(t, schema, field)["enum"].([]any)
		if !slices.Equal(got, want) {
			t.Fatalf("%s enum = %v, want %v", field, got, want)
		}
	}
	for _, intent := range []string{"", "auto", "review"} {
		if _, err := normalizeActivityIntent(intent); err != nil {
			t.Fatalf("advertised intent %q refused: %v", intent, err)
		}
	}
	for _, alias := range []string{"revise", "practice", "Review", " auto"} {
		if _, err := normalizeActivityIntent(alias); err == nil {
			t.Fatalf("handler accepts %q, which the schema refuses", alias)
		}
	}
	for _, mode := range []string{"", "context", "activity"} {
		if _, err := normalizePedagogicalTurnMode(mode); err != nil {
			t.Fatalf("advertised mode %q refused: %v", mode, err)
		}
	}
}

func TestUpdateImplementationIntentionEnumRejectsInventedStatus(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, RegisterTools, "L_owner", "update_implementation_intention", map[string]any{"intention_id": 1, "status": "done", "idempotency_key": "enum-intention"})
	if !res.IsError || !strings.Contains(resultText(res), "honored") {
		t.Fatalf("invented status was not rejected with the vocabulary: %q", resultText(res))
	}
}

func TestDomainSetupInstructionIsEnglish(t *testing.T) {
	if got := domainSetupInstruction(&Deps{}); !strings.HasPrefix(got, "Call init_domain(") {
		t.Fatalf("setup instruction = %q", got)
	}
}

// Every tool named in the initialize instructions must exist, so the summary
// cannot drift from the registered surface.
func TestServerInstructionsNameRegisteredTools(t *testing.T) {
	if len(ServerInstructions) > 1600 || regexp.MustCompile(`[^\x00-\x7F]`).MatchString(ServerInstructions) {
		t.Fatalf("instructions must stay short ASCII: %d bytes", len(ServerInstructions))
	}
	_, deps := setupToolsTest(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	RegisterTools(server, deps)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, tool := range listed.Tools {
		registered[tool.Name] = true
	}
	for _, name := range regexp.MustCompile(`\b[a-z]+(?:_[a-z]+)+\b`).FindAllString(ServerInstructions, -1) {
		if strings.HasPrefix(name, "get_") || strings.HasPrefix(name, "record_") || strings.HasPrefix(name, "start_") || strings.HasPrefix(name, "prepare_") || strings.HasPrefix(name, "submit_") || name == "init_domain" {
			if !registered[name] {
				t.Errorf("instructions name unregistered tool %s", name)
			}
		}
	}
}
