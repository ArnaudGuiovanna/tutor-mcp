// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"tutor-mcp/models"
)

// trainingInputDescriptions documents the trainingToolInput fields.
// trainingFieldDescriptionOverrides specializes fields whose meaning depends
// on the tool.
var trainingInputDescriptions = map[string]string{
	"training_id":        "Training ID from list_training_library: personal:<domain_id> or formation:<id>.",
	"query":              "Case-insensitive name search (max 200 bytes).",
	"filter":             "Library view; empty lists everything authorized.",
	"after":              "Pagination cursor: next_after from the previous page.",
	"limit":              "Page size, 1-100 (default 20).",
	"conversation_id":    "The conversation identifier provided by the native client. Bound transports supply it automatically.",
	"expected_revision":  "Current revision from get_training; a stale value is rejected as revision_conflict.",
	"expected_selection": "Current selection_version returned by get_conversation_context. Read it again after init_domain because creation already selects the training.",
	"idempotency_key":    "Stable idempotency key (1..128 letters, digits, '.', '_', ':', '-'). Reuse only for an identical retry; choose a fresh key for a new action.",
	"name":               "Name of the shared copy (max 500 bytes).",
	"audience":           "open (all institution members), invitation or approval. share_training defaults to invitation.",
	"member_id":          "Membership ID from find_training_members.",
	"reason":             "Human-readable reason recorded in the audit trail.",
	"free_publication":   "Owner only: let authorized editors publish without review.",
	"personal_learning":  "Allow personal learning in this institution.",
	"confirm":            "Must be true to apply a lifecycle change; not needed with preview=true.",
	"document":           "Complete proposed programme. Personal trainings: send curriculum (from get_training). Institutional trainings: send modules and concepts. Server-computed fields are ignored and may be omitted.",
	"preview":            "true reports the effects of the action without changing anything (no confirm or idempotency_key needed).",
}

var trainingFieldDescriptionOverrides = map[string]map[string]string{
	"decide_training":           {"decision": "approve publishes atomically; changes_requested returns the draft to its author."},
	"set_training_access":       {"decision": "With member_id: invite (default), revoke, edit (grant editing) or remove_editor."},
	"edit_training":             {"action": "preview validates without changing anything; propose stores a proposal for human review; save applies the reviewed document."},
	"manage_training_lifecycle": {"action": "Lifecycle action to preview or apply."},
	"find_training_members":     {"query": "Member display name or public key to search (max 200 bytes)."},
	"get_training_groups":       {"training_id": "Formation training ID (formation:<id>) whose assigned cohorts to read."},
}

// The store reads only these parts of a submitted document: the description
// of a new institutional programme, the curriculum of a personal training,
// and the modules/concepts of an institutional one (db.validateTrainingEdit,
// db.shareTraining). It replaces the training projection, proposal,
// submission, calibration and every curriculum envelope field (version,
// provenance, review, timestamps) with server values.
type trainingDocumentInput struct {
	Training   *trainingProjectionInput `json:"training,omitempty" jsonschema:"Only description is read, when an owner creates an institutional programme with share_training."`
	Curriculum *trainingCurriculumInput `json:"curriculum,omitempty" jsonschema:"Personal trainings: the complete next curriculum. Keep every concept id and key; retire concepts instead of removing them."`
	Modules    []trainingModuleInput    `json:"modules,omitempty" jsonschema:"Institutional trainings: 1-100 modules with unique stable_key."`
	Concepts   []trainingConceptInput   `json:"concepts,omitempty" jsonschema:"Institutional trainings: 1-500 concepts, each in an existing module_stable_key."`
}

type trainingProjectionInput struct {
	Description string `json:"description,omitempty" jsonschema:"Programme description."`
}

type trainingCurriculumInput struct {
	Graph    trainingGraphInput          `json:"graph" jsonschema:"Active concept keys and prerequisites."`
	Concepts []trainingCurriculumConcept `json:"concepts" jsonschema:"Active and retired concepts; at most 500."`
}

type trainingGraphInput struct {
	Concepts      []string            `json:"concepts" jsonschema:"Active concept keys."`
	Prerequisites map[string][]string `json:"prerequisites" jsonschema:"Concept key -> prerequisite concept keys."`
}

type trainingCurriculumConcept struct {
	ID          string                       `json:"id" jsonschema:"Stable concept ID; never change it."`
	Key         string                       `json:"key" jsonschema:"Immutable graph key."`
	Label       string                       `json:"label" jsonschema:"Learner-facing label (max 500 bytes)."`
	Description string                       `json:"description,omitempty" jsonschema:"Competency description."`
	Level       string                       `json:"level" jsonschema:"Curriculum level."`
	Outcomes    []models.CurriculumOutcome   `json:"outcomes,omitempty" jsonschema:"Observable learning outcomes."`
	Criteria    []models.CurriculumCriterion `json:"criteria,omitempty" jsonschema:"Assessment criteria."`
	Status      string                       `json:"status" jsonschema:"Changing description, level, outcomes, criteria or status invalidates that concept's prior evidence."`
	ReplacedBy  []string                     `json:"replaced_by,omitempty" jsonschema:"For a retired concept: IDs of the concepts that replace it."`
}

