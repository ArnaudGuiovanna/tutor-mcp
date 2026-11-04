package tools

import (
	"encoding/json"
	"testing"
)

func TestTrainingReadSchemasExcludeProgrammeEditingPayload(t *testing.T) {
	_, deps := setupToolsTest(t)
	for _, name := range []string{"get_conversation_context", "get_training", "list_training_library", "get_training_policy", "get_training_groups", "find_training_members"} {
		t.Run(name, func(t *testing.T) {
			schema := advertisedInputSchema(t, registerTrainingTools, deps, name)
			properties, _ := schema["properties"].(map[string]any)
			for _, field := range []string{"document", "free_publication", "confirm", "personal_learning"} {
				if _, ok := properties[field]; ok {
					t.Fatalf("read %s advertises unrelated mutation field %s", name, field)
				}
			}
			raw, err := json.Marshal(schema)
			if err != nil || len(raw) > 1024 {
				t.Fatalf("simple read schema is not bounded: bytes=%d err=%v", len(raw), err)
			}
		})
	}
}

func TestTrainingSelectionSchemaAndRetryKeepConfirmedTraining(t *testing.T) {
	_, deps := setupToolsTest(t)
	created := callTool(t, deps, RegisterTools, "L_owner", "init_domain", map[string]any{
		"name": "Go", "concepts": []string{"variables"}, "prerequisites": map[string][]string{},
		"idempotency_key": "compact-schema-create",
	})
	if created.IsError {
		t.Fatal(resultText(created))
	}
	var domain struct {
		ID string `json:"domain_id"`
	}
	if err := json.Unmarshal([]byte(resultText(created)), &domain); err != nil || domain.ID == "" {
		t.Fatalf("created domain: %s %v", resultText(created), err)
	}
	args := map[string]any{"id": "personal:" + domain.ID, "conversation_id": "compact-schema", "expected_selection": 0, "key": "compact-schema-select"}
	for range 2 {
		selected := callTool(t, deps, RegisterTools, "L_owner", "select_conversation_training", args)
		if selected.IsError {
			t.Fatal(resultText(selected))
		}
	}
	read := callTool(t, deps, RegisterTools, "L_owner", "get_conversation_context", map[string]any{"conversation_id": "compact-schema"})
	var got struct {
		DomainID string `json:"domain_id"`
		Version  int    `json:"selection_version"`
	}
	if read.IsError || json.Unmarshal([]byte(resultText(read)), &got) != nil || got.DomainID != domain.ID || got.Version != 1 {
		t.Fatalf("selection was lost or retried as a new mutation: %s", resultText(read))
	}
}

func TestTrainingEditSchemaRetainsProgrammeAndMutationKey(t *testing.T) {
	_, deps := setupToolsTest(t)
	for _, name := range []string{"propose_training_edit", "save_training_edit", "share_training"} {
		schema := advertisedInputSchema(t, registerTrainingTools, deps, name)
		if len(schemaProperty(t, schema, "document")) == 0 || len(schemaProperty(t, schema, "key")) == 0 {
			t.Fatalf("%s lost its edit or retry contract", name)
		}
		if _, ok := schemaProperty(t, schema, "key")["minLength"]; !ok {
			t.Fatalf("%s no longer requires a nonempty retry key", name)
		}
	}
}
