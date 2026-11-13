// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolInputStringForms names inputs declared as an object that also accept a
// plain sentence, decoded by the type's own UnmarshalJSON. The schema has to
// advertise both shapes, because the SDK validates arguments against it before
// any decoding happens.
var toolInputStringForms = map[string][]string{
	"record_session_close": {"implementation_intention"},
}

// toolInputSchemaOverrides replaces the inferred schema of structured object
// inputs carried as json.RawMessage, so the advertised contract is the object
// the handler parses instead of an opaque value.
var toolInputSchemaOverrides = map[string]map[string]func() *jsonschema.Schema{
	"prepare_assessment_attempt": {
		"rubric": func() *jsonschema.Schema {
			schema := rubricInputSchema(prepareAssessmentRubricDescription, true)
			schema.Examples = []any{prepareAssessmentRubricExample}
			return schema
		},
	},
	"record_interaction": {
		"rubric": func() *jsonschema.Schema {
			return rubricInputSchema("Optional rubric of an observation without attempt_id, shaped like the prepare_assessment_attempt rubric (passing_score optional); never sent with attempt_id, whose rubric is frozen. Scoring fields belong to rubric_score.", false)
		},
		"rubric_score": func() *jsonschema.Schema {
			return rubricScoreInputSchema("Rubric scoring result, required with attempt_id: one criteria_scores entry per frozen criterion with its exact id, a numeric score and concrete evidence from the learner's actual answer.")
		},
		"semantic_observation": func() *jsonschema.Schema {
			return freeObjectSchema("Optional semantic observation (for example reasoning_quality, success_mode, confidence_alignment).")
		},
		"evaluation_provenance": func() *jsonschema.Schema {
			return freeObjectSchema("Optional evaluator model/version/trace identifiers; only with attempt_id.")
		},
	},
}

// constrainInputSchema infers the schema the SDK would have inferred anyway
// (or takes the hand-built one) and completes it: object inputs, enums,
// alternate string shapes and a description on every property.
func constrainInputSchema[In any](tool *mcp.Tool) {
	var schema *jsonschema.Schema
	switch explicit := tool.InputSchema.(type) {
	case nil:
		inferred, err := jsonschema.For[In](&jsonschema.ForOptions{})
		if err != nil {
			panic("cannot infer the input schema of MCP tool " + tool.Name + ": " + err.Error())
		}
		schema = inferred
	case *jsonschema.Schema:
		schema = explicit
	default:
		return
	}
	for field, build := range toolInputSchemaOverrides[tool.Name] {
		if schema.Properties[field] != nil {
			schema.Properties[field] = build()
		}
	}
	for field, values := range toolInputEnums[tool.Name] {
		// A probe registered under a real tool name with another input type
		// has nothing to constrain; TestHandlerValidatedInputsAdvertiseEnums
		// checks that every real tool advertises its enums.
		if property := schemaPropertyAt(schema, field); property != nil {
			property.Enum = values
		}
	}
	for _, field := range toolInputStringForms[tool.Name] {
		property, ok := schema.Properties[field]
		if !ok {
			panic("MCP tool " + tool.Name + " has no " + field + " input to constrain")
		}
		object := *property
		object.Description = ""
		schema.Properties[field] = &jsonschema.Schema{
			Description: property.Description,
			AnyOf:       []*jsonschema.Schema{&object, {Type: "string"}},
		}
	}
	describeSchema(schema)
	tool.InputSchema = schema
}

// nestedPropertyDescriptions documents members of shared models whose Go types
// carry no schema description. Keys are "<parent>.<name>" (an array parent is
// written "<parent>[]") or a bare "<name>" fallback.
var nestedPropertyDescriptions = map[string]string{
	"outcomes[].id":             "Stable outcome ID.",
	"outcomes[].statement":      "What the learner can do.",
	"outcomes[].observable":     "Observable evidence of the outcome.",
	"criteria[].id":             "Stable criterion ID.",
	"criteria[].description":    "What acceptable evidence looks like.",
	"criteria[].evidence":       "Example of acceptable evidence.",
	"details{}.label":           "Learner-facing label.",
	"updates[].scope":           "Exact scope of the claimed snapshot.",
	"updates[].key":             "Exact key of the claimed snapshot.",
	"updates[].domain_id":       "Exact domain_id of the claimed snapshot.",
	"updates[].content":         "Complete final markdown content for this snapshot.",
	"brief.version":             "Brief schema version (default 1).",
	"brief.kind":                "Message kind; defaults to the queued kind.",
	"brief.domain_id":           "Domain the message is about.",
	"brief.domain_name":         "Learner-facing domain name.",
	"brief.concept":             "Concept the message is about.",
	"brief.trigger":             "Signal that justifies the message.",
	"brief.pedagogical_intent":  "Pedagogical purpose of the message.",
	"brief.learning_gain":       "Concrete gain the learner gets by coming back.",
	"brief.why_now":             "Why this message is sent now.",
	"brief.evidence":            "Factual observations supporting the message.",
	"brief.goal_link":           "Link to the learner's personal goal.",
	"brief.open_loop":           "Unfinished learning thread to reopen.",
	"brief.next_action":         "Single concrete next step.",
	"brief.estimated_minutes":   "Expected duration of the next step in minutes.",
	"brief.language":            "BCP 47 language of the message.",
	"brief.tone":                "Requested tone.",
	"preferred_windows[].day":   "Weekday of the window (0 = Sunday ... 6 = Saturday).",
	"preferred_windows[].start": "Local start time, HH:MM.",
	"preferred_windows[].end":   "Local end time, HH:MM.",
	"accessibility_preferences.screen_reader_optimized": "Prefer output that works well with screen readers.",
	"accessibility_preferences.avoid_emojis":            "Avoid emojis in learner-facing text.",
	"accessibility_preferences.prefer_plain_language":   "Prefer plain language.",
	"accessibility_preferences.extra_processing_time":   "Give extra time before expecting an answer.",
	"description": "Description.",
	"level":       "Curriculum level.",
	"label":       "Learner-facing label.",
	"outcomes":    "Observable learning outcomes.",
	"criteria":    "Assessment criteria.",
}

// describeSchema fills every property that has no description, recursively,
// from nestedPropertyDescriptions. Existing descriptions are never changed,
// so concurrent registrations of shared schemas only read.
func describeSchema(schema *jsonschema.Schema) {
	describeSchemaAt(schema, "")
}

func describeSchemaAt(schema *jsonschema.Schema, parent string) {
	if schema == nil {
		return
	}
	for name, property := range schema.Properties {
		if property == nil {
			continue
		}
		if property.Description == "" && property.Ref == "" {
			if description, ok := nestedPropertyDescriptions[parent+"."+name]; ok && parent != "" {
				property.Description = description
			} else if description, ok := nestedPropertyDescriptions[name]; ok {
				property.Description = description
			}
		}
		describeSchemaAt(property, name)
	}
	if schema.Items != nil {
		describeSchemaAt(schema.Items, strings.TrimSuffix(parent, "[]")+"[]")
	}
	if schema.AdditionalProperties != nil {
		describeSchemaAt(schema.AdditionalProperties, parent+"{}")
	}
	for _, alternative := range schema.AnyOf {
		describeSchemaAt(alternative, parent)
	}
	for name, def := range schema.Defs {
		describeSchemaAt(def, name)
	}
}
