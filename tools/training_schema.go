// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"tutor-mcp/models"
)

// TrainingRequest is the shared internal/HTTP envelope. Advertising its entire
// programme document on every MCP operation makes simple reads look like edits
// and repeats the same large schema throughout the model's tool catalogue.
// Keep the handler's types, while advertising only the inputs it consumes.
var trainingInputFields = map[string]string{
	"list_training_library":        "query filter after limit",
	"get_training_groups":          "id after limit",
	"get_training":                 "id",
	"get_conversation_context":     "conversation_id",
	"select_conversation_training": "id conversation_id expected_selection key",
	"preview_training_edit":        "id expected_revision document",
	"propose_training_edit":        "id expected_revision document key",
	"save_training_edit":           "id expected_revision document key",
	"share_training":               "id expected_revision name audience document key",
	"submit_training":              "id expected_revision reason key",
	"decide_training":              "id expected_revision decision reason key",
	"publish_training":             "id expected_revision reason key",
	"set_training_access":          "id expected_revision audience member_id decision free_publication key",
	"get_training_policy":          "",
	"set_training_policy":          "personal_learning key",
	"find_training_members":        "id query",
	"join_training":                "id key",
	"preview_training_lifecycle":   "id decision",
	"archive_training":             "id expected_revision confirm reason key",
	"restore_training":             "id expected_revision confirm reason key",
	"delete_training":              "id expected_revision confirm reason key",
	"withdraw_training":            "id expected_revision confirm reason key",
}

func trainingInputSchema(name string) *jsonschema.Schema {
	fields, ok := trainingInputFields[name]
	if !ok {
		panic("missing training input contract for " + name)
	}
	schema, err := jsonschema.For[models.TrainingRequest](&jsonschema.ForOptions{})
	if err != nil {
		panic("cannot infer training input schema: " + err.Error())
	}
	properties := make(map[string]*jsonschema.Schema)
	for _, field := range strings.Fields(fields) {
		property := schema.Properties[field]
		if property == nil {
			panic("unknown training input " + name + "." + field)
		}
		properties[field] = property
	}
	schema.Properties = properties
	return schema
}
