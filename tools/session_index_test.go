package tools

import (
	"encoding/base64"
	"testing"

	"tutor-mcp/db"
	"tutor-mcp/memory"
	"tutor-mcp/models"
)

func TestSessionIndexDegradationIsVisibleThroughMCPAndRecovers(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	persistence, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, persistence, "L_owner", "indexed-history")
	keyring, err := db.NewIntegrationSecretKeyring("test:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "test")
	if err != nil {
		t.Fatal(err)
	}
	persistence.SetIntegrationSecretKeyring(keyring)
	memory.ConfigureNarrativeStore(persistence)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := persistence.CompareAndSwapNarrative(t.Context(), memory.NarrativeKey{LearnerID: "L_owner", Scope: memory.ScopeSession, Key: "2026-10-14T00-00-00Z"}, 0, "---\ndomain_id: "+domain.ID+"\n---\nrestored context", "", "", limits); err != nil {
		t.Fatal(err)
	}
	if _, err := persistence.RawDB().Exec(`UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	// RegisterTools installs the real tenant transaction around both tools.
	result := callTool(t, deps, RegisterTools, "L_owner", "get_next_activity", map[string]any{"domain_id": domain.ID})
	if result.IsError {
		t.Fatalf("activity failed instead of degrading: %s", resultText(result))
	}
	assertDegradedComponent(t, decodeResult(t, result), "session_narrative_index")
	state := callTool(t, deps, RegisterTools, "L_owner", "get_memory_state", map[string]any{})
	if state.IsError {
		t.Fatalf("memory state failed: %s", resultText(state))
	}
	assertDegradedComponent(t, decodeResult(t, state), "narrative_context")
	scope := models.LegacyPrincipal("L_owner").TenantScope()
	if page, _, err := persistence.BackfillSessionNarrativeIndex(t.Context(), scope, "", 100); err != nil || page.Indexed != 1 {
		t.Fatalf("repair=%+v,%v", page, err)
	}
	result = callTool(t, deps, RegisterTools, "L_owner", "get_next_activity", map[string]any{"domain_id": domain.ID})
	if result.IsError {
		t.Fatal(resultText(result))
	}
	output := decodeResult(t, result)
	contextView, ok := output["episodic_context"].(map[string]any)
	if !ok {
		t.Fatalf("repaired context missing: %v", output)
	}
	if sessions, ok := contextView["recent_sessions"].([]any); !ok || len(sessions) != 1 {
		t.Fatalf("repaired sessions=%v", contextView)
	}
}