type trainingModuleInput struct {
	StableKey string         `json:"stable_key" jsonschema:"Unique module key."`
	Title     string         `json:"title" jsonschema:"Module title."`
	Position  int            `json:"position,omitempty" jsonschema:"Display order."`
	Metadata  map[string]any `json:"metadata,omitempty" jsonschema:"Optional free-form module metadata object."`
}

type trainingConceptInput struct {
	StableKey       string                       `json:"stable_key" jsonschema:"Unique concept key."`
	ModuleStableKey string                       `json:"module_stable_key" jsonschema:"stable_key of the owning module."`
	Label           string                       `json:"label" jsonschema:"Learner-facing label."`
	Position        int                          `json:"position,omitempty" jsonschema:"Display order inside the module."`
	Description     string                       `json:"description,omitempty" jsonschema:"Competency description."`
	Level           string                       `json:"level,omitempty" jsonschema:"Curriculum level."`
	Outcomes        []models.CurriculumOutcome   `json:"outcomes,omitempty" jsonschema:"Observable learning outcomes."`
	Criteria        []models.CurriculumCriterion `json:"criteria,omitempty" jsonschema:"Assessment criteria."`
	Prerequisites   []string                     `json:"prerequisites,omitempty" jsonschema:"stable_key values of prerequisite concepts."`
	Metadata        map[string]any               `json:"metadata,omitempty" jsonschema:"Optional free-form concept metadata object."`
}

// trainingDocumentDefs are shared by both concept shapes of a document. They
// are emitted once under $defs at the root of each tool schema that carries a
// document and referenced with $ref; the SDK serializes and validates them.
var trainingDocumentSchema, trainingDocumentDefs = buildTrainingDocumentSchema()

func buildTrainingDocumentSchema() (*jsonschema.Schema, map[string]*jsonschema.Schema) {
	schema, err := jsonschema.For[trainingDocumentInput](&jsonschema.ForOptions{})
	if err != nil {
		panic("cannot infer training document schema: " + err.Error())
	}
	levels := enumValues(string(models.CurriculumLevelUnspecified), string(models.CurriculumLevelFoundation), string(models.CurriculumLevelIntermediate), string(models.CurriculumLevelAdvanced))
	schemaPropertyAt(schema, "curriculum.concepts[].level").Enum = levels
	// Formation concepts normalize an empty level to unspecified.
	schemaPropertyAt(schema, "concepts[].level").Enum = append([]any{""}, levels...)
	schemaPropertyAt(schema, "curriculum.concepts[].status").Enum = enumValues(string(models.CurriculumConceptActive), string(models.CurriculumConceptRetired))
	describeSchema(schema)
	defs := map[string]*jsonschema.Schema{
		"outcome":   schemaPropertyAt(schema, "curriculum.concepts[].outcomes").Items,
		"criterion": schemaPropertyAt(schema, "curriculum.concepts[].criteria").Items,
	}
	for _, concepts := range []string{"curriculum.concepts[]", "concepts[]"} {
		schemaPropertyAt(schema, concepts+".outcomes").Items = &jsonschema.Schema{Ref: "#/$defs/outcome"}
		schemaPropertyAt(schema, concepts+".criteria").Items = &jsonschema.Schema{Ref: "#/$defs/criterion"}
	}
	// Documents read back from get_training carry server-computed fields
	// (can_edit, progress, revision, provenance...). Clients send them back
	// unchanged, so the advertised subset must not reject them.
	openSchema(schema)
	for _, def := range defs {
		openSchema(def)
	}
	return schema, defs
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
	spec, ok := trainingTools[name]
	if !ok {
		panic("missing training input contract for " + name)
	}
	inferred, err := jsonschema.For[trainingToolInput](&jsonschema.ForOptions{})
	if err != nil {
		panic("cannot infer training input schema: " + err.Error())
	}
	schema := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}, AdditionalProperties: inferred.AdditionalProperties}
	for _, field := range strings.Fields(spec.fields) {
		property := inferred.Properties[field]
		if property == nil {
			panic("unknown training input " + name + "." + field)
		}
		switch field {
		case "document":
			document := *trainingDocumentSchema
			document.Description = trainingInputDescriptions[field]
			property = &document
			schema.Defs = trainingDocumentDefs
		case "idempotency_key":
			minimum, maximum := 1, 128
			property.MinLength, property.MaxLength = &minimum, &maximum
			if !spec.read && !spec.actionSelected {
				schema.Required = append(schema.Required, field)
			}
		case "action":
			schema.Required = append(schema.Required, field)
		}
		property.Description = trainingInputDescriptions[field]
		if override, ok := trainingFieldDescriptionOverrides[name][field]; ok {
			property.Description = override
		}
		schema.Properties[field] = property
	}
	if name == "select_conversation_training" {
		schema.Required = append(schema.Required, "training_id", "expected_selection")
	}
	return schema
}
