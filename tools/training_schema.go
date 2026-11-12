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

// trainingInputDescriptions documents the TrainingRequest fields, whose model
// struct carries no schema descriptions. trainingDecisionDescriptions
// specializes the one field whose meaning depends on the tool.
var trainingInputDescriptions = map[string]string{
	"id":                "Training ID from list_training_library: personal:<domain_id> or formation:<id>.",
	"query":             "Case-insensitive name search (max 200 bytes).",
	"filter":            "Library view; empty lists everything authorized.",
	"after":             "Pagination cursor: next_after from the previous page.",
	"limit":             "Page size, 1-100 (default 20).",
	"expected_revision": "Current revision from get_training; a stale value is rejected as revision_conflict.",
	"name":              "Name of the shared copy (max 500 bytes).",
	"audience":          "open (all institution members), invitation or approval. share_training defaults to invitation.",
	"member_id":         "Membership ID from find_training_members.",
	"reason":            "Human-readable reason recorded in the audit trail.",
	"free_publication":  "Owner only: let authorized editors publish without review.",
	"personal_learning": "Allow personal learning in this institution.",
	"confirm":           "Must be true to apply a lifecycle change.",
	"document":          "Complete proposed programme. Personal trainings: send curriculum (from get_training). Institutional trainings: send modules and concepts. Server-computed fields are ignored and may be omitted.",
}

var trainingDecisionDescriptions = map[string]string{
	"decide_training":            "approve publishes atomically; changes_requested returns the draft to its author.",
	"set_training_access":        "With member_id: invite (default), revoke, edit (grant editing) or remove_editor.",
	"preview_training_lifecycle": "Lifecycle action to preview.",
}

// The store reads only these parts of a submitted document: the description
// of a new institutional programme, the curriculum of a personal training,
// and the modules/concepts of an institutional one (db.validateTrainingEdit,
// db.shareTraining). It replaces the training projection, proposal,
// submission, calibration and every curriculum envelope field (version,
// provenance, review, timestamps) with server values.
type trainingDocumentInput struct {
	Training   *trainingProjectionInput      `json:"training,omitempty" jsonschema:"Only description is read, when an owner creates an institutional programme with share_training."`
	Curriculum *trainingCurriculumInput      `json:"curriculum,omitempty" jsonschema:"Personal trainings: the complete next curriculum. Keep every concept id and key; retire concepts instead of removing them."`
	Modules    []models.FormationModuleInput `json:"modules,omitempty" jsonschema:"Institutional trainings: 1-100 modules with unique stable_key."`
	Concepts   []trainingConceptInput        `json:"concepts,omitempty" jsonschema:"Institutional trainings: 1-500 concepts, each in an existing module_stable_key."`
}

type trainingProjectionInput struct {
	Description string `json:"description,omitempty" jsonschema:"Programme description."`
}

type trainingCurriculumInput struct {
	Graph    models.KnowledgeSpace       `json:"graph" jsonschema:"Active concept keys and prerequisites (key -> prerequisite keys)."`
	Concepts []trainingCurriculumConcept `json:"concepts" jsonschema:"Active and retired concepts; at most 500."`
}

type trainingCurriculumConcept struct {
	ID          string                       `json:"id" jsonschema:"Stable concept ID; never change it."`
	Key         string                       `json:"key" jsonschema:"Immutable graph key."`
	Label       string                       `json:"label" jsonschema:"Learner-facing label (max 500 bytes)."`
	Description string                       `json:"description,omitempty"`
	Level       string                       `json:"level"`
	Outcomes    []models.CurriculumOutcome   `json:"outcomes,omitempty"`
	Criteria    []models.CurriculumCriterion `json:"criteria,omitempty"`
	Status      string                       `json:"status" jsonschema:"Changing description, level, outcomes, criteria or status invalidates that concept's prior evidence."`
	ReplacedBy  []string                     `json:"replaced_by,omitempty"`
}

type trainingConceptInput struct {
	StableKey       string                       `json:"stable_key" jsonschema:"Unique concept key."`
	ModuleStableKey string                       `json:"module_stable_key" jsonschema:"stable_key of the owning module."`
	Label           string                       `json:"label"`
	Position        int                          `json:"position,omitempty"`
	Description     string                       `json:"description,omitempty"`
	Level           string                       `json:"level,omitempty"`
	Outcomes        []models.CurriculumOutcome   `json:"outcomes,omitempty"`
	Criteria        []models.CurriculumCriterion `json:"criteria,omitempty"`
	Prerequisites   []string                     `json:"prerequisites,omitempty" jsonschema:"stable_key values of prerequisite concepts."`
	MetadataJSON    string                       `json:"metadata_json,omitempty" jsonschema:"Optional JSON object."`
}

var trainingDocumentSchema = buildTrainingDocumentSchema()

func buildTrainingDocumentSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[trainingDocumentInput](&jsonschema.ForOptions{})
	if err != nil {
		panic("cannot infer training document schema: " + err.Error())
	}
	levels := enumValues(string(models.CurriculumLevelUnspecified), string(models.CurriculumLevelFoundation), string(models.CurriculumLevelIntermediate), string(models.CurriculumLevelAdvanced))
	schemaPropertyAt(schema, "curriculum.concepts[].level").Enum = levels
	// Formation concepts normalize an empty level to unspecified.
	schemaPropertyAt(schema, "concepts[].level").Enum = append([]any{""}, levels...)
	schemaPropertyAt(schema, "curriculum.concepts[].status").Enum = enumValues(string(models.CurriculumConceptActive), string(models.CurriculumConceptRetired))
	// Documents read back from get_training carry server-computed fields
	// (can_edit, progress, revision, provenance...). Existing clients send
	// them back unchanged, so the advertised subset must not reject them.
	openSchema(schema)
	return schema
}

// openSchema removes additionalProperties:false from every object so
// undocumented members remain accepted (and ignored by the store).
func openSchema(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Not != nil {
		schema.AdditionalProperties = nil
	}
	openSchema(schema.AdditionalProperties)
	openSchema(schema.Items)
	for _, property := range schema.Properties {
		openSchema(property)
	}
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
		if field == "document" {
			document := *trainingDocumentSchema
			document.Type, document.Types = "", []string{"null", "object"}
			document.Description = trainingInputDescriptions[field]
			property = &document
		}
		if property.Description == "" {
			property.Description = trainingInputDescriptions[field]
		}
		if field == "decision" {
			property.Description = trainingDecisionDescriptions[name]
		}
		properties[field] = property
	}
	schema.Properties = properties
	return schema
}
