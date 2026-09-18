// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"encoding/json"
	"testing"
)

// An if-then commitment is an object, and saying so in the description was not
// enough: two recorded sessions show the caller sending a plain sentence and
// the attempt dying in schema validation before the handler ran. The sentence
// is now accepted and split into the two clauses, the same way criteria_scores
// accepts both of the shapes the rubric layer allows.
func TestImplementationIntentionAcceptsSentence(t *testing.T) {
	for name, tc := range map[string]struct {
		raw             string
		trigger, action string
	}{
		"objet":                  {`{"trigger":"tomorrow evening","action":"build a small Go CLI"}`, "tomorrow evening", "build a small Go CLI"},
		"phrase avec virgule":    {`"Tomorrow evening, 30 minutes on error wrapping"`, "Tomorrow evening", "30 minutes on error wrapping"},
		"phrase avec deux-pts":   {`"Tomorrow evening: build a small Go CLI"`, "Tomorrow evening", "build a small Go CLI"},
		"phrase sans separateur": {`"Tomorrow evening"`, "Tomorrow evening", ""},
	} {
		t.Run(name, func(t *testing.T) {
			var intention ImplementationIntentionInput
			if err := json.Unmarshal([]byte(tc.raw), &intention); err != nil {
				t.Fatalf("rejected %s: %v", tc.raw, err)
			}
			if intention.Trigger != tc.trigger || intention.Action != tc.action {
				t.Fatalf("got trigger=%q action=%q, want trigger=%q action=%q",
					intention.Trigger, intention.Action, tc.trigger, tc.action)
			}
		})
	}
}

// A sentence has to clear schema validation too, otherwise the SDK rejects the
// call before UnmarshalJSON is ever reached.
func TestRecordSessionCloseSchemaAcceptsIntentionSentence(t *testing.T) {
	_, deps := setupToolsTest(t)
	schema := advertisedInputSchema(t, registerRecordSessionClose, deps, "record_session_close")

	property := schemaProperty(t, schema, "implementation_intention")
	branches, ok := property["anyOf"].([]any)
	if !ok {
		t.Fatalf("implementation_intention advertises no anyOf, so a sentence cannot validate: %v", property)
	}
	for _, branch := range branches {
		if shape, ok := branch.(map[string]any); ok && shape["type"] == "string" {
			return
		}
	}
	t.Fatalf("no string branch among the advertised shapes: %v", branches)
}
