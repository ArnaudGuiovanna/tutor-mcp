// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
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
)

// advertisedInputSchema returns the input schema a client actually receives for
// one tool. A jsonschema struct tag only ever becomes a property description, so
// this is the single place a caller can read the contract from.
func advertisedInputSchema(t *testing.T, register func(*mcp.Server, *Deps), deps *Deps, name string) map[string]any {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	register(server, deps)

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
	for _, tool := range res.Tools {
		if tool.Name != name {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %q input schema: %v", name, err)
		}
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode %q input schema: %v", name, err)
		}
		return schema
	}
	t.Fatalf("tool %q is not advertised", name)
	return nil
}

func schemaProperty(t *testing.T, schema map[string]any, name string) map[string]any {
	t.Helper()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("input schema advertises no properties")
	}
	property, ok := properties[name].(map[string]any)
	if !ok {
		t.Fatalf("input schema advertises no %q property", name)
	}
	return property
}

// RecordLearningEvent accepts exactly two kinds and rejects every other value.
// The description already named both, and a recorded session still shows a
// caller sending an invented "diagnostic_feedback" four times in a row: prose
// that never reaches the schema does not constrain anyone, so the accepted
// values have to be enumerated in the schema the client validates against.
func TestRecordLearningEventSchemaEnumeratesAcceptedKinds(t *testing.T) {
	_, deps := setupToolsTest(t)
	schema := advertisedInputSchema(t, registerRecordLearningEvent, deps, "record_learning_event")

	enum, ok := schemaProperty(t, schema, "kind")["enum"].([]any)
	if !ok {
		t.Fatal("kind is advertised as a free-form string, so any invented value clears validation")
	}
	got := make([]string, 0, len(enum))
	for _, value := range enum {
		got = append(got, fmt.Sprint(value))
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"feedback", "instruction"}) {
		t.Fatalf("advertised kinds %v are not the two the store accepts", got)
	}
}

// implementation_intention is an object with required trigger and action
// clauses. Its description read as free prose, and a recorded session shows a
// caller sending a sentence three times before guessing the object form, so the
// description has to name the fields the schema requires.
func TestRecordSessionCloseSchemaStatesIntentionShape(t *testing.T) {
	_, deps := setupToolsTest(t)
	schema := advertisedInputSchema(t, registerRecordSessionClose, deps, "record_session_close")

	property := schemaProperty(t, schema, "implementation_intention")
	description, _ := property["description"].(string)
	for _, field := range []string{"trigger", "action"} {
		if !strings.Contains(description, field) {
			t.Fatalf("intention description %q never names the required %q field, so a caller reads it as prose", description, field)
		}
	}
}

// A description that calls an input optional while the schema lists it as
// required contradicts itself, and a caller believes the description: a
// recorded session shows record_interaction rejected for a missing "notes"
// that its own description called optional. This guards every tool at once,
// because the two statements are written in different places.
func TestNoRequiredInputIsDocumentedOptional(t *testing.T) {
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
	for _, tool := range res.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %q input schema: %v", tool.Name, err)
		}
		var schema struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode %q input schema: %v", tool.Name, err)
		}
		for _, field := range schema.Required {
			if strings.Contains(strings.ToLower(schema.Properties[field].Description), "optional") {
				t.Errorf("%s.%s is required by the schema but its description calls it optional: %q",
					tool.Name, field, schema.Properties[field].Description)
			}
		}
	}
}
