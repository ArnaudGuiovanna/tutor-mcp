package tools

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
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
	args := map[string]any{"training_id": "personal:" + domain.ID, "conversation_id": "compact-schema", "expected_selection": 0, "idempotency_key": "compact-schema-select"}
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
	for _, name := range []string{"edit_training", "share_training"} {
		schema := advertisedInputSchema(t, registerTrainingTools, deps, name)
		if len(schemaProperty(t, schema, "document")) == 0 || len(schemaProperty(t, schema, "idempotency_key")) == 0 {
			t.Fatalf("%s lost its edit or retry contract", name)
		}
		if _, ok := schemaProperty(t, schema, "idempotency_key")["minLength"]; !ok {
			t.Fatalf("%s no longer requires a nonempty retry key", name)
		}
	}
}

func trainingTestDomain(t *testing.T, deps *Deps, key string) string {
	t.Helper()
	created := callTool(t, deps, RegisterTools, "L_owner", "init_domain", map[string]any{
		"name": "Go " + key, "concepts": []string{"variables", "loops"}, "prerequisites": map[string][]string{"loops": {"variables"}},
		"idempotency_key": key,
	})
	var domain struct {
		ID string `json:"domain_id"`
	}
	if created.IsError || json.Unmarshal([]byte(resultText(created)), &domain) != nil || domain.ID == "" {
		t.Fatalf("created domain: %s", resultText(created))
	}
	return "personal:" + domain.ID
}

func TestTrainingEditDocumentSchemaIsCompactAndOptional(t *testing.T) {
	_, deps := setupToolsTest(t)
	for _, name := range []string{"edit_training", "share_training"} {
		schema := advertisedInputSchema(t, registerTrainingTools, deps, name)
		document := schemaProperty(t, schema, "document")
		raw, err := json.Marshal(document)
		if err != nil || len(raw) > 4096 {
			t.Fatalf("%s document schema is not compact: bytes=%d err=%v", name, len(raw), err)
		}
		for _, computed := range []string{"can_edit", "progress", "author_id", "calibration", "proposal", "submission", "provenance", "created_at"} {
			if bytes.Contains(raw, []byte(`"`+computed+`"`)) {
				t.Fatalf("%s advertises server-computed field %s", name, computed)
			}
		}
		if _, required := document["required"]; required {
			t.Fatalf("%s document requires fields: %v", name, document["required"])
		}
		if additional, ok := document["additionalProperties"]; ok {
			t.Fatalf("%s document rejects unknown members: %v", name, additional)
		}
	}
}

// A document read from get_training, computed fields included, must still be
// accepted unchanged, and so must the minimal advertised subset.
func TestTrainingEditAcceptsFullAndMinimalDocuments(t *testing.T) {
	_, deps := setupToolsTest(t)
	id := trainingTestDomain(t, deps, "document-roundtrip")
	read := callTool(t, deps, RegisterTools, "L_owner", "get_training", map[string]any{"training_id": id})
	if read.IsError {
		t.Fatal(resultText(read))
	}
	var full map[string]any
	if err := json.Unmarshal([]byte(resultText(read)), &full); err != nil {
		t.Fatal(err)
	}
	training, _ := full["training"].(map[string]any)
	revision, _ := training["revision"].(string)
	if _, ok := training["can_edit"]; !ok || revision == "" {
		t.Fatalf("get_training no longer returns computed fields: %v", training)
	}
	for name, document := range map[string]any{"full": full, "minimal": map[string]any{"curriculum": full["curriculum"]}} {
		preview := callTool(t, deps, RegisterTools, "L_owner", "edit_training", map[string]any{"action": "preview", "training_id": id, "expected_revision": revision, "document": document})
		if preview.IsError {
			t.Fatalf("%s document rejected: %s", name, resultText(preview))
		}
	}
}

func TestTrainingErrorsNameTheCorrectableReason(t *testing.T) {
	_, deps := setupToolsTest(t)
	id := trainingTestDomain(t, deps, "training-errors")
	read := callTool(t, deps, RegisterTools, "L_owner", "get_training", map[string]any{"training_id": id})
	var doc struct {
		Training struct {
			Revision string `json:"revision"`
		} `json:"training"`
	}
	if read.IsError || json.Unmarshal([]byte(resultText(read)), &doc) != nil {
		t.Fatal(resultText(read))
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"edit_training", map[string]any{"action": "preview", "training_id": id, "expected_revision": doc.Training.Revision}, "training action rejected: document is required"},
		{"get_training", map[string]any{"training_id": "personal:missing"}, "training action rejected: training not found or not available to this account; search with list_training_library"},
		{"manage_training_lifecycle", map[string]any{"action": "withdraw", "training_id": id, "preview": true}, "training action rejected: this account is not permitted"},
		{"manage_training_lifecycle", map[string]any{"action": "archive", "training_id": id, "expected_revision": doc.Training.Revision, "confirm": true}, "idempotency_key is required"},
	} {
		result := callTool(t, deps, RegisterTools, "L_owner", tc.tool, tc.args)
		if !result.IsError || !strings.HasPrefix(resultText(result), tc.want) {
			t.Fatalf("%s: got error=%v %q, want prefix %q", tc.tool, result.IsError, resultText(result), tc.want)
		}
	}
}

func TestTrainingEnumsAdvertiseStoreVocabulary(t *testing.T) {
	_, deps := setupToolsTest(t)
	for tool, field := range map[string]string{"list_training_library": "filter", "share_training": "audience", "decide_training": "decision", "edit_training": "action", "manage_training_lifecycle": "action"} {
		values, _ := schemaProperty(t, advertisedInputSchema(t, registerTrainingTools, deps, tool), field)["enum"].([]any)
		if len(values) == 0 {
			t.Fatalf("%s.%s advertises no enum", tool, field)
		}
	}
	filter := schemaProperty(t, advertisedInputSchema(t, registerTrainingTools, deps, "list_training_library"), "filter")["enum"].([]any)
	if !slices.Contains(filter, any("")) || !slices.Contains(filter, any("archived")) {
		t.Fatalf("filter enum dropped an accepted value: %v", filter)
	}
}
